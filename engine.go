package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	errorTypeTimeout        = "timeout"
	errorTypeNetwork        = "network_error"
	errorTypeStatusMismatch = "unexpected_status"
	errorTypeMissingVar     = "missing_variable"
	errorTypeCapture        = "capture_error"
)

var placeholderPattern = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}`)

type endpointAggregate struct {
	Total     int64
	Success   int64
	Latencies []time.Duration
}

type workerResult struct {
	totalRequests int64
	successCount  int64
	latencies     []time.Duration

	endpointStats map[string]*endpointAggregate
	statusCounts  map[int]int64
	errorCounts   map[string]int64
}

type executionPlan struct {
	mode              string
	tasks             []compiledTask
	totalWeight       int
	cumulativeWeights []int
	thinkTime         ThinkTimeConfig
	thresholds        ThresholdsConfig
	feeder            *feederDataset
}

type runtimeStage struct {
	duration          time.Duration
	targetConcurrency int
	spawnRate         float64
}

type loadProfile struct {
	totalDuration    time.Duration
	defaultSpawnRate float64

	peakConcurrency int
	rampUp          time.Duration
	steady          time.Duration
	rampDown        time.Duration

	stages []runtimeStage
}

type requestOutcome struct {
	EndpointKey string
	Latency     time.Duration
	StatusCode  int
	Success     bool
	ErrorType   string
	Captures    map[string]string
}

func runLoadTest(cfg TestConfig) (TestResult, error) {
	if err := validateLoadTestConfig(&cfg); err != nil {
		return TestResult{}, err
	}

	execPlan, err := buildExecutionPlan(cfg)
	if err != nil {
		return TestResult{}, err
	}
	profile, err := buildLoadProfile(cfg)
	if err != nil {
		return TestResult{}, err
	}
	if profile.totalDuration <= 0 {
		return TestResult{}, errors.New("invalid load profile duration")
	}

	startTime := time.Now()
	results := make(chan workerResult, profile.maxConcurrency())
	var wg sync.WaitGroup

	activeWorkers := make(map[int]chan struct{}, profile.maxConcurrency())
	activeOrder := make([]int, 0, profile.maxConcurrency())
	nextWorkerID := 0
	spawnBudget := 0.0
	tickInterval := 100 * time.Millisecond

	spawnWorker := func() {
		stop := make(chan struct{})
		id := nextWorkerID
		nextWorkerID++

		activeWorkers[id] = stop
		activeOrder = append(activeOrder, id)

		wg.Add(1)
		go runVirtualUser(id, stop, execPlan, results, &wg)
	}

	stopWorkers := func(count int) {
		for i := 0; i < count && len(activeOrder) > 0; i++ {
			lastIndex := len(activeOrder) - 1
			id := activeOrder[lastIndex]
			activeOrder = activeOrder[:lastIndex]
			close(activeWorkers[id])
			delete(activeWorkers, id)
		}
	}

	reconcile := func(desired int, spawnRate float64) {
		if desired < 0 {
			desired = 0
		}

		current := len(activeOrder)
		if desired == current {
			return
		}
		if desired < current {
			spawnBudget = 0
			stopWorkers(current - desired)
			return
		}

		toAdd := desired - current
		effectiveSpawnRate := spawnRate
		if effectiveSpawnRate <= 0 {
			effectiveSpawnRate = math.Inf(1)
		}

		if math.IsInf(effectiveSpawnRate, 1) {
			spawnBudget = float64(toAdd)
		} else {
			spawnBudget += effectiveSpawnRate * tickInterval.Seconds()
		}

		canSpawn := int(math.Floor(spawnBudget))
		if canSpawn > toAdd {
			canSpawn = toAdd
		}
		for i := 0; i < canSpawn; i++ {
			spawnWorker()
			spawnBudget--
		}
	}

	reconcile(profile.targetAt(0))
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		<-ticker.C
		elapsed := time.Since(startTime)
		if elapsed >= profile.totalDuration {
			break
		}
		reconcile(profile.targetAt(elapsed))
	}

	stopWorkers(len(activeOrder))
	wg.Wait()
	close(results)

	endpointStats := make(map[string]*endpointAggregate)
	statusCounts := make(map[int]int64)
	errorCounts := make(map[string]int64)

	var totalRequests int64
	var successCount int64
	latencies := make([]time.Duration, 0, 2048)

	for result := range results {
		totalRequests += result.totalRequests
		successCount += result.successCount
		latencies = append(latencies, result.latencies...)
		mergeEndpointAggregates(endpointStats, result.endpointStats)
		mergeIntCounters(statusCounts, result.statusCounts)
		mergeStringCounters(errorCounts, result.errorCounts)
	}

	latSummary := calculateLatencySummary(latencies)

	endTime := time.Now()
	durationSeconds := endTime.Sub(startTime).Seconds()
	rps := 0.0
	if durationSeconds > 0 {
		rps = float64(totalRequests) / durationSeconds
	}

	successRate := 0.0
	if totalRequests > 0 {
		successRate = (float64(successCount) / float64(totalRequests)) * 100
	}
	errorRate := 100 - successRate
	if totalRequests == 0 {
		errorRate = 0
	}

	endpointMetrics := buildEndpointMetrics(endpointStats)
	statusMetrics := buildStatusMetrics(statusCounts, totalRequests)
	errorMetrics := buildErrorMetrics(errorCounts, totalRequests)

	thresholds := evaluateThresholds(execPlan.thresholds, latSummary.P95Ms, errorRate, rps)
	passed := thresholds.Passed
	if !thresholds.Enabled {
		passed = true
	}

	return TestResult{
		SuccessRate:       successRate,
		ErrorRate:         errorRate,
		AverageLatency:    latSummary.AverageMs,
		P50Latency:        latSummary.P50Ms,
		P90Latency:        latSummary.P90Ms,
		P95Latency:        latSummary.P95Ms,
		P99Latency:        latSummary.P99Ms,
		TotalRequests:     int(totalRequests),
		RequestsPerSecond: rps,
		StartTime:         startTime,
		EndTime:           endTime,
		EndpointMetrics:   endpointMetrics,
		StatusMetrics:     statusMetrics,
		ErrorMetrics:      errorMetrics,
		Thresholds:        thresholds,
		Passed:            passed,
	}, nil
}

func runVirtualUser(id int, stop <-chan struct{}, plan executionPlan, out chan<- workerResult, wg *sync.WaitGroup) {
	defer wg.Done()

	client := newFastHTTPClient()
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	seed := time.Now().UnixNano() + int64(id*7919)
	rng := rand.New(rand.NewSource(seed))

	result := workerResult{
		latencies:     make([]time.Duration, 0, 256),
		endpointStats: make(map[string]*endpointAggregate),
		statusCounts:  make(map[int]int64),
		errorCounts:   make(map[string]int64),
	}
	sessionVars := map[string]string{
		"vu.id": strconv.Itoa(id),
	}
	iteration := 0

	for {
		if shouldStop(stop) {
			out <- result
			return
		}

		iteration++
		iterationVars, hasData := plan.nextIterationVars(iteration)
		if !hasData {
			out <- result
			return
		}
		vars := mergeVars(sessionVars, iterationVars)
		vars["iteration"] = strconv.Itoa(iteration)

		if plan.mode == scenarioModeSequential {
			for _, task := range plan.tasks {
				if shouldStop(stop) {
					out <- result
					return
				}

				outcome := executeTaskWithVars(client, req, resp, task, vars)
				recordOutcome(&result, outcome)
				if len(outcome.Captures) > 0 {
					for key, value := range outcome.Captures {
						sessionVars[key] = value
						vars[key] = value
					}
				}

				if !pauseThinkTime(plan.thinkTime, rng, stop) {
					out <- result
					return
				}
			}
			continue
		}

		task := chooseWeightedTask(plan, rng)
		outcome := executeTaskWithVars(client, req, resp, task, vars)
		recordOutcome(&result, outcome)
		if len(outcome.Captures) > 0 {
			for key, value := range outcome.Captures {
				sessionVars[key] = value
			}
		}

		if !pauseThinkTime(plan.thinkTime, rng, stop) {
			out <- result
			return
		}
	}
}

func executeTaskWithVars(client *fasthttp.Client, req *fasthttp.Request, resp *fasthttp.Response, task compiledTask, vars map[string]string) requestOutcome {
	resolved, missingVars := applyTaskTemplates(task, vars)
	if len(missingVars) > 0 {
		return requestOutcome{
			EndpointKey: task.EndpointKey,
			Success:     false,
			ErrorType:   errorTypeMissingVar,
		}
	}

	buildTaskRequest(req, resolved)
	start := time.Now()
	err := client.DoTimeout(req, resp, requestTimeout)
	latency := time.Since(start)

	outcome := requestOutcome{
		EndpointKey: task.EndpointKey,
		Latency:     latency,
	}

	if err != nil {
		outcome.Success = false
		outcome.ErrorType = classifyRequestError(err)
		resp.Reset()
		return outcome
	}

	statusCode := resp.StatusCode()
	outcome.StatusCode = statusCode
	outcome.Success = statusCode == resolved.ExpectedStatusCode
	if !outcome.Success {
		outcome.ErrorType = errorTypeStatusMismatch
	}

	if len(task.Capture) > 0 {
		captures, captureErr := captureFromResponse(resp.Body(), task.Capture)
		if captureErr != nil {
			outcome.Success = false
			outcome.ErrorType = errorTypeCapture
		} else {
			outcome.Captures = captures
		}
	}

	resp.Reset()
	return outcome
}

func classifyRequestError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return errorTypeTimeout
	}
	return errorTypeNetwork
}

func recordOutcome(result *workerResult, outcome requestOutcome) {
	result.totalRequests++
	if outcome.Success {
		result.successCount++
	}
	if outcome.Latency > 0 {
		result.latencies = append(result.latencies, outcome.Latency)
	}

	endpoint := strings.TrimSpace(outcome.EndpointKey)
	if endpoint == "" {
		endpoint = "unknown"
	}
	stats := result.endpointStats[endpoint]
	if stats == nil {
		stats = &endpointAggregate{
			Latencies: make([]time.Duration, 0, 64),
		}
		result.endpointStats[endpoint] = stats
	}
	stats.Total++
	if outcome.Success {
		stats.Success++
	}
	if outcome.Latency > 0 {
		stats.Latencies = append(stats.Latencies, outcome.Latency)
	}

	if outcome.StatusCode > 0 {
		result.statusCounts[outcome.StatusCode]++
	}
	if strings.TrimSpace(outcome.ErrorType) != "" {
		result.errorCounts[outcome.ErrorType]++
	}
}

func chooseWeightedTask(plan executionPlan, rng *rand.Rand) compiledTask {
	if len(plan.tasks) == 1 {
		return plan.tasks[0]
	}

	roll := rng.Intn(plan.totalWeight) + 1
	idx := sort.SearchInts(plan.cumulativeWeights, roll)
	if idx < 0 || idx >= len(plan.tasks) {
		return plan.tasks[len(plan.tasks)-1]
	}
	return plan.tasks[idx]
}

func shouldStop(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func pauseThinkTime(cfg ThinkTimeConfig, rng *rand.Rand, stop <-chan struct{}) bool {
	delay := sampleThinkTime(cfg, rng)
	return sleepWithStop(delay, stop)
}

func sleepWithStop(delay time.Duration, stop <-chan struct{}) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-stop:
		return false
	}
}

func sampleThinkTime(cfg ThinkTimeConfig, rng *rand.Rand) time.Duration {
	switch cfg.Mode {
	case thinkTimeModeFixed:
		return time.Duration(cfg.FixedMs) * time.Millisecond
	case thinkTimeModeRandom:
		if cfg.MaxMs <= cfg.MinMs {
			return time.Duration(cfg.MinMs) * time.Millisecond
		}
		n := rng.Intn(cfg.MaxMs-cfg.MinMs+1) + cfg.MinMs
		return time.Duration(n) * time.Millisecond
	case thinkTimeModeDistribution:
		switch cfg.Distribution {
		case thinkDistUniform:
			if cfg.MaxMs <= cfg.MinMs {
				return time.Duration(cfg.MinMs) * time.Millisecond
			}
			n := rng.Intn(cfg.MaxMs-cfg.MinMs+1) + cfg.MinMs
			return time.Duration(n) * time.Millisecond
		case thinkDistNormal:
			sample := rng.NormFloat64()*cfg.StdDevMs + cfg.MeanMs
			if sample < 0 {
				sample = 0
			}
			return time.Duration(sample * float64(time.Millisecond))
		case thinkDistExponential:
			sample := rng.ExpFloat64() * cfg.MeanMs
			if sample < 0 {
				sample = 0
			}
			return time.Duration(sample * float64(time.Millisecond))
		default:
			return 0
		}
	default:
		return 0
	}
}

func buildExecutionPlan(cfg TestConfig) (executionPlan, error) {
	think, err := normalizeThinkTime(cfg.ThinkTime)
	if err != nil {
		return executionPlan{}, err
	}
	thresholds, err := normalizeThresholds(cfg.Thresholds)
	if err != nil {
		return executionPlan{}, err
	}

	tasks, mode, err := compileTasks(cfg)
	if err != nil {
		return executionPlan{}, err
	}

	var feeder *feederDataset
	if cfg.DataFeeder != nil {
		normalized, err := normalizeDataFeeder(cfg.DataFeeder)
		if err != nil {
			return executionPlan{}, err
		}
		fd, err := loadDataFeeder(normalized)
		if err != nil {
			return executionPlan{}, err
		}
		feeder = fd
	}

	plan := executionPlan{
		mode:       mode,
		tasks:      tasks,
		thinkTime:  think,
		thresholds: thresholds,
		feeder:     feeder,
	}

	if mode == scenarioModeWeighted {
		cumulative := make([]int, len(tasks))
		sum := 0
		for i, task := range tasks {
			weight := task.Weight
			if weight <= 0 {
				weight = 1
			}
			sum += weight
			cumulative[i] = sum
		}
		plan.totalWeight = sum
		plan.cumulativeWeights = cumulative
	}

	return plan, nil
}

func (plan executionPlan) nextIterationVars(iteration int) (map[string]string, bool) {
	vars := map[string]string{}
	if plan.feeder == nil {
		return vars, true
	}

	row, ok := plan.feeder.NextRow()
	if !ok {
		return nil, false
	}
	for key, value := range row {
		vars[key] = value
	}
	return vars, true
}

func buildLoadProfile(cfg TestConfig) (loadProfile, error) {
	profile := loadProfile{}
	if len(cfg.Stages) > 0 {
		profile.stages = make([]runtimeStage, 0, len(cfg.Stages))
		total := time.Duration(0)
		peak := 0
		for _, stage := range cfg.Stages {
			d := time.Duration(stage.DurationSeconds) * time.Second
			profile.stages = append(profile.stages, runtimeStage{
				duration:          d,
				targetConcurrency: stage.TargetConcurrency,
				spawnRate:         stage.SpawnRate,
			})
			total += d
			if stage.TargetConcurrency > peak {
				peak = stage.TargetConcurrency
			}
		}
		profile.totalDuration = total
		profile.peakConcurrency = peak
		profile.defaultSpawnRate = cfg.SpawnRate
		if profile.defaultSpawnRate <= 0 {
			profile.defaultSpawnRate = math.Inf(1)
		}
		return profile, nil
	}

	totalDuration := time.Duration(cfg.TestDuration) * time.Second
	rampUp := time.Duration(cfg.RampUpSeconds) * time.Second
	rampDown := time.Duration(cfg.RampDownSeconds) * time.Second
	steady := totalDuration - rampUp - rampDown
	if steady < 0 {
		return loadProfile{}, errors.New("rampUpSeconds + rampDownSeconds must be <= testDurationSeconds")
	}

	spawnRate := cfg.SpawnRate
	if spawnRate <= 0 {
		if rampUp > 0 {
			spawnRate = float64(cfg.Concurrency) / rampUp.Seconds()
		} else {
			spawnRate = math.Inf(1)
		}
	}

	return loadProfile{
		totalDuration:    totalDuration,
		defaultSpawnRate: spawnRate,
		peakConcurrency:  cfg.Concurrency,
		rampUp:           rampUp,
		steady:           steady,
		rampDown:         rampDown,
	}, nil
}

func (p loadProfile) targetAt(elapsed time.Duration) (int, float64) {
	if elapsed < 0 {
		return 0, p.defaultSpawnRate
	}
	if elapsed >= p.totalDuration {
		return 0, p.defaultSpawnRate
	}

	if len(p.stages) > 0 {
		cursor := time.Duration(0)
		for _, stage := range p.stages {
			cursor += stage.duration
			if elapsed < cursor {
				spawnRate := stage.spawnRate
				if spawnRate <= 0 {
					spawnRate = p.defaultSpawnRate
				}
				if spawnRate <= 0 {
					spawnRate = math.Inf(1)
				}
				return stage.targetConcurrency, spawnRate
			}
		}
		return 0, p.defaultSpawnRate
	}

	if p.rampUp > 0 && elapsed < p.rampUp {
		fraction := float64(elapsed) / float64(p.rampUp)
		desired := int(math.Round(float64(p.peakConcurrency) * fraction))
		return desired, p.defaultSpawnRate
	}

	steadyEnd := p.rampUp + p.steady
	if elapsed < steadyEnd {
		return p.peakConcurrency, p.defaultSpawnRate
	}

	if p.rampDown > 0 && elapsed < p.totalDuration {
		downElapsed := elapsed - steadyEnd
		fraction := 1 - (float64(downElapsed) / float64(p.rampDown))
		if fraction < 0 {
			fraction = 0
		}
		desired := int(math.Round(float64(p.peakConcurrency) * fraction))
		return desired, p.defaultSpawnRate
	}

	return 0, p.defaultSpawnRate
}

func (p loadProfile) maxConcurrency() int {
	if p.peakConcurrency > 0 {
		return p.peakConcurrency
	}
	maxValue := 1
	for _, stage := range p.stages {
		if stage.targetConcurrency > maxValue {
			maxValue = stage.targetConcurrency
		}
	}
	return maxValue
}

func mergeEndpointAggregates(dst map[string]*endpointAggregate, src map[string]*endpointAggregate) {
	for key, agg := range src {
		current := dst[key]
		if current == nil {
			current = &endpointAggregate{
				Latencies: make([]time.Duration, 0, len(agg.Latencies)),
			}
			dst[key] = current
		}
		current.Total += agg.Total
		current.Success += agg.Success
		current.Latencies = append(current.Latencies, agg.Latencies...)
	}
}

func mergeIntCounters(dst, src map[int]int64) {
	for key, value := range src {
		dst[key] += value
	}
}

func mergeStringCounters(dst, src map[string]int64) {
	for key, value := range src {
		dst[key] += value
	}
}

type latencySummary struct {
	AverageMs float64
	P50Ms     float64
	P90Ms     float64
	P95Ms     float64
	P99Ms     float64
}

func calculateLatencySummary(latencies []time.Duration) latencySummary {
	if len(latencies) == 0 {
		return latencySummary{}
	}

	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, latency := range sorted {
		total += latency
	}

	return latencySummary{
		AverageMs: durationToMs(total) / float64(len(sorted)),
		P50Ms:     percentileMs(sorted, 0.50),
		P90Ms:     percentileMs(sorted, 0.90),
		P95Ms:     percentileMs(sorted, 0.95),
		P99Ms:     percentileMs(sorted, 0.99),
	}
}

func percentileMs(sorted []time.Duration, percentile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return durationToMs(sorted[index])
}

func durationToMs(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func buildEndpointMetrics(stats map[string]*endpointAggregate) []EndpointMetric {
	keys := make([]string, 0, len(stats))
	for key := range stats {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	metrics := make([]EndpointMetric, 0, len(keys))
	for _, key := range keys {
		agg := stats[key]
		summary := calculateLatencySummary(agg.Latencies)

		successRate := 0.0
		if agg.Total > 0 {
			successRate = (float64(agg.Success) / float64(agg.Total)) * 100
		}

		metrics = append(metrics, EndpointMetric{
			Endpoint:         key,
			TotalRequests:    int(agg.Total),
			SuccessRate:      successRate,
			AverageLatencyMs: summary.AverageMs,
			P50LatencyMs:     summary.P50Ms,
			P90LatencyMs:     summary.P90Ms,
			P95LatencyMs:     summary.P95Ms,
			P99LatencyMs:     summary.P99Ms,
		})
	}
	return metrics
}

func buildStatusMetrics(statusCounts map[int]int64, totalRequests int64) []StatusMetric {
	codes := make([]int, 0, len(statusCounts))
	for code := range statusCounts {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	metrics := make([]StatusMetric, 0, len(codes))
	for _, code := range codes {
		count := statusCounts[code]
		rate := 0.0
		if totalRequests > 0 {
			rate = (float64(count) / float64(totalRequests)) * 100
		}
		metrics = append(metrics, StatusMetric{
			StatusCode: code,
			Count:      int(count),
			Rate:       rate,
		})
	}
	return metrics
}

func buildErrorMetrics(errorCounts map[string]int64, totalRequests int64) []ErrorMetric {
	keys := make([]string, 0, len(errorCounts))
	for key := range errorCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	metrics := make([]ErrorMetric, 0, len(keys))
	for _, key := range keys {
		count := errorCounts[key]
		rate := 0.0
		if totalRequests > 0 {
			rate = (float64(count) / float64(totalRequests)) * 100
		}
		metrics = append(metrics, ErrorMetric{
			Type:  key,
			Count: int(count),
			Rate:  rate,
		})
	}
	return metrics
}

func evaluateThresholds(cfg ThresholdsConfig, p95 float64, errorRate float64, rps float64) ThresholdEvaluation {
	checks := make([]ThresholdCheckResult, 0, 3)
	enabled := cfg.MaxP95LatencyMs != nil || cfg.MaxErrorRate != nil || cfg.MinRPS != nil
	passed := true

	if cfg.MaxP95LatencyMs != nil {
		ok := p95 <= *cfg.MaxP95LatencyMs
		checks = append(checks, ThresholdCheckResult{
			Name:     "p95_latency_ms",
			Actual:   p95,
			Expected: fmt.Sprintf("<= %.4f", *cfg.MaxP95LatencyMs),
			Passed:   ok,
		})
		if !ok {
			passed = false
		}
	}

	if cfg.MaxErrorRate != nil {
		ok := errorRate <= *cfg.MaxErrorRate
		checks = append(checks, ThresholdCheckResult{
			Name:     "error_rate",
			Actual:   errorRate,
			Expected: fmt.Sprintf("<= %.4f", *cfg.MaxErrorRate),
			Passed:   ok,
		})
		if !ok {
			passed = false
		}
	}

	if cfg.MinRPS != nil {
		ok := rps >= *cfg.MinRPS
		checks = append(checks, ThresholdCheckResult{
			Name:     "requests_per_second",
			Actual:   rps,
			Expected: fmt.Sprintf(">= %.4f", *cfg.MinRPS),
			Passed:   ok,
		})
		if !ok {
			passed = false
		}
	}

	return ThresholdEvaluation{
		Enabled: enabled,
		Passed:  !enabled || passed,
		Checks:  checks,
	}
}

func applyTaskTemplates(task compiledTask, vars map[string]string) (compiledTask, []string) {
	out := task

	var missing []string
	renderedURL, missingURL := renderTemplateString(task.URL, vars)
	renderedBody, missingBody := renderTemplateString(task.RequestBody, vars)
	out.URL = renderedURL
	out.RequestBody = renderedBody
	missing = append(missing, missingURL...)
	missing = append(missing, missingBody...)

	renderedHeaders := make(map[string]string, len(task.Headers))
	for key, value := range task.Headers {
		rendered, missingHeader := renderTemplateString(value, vars)
		renderedHeaders[key] = rendered
		missing = append(missing, missingHeader...)
	}
	out.Headers = renderedHeaders

	return out, uniqueStrings(missing)
}

func renderTemplateString(input string, vars map[string]string) (string, []string) {
	if strings.TrimSpace(input) == "" {
		return input, nil
	}

	missing := make([]string, 0)
	rendered := placeholderPattern.ReplaceAllStringFunc(input, func(raw string) string {
		matches := placeholderPattern.FindStringSubmatch(raw)
		if len(matches) != 2 {
			return raw
		}
		key := strings.TrimSpace(matches[1])
		if value, ok := vars[key]; ok {
			return value
		}
		missing = append(missing, key)
		return raw
	})
	return rendered, missing
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func mergeVars(base map[string]string, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		merged[key] = value
	}
	return merged
}

func captureFromResponse(body []byte, captureMap map[string]string) (map[string]string, error) {
	var decoded interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}

	captured := make(map[string]string, len(captureMap))
	for variable, path := range captureMap {
		value, ok := extractByPath(decoded, path)
		if !ok {
			return nil, fmt.Errorf("path %q not found", path)
		}
		captured[variable] = valueToString(value)
	}
	return captured, nil
}

func extractByPath(root interface{}, path string) (interface{}, bool) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")
	if path == "" {
		return root, true
	}

	segments := strings.Split(path, ".")
	current := root
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		for {
			bracket := strings.Index(segment, "[")
			if bracket == -1 {
				break
			}

			field := strings.TrimSpace(segment[:bracket])
			if field != "" {
				obj, ok := current.(map[string]interface{})
				if !ok {
					return nil, false
				}
				next, ok := obj[field]
				if !ok {
					return nil, false
				}
				current = next
			}

			end := strings.Index(segment[bracket:], "]")
			if end == -1 {
				return nil, false
			}
			end += bracket

			indexText := strings.TrimSpace(segment[bracket+1 : end])
			idx, err := strconv.Atoi(indexText)
			if err != nil || idx < 0 {
				return nil, false
			}

			arr, ok := current.([]interface{})
			if !ok || idx >= len(arr) {
				return nil, false
			}
			current = arr[idx]

			if end+1 >= len(segment) {
				segment = ""
			} else {
				segment = segment[end+1:]
			}
		}

		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, ok := obj[segment]
		if !ok {
			return nil, false
		}
		current = next
	}

	return current, true
}

func valueToString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return ""
	default:
		bytes, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(bytes)
	}
}
