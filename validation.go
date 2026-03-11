package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/valyala/fasthttp"
)

type compiledTask struct {
	Name               string
	EndpointKey        string
	Method             string
	URL                string
	ExpectedStatusCode int
	RequestBody        string
	Headers            map[string]string
	Weight             int
	Capture            map[string]string
}

func validateLoadTestConfig(cfg *TestConfig) error {
	normalizeConfig(cfg)

	if err := validateLoadShapeConfig(cfg); err != nil {
		return err
	}

	if cfg.Scenario != nil && len(cfg.Scenario.Tasks) > 0 {
		if err := validateScenarioConfig(cfg); err != nil {
			return err
		}
	} else {
		if cfg.ExpectedStatusCode == 0 {
			cfg.ExpectedStatusCode = http.StatusOK
		}
		if err := validateTask(compiledTask{
			Method:             cfg.Method,
			URL:                cfg.URL,
			ExpectedStatusCode: cfg.ExpectedStatusCode,
			RequestBody:        cfg.RequestBody,
			Headers:            cfg.Headers,
		}); err != nil {
			return err
		}
	}

	if _, err := normalizeThinkTime(cfg.ThinkTime); err != nil {
		return err
	}
	if _, err := normalizeThresholds(cfg.Thresholds); err != nil {
		return err
	}
	if _, err := normalizeDataFeeder(cfg.DataFeeder); err != nil {
		return err
	}

	return nil
}

func validateLoadShapeConfig(cfg *TestConfig) error {
	if cfg.SpawnRate < minSpawnRate {
		return errors.New("spawnRate must be >= 0")
	}
	if cfg.RampUpSeconds < 0 {
		return errors.New("rampUpSeconds must be >= 0")
	}
	if cfg.RampDownSeconds < 0 {
		return errors.New("rampDownSeconds must be >= 0")
	}

	if len(cfg.Stages) > 0 {
		total := 0
		maxTarget := 0
		for i := range cfg.Stages {
			stage := &cfg.Stages[i]
			if stage.DurationSeconds < minDurationSeconds {
				return fmt.Errorf("stages[%d].durationSeconds must be >= %d", i, minDurationSeconds)
			}
			if stage.TargetConcurrency < 0 || stage.TargetConcurrency > maxConcurrency {
				return fmt.Errorf("stages[%d].targetConcurrency must be between 0 and %d", i, maxConcurrency)
			}
			if stage.SpawnRate < minSpawnRate {
				return fmt.Errorf("stages[%d].spawnRate must be >= 0", i)
			}
			total += stage.DurationSeconds
			if stage.TargetConcurrency > maxTarget {
				maxTarget = stage.TargetConcurrency
			}
		}
		if maxTarget <= 0 {
			return errors.New("at least one stage must have targetConcurrency > 0")
		}
		if cfg.Concurrency == 0 {
			cfg.Concurrency = maxTarget
		}
		if cfg.Concurrency < minConcurrency || cfg.Concurrency > maxConcurrency {
			return fmt.Errorf("concurrency must be between %d and %d", minConcurrency, maxConcurrency)
		}
		if cfg.TestDuration <= 0 {
			cfg.TestDuration = total
		}
		return nil
	}

	if cfg.Concurrency < minConcurrency || cfg.Concurrency > maxConcurrency {
		return fmt.Errorf("concurrency must be between %d and %d", minConcurrency, maxConcurrency)
	}
	if cfg.TestDuration < minDurationSeconds || cfg.TestDuration > maxDurationSeconds {
		return fmt.Errorf("testDurationSeconds must be between %d and %d", minDurationSeconds, maxDurationSeconds)
	}
	if cfg.RampUpSeconds+cfg.RampDownSeconds > cfg.TestDuration {
		return errors.New("rampUpSeconds + rampDownSeconds must be <= testDurationSeconds")
	}

	return nil
}

func validateScenarioConfig(cfg *TestConfig) error {
	if cfg.Scenario == nil || len(cfg.Scenario.Tasks) == 0 {
		return errors.New("scenario.tasks must not be empty")
	}

	mode := normalizeScenarioMode(cfg.Scenario.Mode, cfg.Scenario.Tasks)
	cfg.Scenario.Mode = mode
	totalWeight := 0

	for i := range cfg.Scenario.Tasks {
		task := &cfg.Scenario.Tasks[i]
		task.Name = strings.TrimSpace(task.Name)
		task.Method = strings.ToUpper(strings.TrimSpace(firstNonEmpty(task.Method, cfg.Method)))
		task.URL = strings.TrimSpace(firstNonEmpty(task.URL, cfg.URL))
		task.Headers = mergeHeaders(cfg.Headers, task.Headers)
		task.Capture = trimCaptureMap(task.Capture)

		if task.ExpectedStatusCode == 0 {
			task.ExpectedStatusCode = cfg.ExpectedStatusCode
		}
		if task.ExpectedStatusCode == 0 {
			task.ExpectedStatusCode = http.StatusOK
		}

		taskBody := firstNonEmpty(task.RequestBody, cfg.RequestBody)
		if err := validateTask(compiledTask{
			Name:               task.Name,
			Method:             task.Method,
			URL:                task.URL,
			ExpectedStatusCode: task.ExpectedStatusCode,
			RequestBody:        taskBody,
			Headers:            task.Headers,
			Weight:             task.Weight,
			Capture:            task.Capture,
		}); err != nil {
			return fmt.Errorf("scenario task %d: %w", i+1, err)
		}

		if mode == scenarioModeWeighted {
			if task.Weight <= 0 {
				task.Weight = 1
			}
			totalWeight += task.Weight
		} else if task.Weight < 0 {
			return fmt.Errorf("scenario task %d: weight must be >= 0", i+1)
		}
	}

	if mode == scenarioModeWeighted && totalWeight <= 0 {
		return errors.New("scenario weights must be > 0 in weighted mode")
	}

	return nil
}

func normalizeThinkTime(raw *ThinkTimeConfig) (ThinkTimeConfig, error) {
	if raw == nil {
		return ThinkTimeConfig{Mode: thinkTimeModeNone}, nil
	}

	cfg := *raw
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = thinkTimeModeNone
	}

	switch cfg.Mode {
	case thinkTimeModeNone:
		return cfg, nil
	case thinkTimeModeFixed:
		if cfg.FixedMs < 0 {
			return ThinkTimeConfig{}, errors.New("thinkTime.fixedMs must be >= 0")
		}
		return cfg, nil
	case thinkTimeModeRandom:
		if cfg.MinMs < 0 {
			return ThinkTimeConfig{}, errors.New("thinkTime.minMs must be >= 0")
		}
		if cfg.MaxMs < cfg.MinMs {
			return ThinkTimeConfig{}, errors.New("thinkTime.maxMs must be >= thinkTime.minMs")
		}
		return cfg, nil
	case thinkTimeModeDistribution:
		cfg.Distribution = strings.ToLower(strings.TrimSpace(cfg.Distribution))
		if cfg.Distribution == "" {
			cfg.Distribution = thinkDistUniform
		}
		switch cfg.Distribution {
		case thinkDistUniform:
			if cfg.MinMs < 0 {
				return ThinkTimeConfig{}, errors.New("thinkTime.minMs must be >= 0")
			}
			if cfg.MaxMs < cfg.MinMs {
				return ThinkTimeConfig{}, errors.New("thinkTime.maxMs must be >= thinkTime.minMs")
			}
		case thinkDistNormal:
			if cfg.MeanMs < 0 {
				return ThinkTimeConfig{}, errors.New("thinkTime.meanMs must be >= 0")
			}
			if cfg.StdDevMs < 0 {
				return ThinkTimeConfig{}, errors.New("thinkTime.stdDevMs must be >= 0")
			}
		case thinkDistExponential:
			if cfg.MeanMs <= 0 {
				return ThinkTimeConfig{}, errors.New("thinkTime.meanMs must be > 0 for exponential distribution")
			}
		default:
			return ThinkTimeConfig{}, errors.New("thinkTime.distribution must be one of: uniform, normal, exponential")
		}
		return cfg, nil
	default:
		return ThinkTimeConfig{}, errors.New("thinkTime.mode must be one of: none, fixed, random, distribution")
	}
}

func normalizeThresholds(raw *ThresholdsConfig) (ThresholdsConfig, error) {
	if raw == nil {
		return ThresholdsConfig{}, nil
	}

	cfg := *raw
	if cfg.MaxP95LatencyMs != nil && *cfg.MaxP95LatencyMs < 0 {
		return ThresholdsConfig{}, errors.New("thresholds.maxP95LatencyMs must be >= 0")
	}
	if cfg.MaxErrorRate != nil && (*cfg.MaxErrorRate < 0 || *cfg.MaxErrorRate > 100) {
		return ThresholdsConfig{}, errors.New("thresholds.maxErrorRate must be between 0 and 100")
	}
	if cfg.MinRPS != nil && *cfg.MinRPS < 0 {
		return ThresholdsConfig{}, errors.New("thresholds.minRps must be >= 0")
	}

	return cfg, nil
}

func normalizeDataFeeder(raw *DataFeederConfig) (DataFeederConfig, error) {
	if raw == nil {
		return DataFeederConfig{}, nil
	}

	cfg := *raw
	cfg.File = strings.TrimSpace(cfg.File)
	cfg.Format = strings.ToLower(strings.TrimSpace(cfg.Format))
	if cfg.Format == "" {
		cfg.Format = inferDataFeederFormat(cfg.File)
	}
	if cfg.File == "" {
		return DataFeederConfig{}, errors.New("dataFeeder.file is required")
	}
	if cfg.Format != dataFeederFormatCSV && cfg.Format != dataFeederFormatJSON {
		return DataFeederConfig{}, errors.New("dataFeeder.format must be csv or json")
	}
	if _, err := os.Stat(cfg.File); err != nil {
		return DataFeederConfig{}, fmt.Errorf("dataFeeder.file not accessible: %w", err)
	}

	return cfg, nil
}

func resolveValidationTask(cfg TestConfig) (compiledTask, error) {
	normalizeConfig(&cfg)

	if cfg.Scenario != nil && len(cfg.Scenario.Tasks) > 0 {
		first := cfg.Scenario.Tasks[0]
		task := compiledTask{
			Name:        strings.TrimSpace(first.Name),
			Method:      strings.ToUpper(strings.TrimSpace(firstNonEmpty(first.Method, cfg.Method))),
			URL:         strings.TrimSpace(firstNonEmpty(first.URL, cfg.URL)),
			RequestBody: firstNonEmpty(first.RequestBody, cfg.RequestBody),
			Headers:     mergeHeaders(cfg.Headers, first.Headers),
		}
		if task.Name == "" {
			task.Name = "scenario_task_1"
		}
		if err := validateTaskRequest(task.Method, task.URL, task.RequestBody); err != nil {
			return compiledTask{}, err
		}
		return task, nil
	}

	if err := validateRequestConfig(&cfg); err != nil {
		return compiledTask{}, err
	}
	return compiledTask{
		Name:        "default",
		Method:      cfg.Method,
		URL:         cfg.URL,
		RequestBody: cfg.RequestBody,
		Headers:     copyHeaders(cfg.Headers),
	}, nil
}

func compileTasks(cfg TestConfig) ([]compiledTask, string, error) {
	if cfg.Scenario == nil || len(cfg.Scenario.Tasks) == 0 {
		key := strings.TrimSpace(cfg.Method) + " " + strings.TrimSpace(cfg.URL)
		task := compiledTask{
			Name:               "default",
			EndpointKey:        strings.TrimSpace(key),
			Method:             cfg.Method,
			URL:                cfg.URL,
			ExpectedStatusCode: cfg.ExpectedStatusCode,
			RequestBody:        cfg.RequestBody,
			Headers:            copyHeaders(cfg.Headers),
			Weight:             1,
			Capture:            map[string]string{},
		}
		return []compiledTask{task}, scenarioModeWeighted, nil
	}

	mode := normalizeScenarioMode(cfg.Scenario.Mode, cfg.Scenario.Tasks)
	tasks := make([]compiledTask, 0, len(cfg.Scenario.Tasks))
	for i, source := range cfg.Scenario.Tasks {
		method := strings.ToUpper(strings.TrimSpace(firstNonEmpty(source.Method, cfg.Method)))
		targetURL := strings.TrimSpace(firstNonEmpty(source.URL, cfg.URL))
		task := compiledTask{
			Name:        strings.TrimSpace(source.Name),
			Method:      method,
			URL:         targetURL,
			RequestBody: firstNonEmpty(source.RequestBody, cfg.RequestBody),
			Headers:     mergeHeaders(cfg.Headers, source.Headers),
			Weight:      source.Weight,
			Capture:     trimCaptureMap(source.Capture),
		}
		if task.Name == "" {
			task.Name = fmt.Sprintf("task_%d", i+1)
		}

		endpointKey := strings.TrimSpace(method + " " + targetURL)
		if endpointKey == "" {
			endpointKey = task.Name
		}
		task.EndpointKey = endpointKey

		expected := source.ExpectedStatusCode
		if expected == 0 {
			expected = cfg.ExpectedStatusCode
		}
		if expected == 0 {
			expected = http.StatusOK
		}
		task.ExpectedStatusCode = expected

		if err := validateTask(task); err != nil {
			return nil, "", fmt.Errorf("scenario task %d: %w", i+1, err)
		}
		if mode == scenarioModeWeighted && task.Weight <= 0 {
			task.Weight = 1
		}
		tasks = append(tasks, task)
	}

	return tasks, mode, nil
}

func validateRequestConfig(cfg *TestConfig) error {
	normalizeConfig(cfg)
	return validateTaskRequest(cfg.Method, cfg.URL, cfg.RequestBody)
}

func validateTask(task compiledTask) error {
	if err := validateTaskRequest(task.Method, task.URL, task.RequestBody); err != nil {
		return err
	}
	if task.ExpectedStatusCode < minStatusCode || task.ExpectedStatusCode > maxStatusCode {
		return fmt.Errorf("expectedStatusCode must be between %d and %d", minStatusCode, maxStatusCode)
	}
	for key, path := range task.Capture {
		if strings.TrimSpace(key) == "" {
			return errors.New("capture variable name must not be empty")
		}
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("capture path for %q must not be empty", key)
		}
	}
	return nil
}

func validateTaskRequest(method, targetURL, body string) error {
	if !isMethodSupported(method) {
		return fmt.Errorf("unsupported method %q", method)
	}
	if err := validateURL(targetURL); err != nil {
		return err
	}
	if body != "" && !methodAllowsBody(method) {
		return fmt.Errorf("requestBody is not allowed for method %s", method)
	}
	if body != "" && !json.Valid([]byte(body)) {
		return errors.New("requestBody must be valid JSON")
	}
	return nil
}

func validateTestResult(result TestResult) error {
	if result.SuccessRate < 0 || result.SuccessRate > 100 {
		return errors.New("successRate must be between 0 and 100")
	}
	if result.ErrorRate < 0 || result.ErrorRate > 100 {
		return errors.New("errorRate must be between 0 and 100")
	}
	if result.AverageLatency < 0 {
		return errors.New("averageLatencyMs must be non-negative")
	}
	if result.P50Latency < 0 || result.P90Latency < 0 || result.P95Latency < 0 || result.P99Latency < 0 {
		return errors.New("latency percentiles must be non-negative")
	}
	if result.TotalRequests < 0 {
		return errors.New("totalRequests must be non-negative")
	}
	if result.RequestsPerSecond < 0 {
		return errors.New("requestsPerSecond must be non-negative")
	}
	if result.StartTime.IsZero() || result.EndTime.IsZero() {
		return errors.New("startTime and endTime are required")
	}
	if result.EndTime.Before(result.StartTime) {
		return errors.New("endTime must not be before startTime")
	}
	if len(result.EndpointMetrics) == 0 && result.TotalRequests > 0 {
		return errors.New("endpointMetrics must not be empty when requests were executed")
	}
	return nil
}

func normalizeConfig(cfg *TestConfig) {
	cfg.Method = strings.ToUpper(strings.TrimSpace(cfg.Method))
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Headers = trimHeaders(cfg.Headers)

	if cfg.Thresholds != nil {
		normalized, err := normalizeThresholds(cfg.Thresholds)
		if err == nil {
			cfg.Thresholds = &normalized
		}
	}
	if cfg.DataFeeder != nil {
		normalized, err := normalizeDataFeeder(cfg.DataFeeder)
		if err == nil {
			cfg.DataFeeder = &normalized
		}
	}

	if cfg.Scenario == nil {
		return
	}
	cfg.Scenario.Mode = strings.ToLower(strings.TrimSpace(cfg.Scenario.Mode))
	for i := range cfg.Scenario.Tasks {
		task := &cfg.Scenario.Tasks[i]
		task.Name = strings.TrimSpace(task.Name)
		task.Method = strings.ToUpper(strings.TrimSpace(task.Method))
		task.URL = strings.TrimSpace(task.URL)
		task.Headers = trimHeaders(task.Headers)
		task.Capture = trimCaptureMap(task.Capture)
	}
}

func normalizeScenarioMode(mode string, tasks []ScenarioTask) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == scenarioModeSequential || mode == scenarioModeWeighted {
		return mode
	}
	for _, task := range tasks {
		if task.Weight > 0 {
			return scenarioModeWeighted
		}
	}
	return scenarioModeSequential
}

func inferDataFeederFormat(path string) string {
	lower := strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasSuffix(lower, ".csv"):
		return dataFeederFormatCSV
	case strings.HasSuffix(lower, ".json"):
		return dataFeederFormatJSON
	default:
		return ""
	}
}

func validateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("url must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("url must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("url must include host")
	}
	return nil
}

func isMethodSupported(method string) bool {
	switch method {
	case fasthttp.MethodGet, fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodDelete, fasthttp.MethodPatch:
		return true
	default:
		return false
	}
}

func methodAllowsBody(method string) bool {
	switch method {
	case fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodPatch:
		return true
	default:
		return false
	}
}

func trimHeaders(headers map[string]string) map[string]string {
	normalized := make(map[string]string, len(headers))
	for key, value := range headers {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		normalized[trimmedKey] = strings.TrimSpace(value)
	}
	return normalized
}

func trimCaptureMap(capture map[string]string) map[string]string {
	normalized := make(map[string]string, len(capture))
	for key, value := range capture {
		trimmedKey := strings.TrimSpace(key)
		trimmedValue := strings.TrimSpace(value)
		if trimmedKey == "" || trimmedValue == "" {
			continue
		}
		normalized[trimmedKey] = trimmedValue
	}
	return normalized
}

func copyHeaders(headers map[string]string) map[string]string {
	return mergeHeaders(nil, headers)
}

func mergeHeaders(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		merged[trimmedKey] = strings.TrimSpace(value)
	}
	for key, value := range override {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		merged[trimmedKey] = strings.TrimSpace(value)
	}
	return merged
}

func firstNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}
