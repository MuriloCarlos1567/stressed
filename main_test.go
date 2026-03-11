package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func isolateState(t *testing.T) {
	t.Helper()

	oldResultsFile := resultsFile
	oldTimeout := requestTimeout

	historyMu.Lock()
	oldHistory := append([]HistoryEntry(nil), history...)
	history = []HistoryEntry{}
	historyMu.Unlock()

	resultsFile = filepath.Join(t.TempDir(), "results.json")
	requestTimeout = 100 * time.Millisecond

	t.Cleanup(func() {
		resultsFile = oldResultsFile
		requestTimeout = oldTimeout

		historyMu.Lock()
		history = oldHistory
		historyMu.Unlock()
	})
}

func validConfig(url string) TestConfig {
	return TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                url,
		ExpectedStatusCode: http.StatusOK,
		RequestBody:        "",
		Headers:            map[string]string{},
		Concurrency:        4,
		TestDuration:       1,
	}
}

func validResult() TestResult {
	start := time.Now().Add(-1 * time.Second)
	end := time.Now()
	return TestResult{
		SuccessRate:       100,
		ErrorRate:         0,
		AverageLatency:    10.5,
		P50Latency:        8.1,
		P90Latency:        16.0,
		P95Latency:        20.2,
		P99Latency:        28.4,
		TotalRequests:     100,
		RequestsPerSecond: 100,
		StartTime:         start,
		EndTime:           end,
		EndpointMetrics: []EndpointMetric{
			{
				Endpoint:         "GET https://example.com",
				TotalRequests:    100,
				SuccessRate:      100,
				AverageLatencyMs: 10.5,
				P50LatencyMs:     8.1,
				P90LatencyMs:     16.0,
				P95LatencyMs:     20.2,
				P99LatencyMs:     28.4,
			},
		},
		StatusMetrics: []StatusMetric{
			{StatusCode: 200, Count: 100, Rate: 100},
		},
		ErrorMetrics: []ErrorMetric{},
		Thresholds: ThresholdEvaluation{
			Enabled: false,
			Passed:  true,
			Checks:  []ThresholdCheckResult{},
		},
		Passed: true,
	}
}

func TestValidateLoadTestConfig(t *testing.T) {
	cfg := validConfig("https://example.com")
	if err := validateLoadTestConfig(&cfg); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	cases := []struct {
		name string
		cfg  TestConfig
	}{
		{
			name: "invalid method",
			cfg: TestConfig{
				Method:             "TRACE",
				URL:                "https://example.com",
				ExpectedStatusCode: 200,
				Concurrency:        1,
				TestDuration:       1,
			},
		},
		{
			name: "invalid url",
			cfg: TestConfig{
				Method:             "GET",
				URL:                "://invalid",
				ExpectedStatusCode: 200,
				Concurrency:        1,
				TestDuration:       1,
			},
		},
		{
			name: "invalid expected status",
			cfg: TestConfig{
				Method:             "GET",
				URL:                "https://example.com",
				ExpectedStatusCode: 99,
				Concurrency:        1,
				TestDuration:       1,
			},
		},
		{
			name: "invalid concurrency",
			cfg: TestConfig{
				Method:             "GET",
				URL:                "https://example.com",
				ExpectedStatusCode: 200,
				Concurrency:        0,
				TestDuration:       1,
			},
		},
		{
			name: "invalid duration",
			cfg: TestConfig{
				Method:             "GET",
				URL:                "https://example.com",
				ExpectedStatusCode: 200,
				Concurrency:        1,
				TestDuration:       0,
			},
		},
		{
			name: "body not allowed for GET",
			cfg: TestConfig{
				Method:             "GET",
				URL:                "https://example.com",
				ExpectedStatusCode: 200,
				RequestBody:        `{"x":1}`,
				Concurrency:        1,
				TestDuration:       1,
			},
		},
		{
			name: "invalid json body",
			cfg: TestConfig{
				Method:             "POST",
				URL:                "https://example.com",
				ExpectedStatusCode: 200,
				RequestBody:        `{"x":`,
				Concurrency:        1,
				TestDuration:       1,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := validateLoadTestConfig(&tc.cfg); err == nil {
				t.Fatalf("expected validation error for case %q", tc.name)
			}
		})
	}
}

func TestRunLoadTestSuccess(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	cfg := validConfig(server.URL)
	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if result.TotalRequests <= 0 {
		t.Fatalf("expected total requests > 0, got %d", result.TotalRequests)
	}
	if result.SuccessRate < 99 {
		t.Fatalf("expected successRate >= 99, got %.2f", result.SuccessRate)
	}
	if result.RequestsPerSecond <= 0 {
		t.Fatalf("expected requestsPerSecond > 0, got %.2f", result.RequestsPerSecond)
	}
}

func TestRunLoadTestTimeoutDoesNotHang(t *testing.T) {
	isolateState(t)
	requestTimeout = 50 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := validConfig(server.URL)
	cfg.Concurrency = 2
	cfg.TestDuration = 1

	start := time.Now()
	result, err := runLoadTest(cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("test took too long: %s", elapsed)
	}
	if result.TotalRequests <= 0 {
		t.Fatalf("expected at least one request, got %d", result.TotalRequests)
	}
	if result.SuccessRate >= 100 {
		t.Fatalf("expected some failures due to timeout, got successRate %.2f", result.SuccessRate)
	}
}

func TestDoRequestWithRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &fasthttp.Client{
		ReadTimeout:        1 * time.Second,
		WriteTimeout:       1 * time.Second,
		MaxConnWaitTimeout: 1 * time.Second,
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(server.URL + "/redirect")
	req.Header.SetMethod(fasthttp.MethodGet)

	if err := doRequestWithRedirects(client, req, resp, time.Second); err != nil {
		t.Fatalf("expected redirect flow to succeed, got error: %v", err)
	}
	if resp.StatusCode() != http.StatusNoContent {
		t.Fatalf("expected final status %d, got %d", http.StatusNoContent, resp.StatusCode())
	}
}

func TestHistoryPersistenceConcurrentAppend(t *testing.T) {
	isolateState(t)

	const totalEntries = 25
	result := validResult()
	cfg := validConfig("https://example.com")
	cfg.Concurrency = 1
	cfg.TestDuration = 1

	var wg sync.WaitGroup
	errCh := make(chan error, totalEntries)

	for i := 0; i < totalEntries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entry := HistoryEntry{
				ID:        fmt.Sprintf("id-%d", i),
				Config:    cfg,
				Result:    result,
				Timestamp: time.Now(),
			}
			if err := appendHistoryEntry(entry); err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("appendHistoryEntry returned error: %v", err)
	}

	historyMu.RLock()
	inMemoryLen := len(history)
	historyMu.RUnlock()
	if inMemoryLen != totalEntries {
		t.Fatalf("expected %d entries in memory, got %d", totalEntries, inMemoryLen)
	}

	data, err := os.ReadFile(resultsFile)
	if err != nil {
		t.Fatalf("failed reading persisted history: %v", err)
	}

	var persisted []HistoryEntry
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("failed decoding persisted history: %v", err)
	}
	if len(persisted) != totalEntries {
		t.Fatalf("expected %d persisted entries, got %d", totalEntries, len(persisted))
	}
}

func TestDeleteHistoryEntry(t *testing.T) {
	isolateState(t)

	entryA := HistoryEntry{ID: "a", Config: validConfig("https://example.com"), Result: validResult(), Timestamp: time.Now()}
	entryB := HistoryEntry{ID: "b", Config: validConfig("https://example.com"), Result: validResult(), Timestamp: time.Now()}
	if err := appendHistoryEntry(entryA); err != nil {
		t.Fatalf("append entryA failed: %v", err)
	}
	if err := appendHistoryEntry(entryB); err != nil {
		t.Fatalf("append entryB failed: %v", err)
	}

	id := "a"
	if err := deleteHistoryEntry(&id); err != nil {
		t.Fatalf("deleteHistoryEntry by id failed: %v", err)
	}

	historyMu.RLock()
	if len(history) != 1 || history[0].ID != "b" {
		historyMu.RUnlock()
		t.Fatalf("unexpected history after delete by id: %#v", history)
	}
	historyMu.RUnlock()

	if err := deleteHistoryEntry(nil); err != nil {
		t.Fatalf("deleteHistoryEntry clear-all failed: %v", err)
	}

	historyMu.RLock()
	defer historyMu.RUnlock()
	if len(history) != 0 {
		t.Fatalf("expected empty history after clear-all, got %d", len(history))
	}
}

func TestDeleteHistoryEntryNotFound(t *testing.T) {
	isolateState(t)

	missing := "missing-id"
	err := deleteHistoryEntry(&missing)
	if !errors.Is(err, errHistoryIDNotFound) {
		t.Fatalf("expected errHistoryIDNotFound, got %v", err)
	}
}

func TestValidateTestResult(t *testing.T) {
	if err := validateTestResult(validResult()); err != nil {
		t.Fatalf("expected valid result, got error: %v", err)
	}

	invalid := validResult()
	invalid.SuccessRate = 120
	if err := validateTestResult(invalid); err == nil {
		t.Fatal("expected validation error for invalid successRate")
	}

	invalid = validResult()
	invalid.EndTime = invalid.StartTime.Add(-1 * time.Second)
	if err := validateTestResult(invalid); err == nil {
		t.Fatal("expected validation error for endTime before startTime")
	}
}

func TestRunLoadTestSequentialScenario(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	var loginCount int64
	var searchCount int64
	var buyCount int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			atomic.AddInt64(&loginCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/search":
			atomic.AddInt64(&searchCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/buy":
			atomic.AddInt64(&buyCount, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                server.URL + "/search",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        2,
		TestDuration:       1,
		Scenario: &ScenarioConfig{
			Mode: scenarioModeSequential,
			Tasks: []ScenarioTask{
				{
					Name:               "login",
					Method:             fasthttp.MethodPost,
					URL:                server.URL + "/login",
					RequestBody:        `{"user":"u1"}`,
					ExpectedStatusCode: http.StatusOK,
				},
				{
					Name:               "search",
					Method:             fasthttp.MethodGet,
					URL:                server.URL + "/search",
					ExpectedStatusCode: http.StatusOK,
				},
				{
					Name:               "buy",
					Method:             fasthttp.MethodPost,
					URL:                server.URL + "/buy",
					RequestBody:        `{"sku":"123"}`,
					ExpectedStatusCode: http.StatusCreated,
				},
			},
		},
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if result.TotalRequests <= 0 {
		t.Fatalf("expected requests > 0, got %d", result.TotalRequests)
	}
	if atomic.LoadInt64(&loginCount) == 0 || atomic.LoadInt64(&searchCount) == 0 || atomic.LoadInt64(&buyCount) == 0 {
		t.Fatalf("expected all scenario endpoints to be called; login=%d search=%d buy=%d", loginCount, searchCount, buyCount)
	}
}

func TestRunLoadTestWeightedScenario(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	var aCount int64
	var bCount int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			atomic.AddInt64(&aCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/b":
			atomic.AddInt64(&bCount, 1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                server.URL + "/a",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        8,
		TestDuration:       2,
		Scenario: &ScenarioConfig{
			Mode: scenarioModeWeighted,
			Tasks: []ScenarioTask{
				{Name: "A", Method: fasthttp.MethodGet, URL: server.URL + "/a", ExpectedStatusCode: http.StatusOK, Weight: 1},
				{Name: "B", Method: fasthttp.MethodGet, URL: server.URL + "/b", ExpectedStatusCode: http.StatusOK, Weight: 4},
			},
		},
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if result.TotalRequests <= 0 {
		t.Fatalf("expected requests > 0, got %d", result.TotalRequests)
	}
	if atomic.LoadInt64(&bCount) <= atomic.LoadInt64(&aCount) {
		t.Fatalf("expected weighted task B to run more often; a=%d b=%d", aCount, bCount)
	}
}

func TestSpawnRateImpactsThroughput(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fast := validConfig(server.URL)
	fast.Concurrency = 20
	fast.TestDuration = 1
	fast.SpawnRate = 1000

	slow := validConfig(server.URL)
	slow.Concurrency = 20
	slow.TestDuration = 1
	slow.SpawnRate = 1

	fastResult, err := runLoadTest(fast)
	if err != nil {
		t.Fatalf("fast run failed: %v", err)
	}
	slowResult, err := runLoadTest(slow)
	if err != nil {
		t.Fatalf("slow run failed: %v", err)
	}

	if fastResult.TotalRequests <= slowResult.TotalRequests*3 {
		t.Fatalf("expected fast spawn rate to generate much more traffic; fast=%d slow=%d", fastResult.TotalRequests, slowResult.TotalRequests)
	}
}

func TestStagesLoadProfile(t *testing.T) {
	cfg := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                "https://example.com",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        5,
		Stages: []LoadStage{
			{DurationSeconds: 1, TargetConcurrency: 2, SpawnRate: 2},
			{DurationSeconds: 2, TargetConcurrency: 5, SpawnRate: 1},
			{DurationSeconds: 1, TargetConcurrency: 0, SpawnRate: 2},
		},
	}

	if err := validateLoadTestConfig(&cfg); err != nil {
		t.Fatalf("expected valid stage config, got: %v", err)
	}

	profile, err := buildLoadProfile(cfg)
	if err != nil {
		t.Fatalf("buildLoadProfile returned error: %v", err)
	}
	if profile.totalDuration != 4*time.Second {
		t.Fatalf("expected 4s total duration, got %s", profile.totalDuration)
	}

	target, _ := profile.targetAt(500 * time.Millisecond)
	if target != 2 {
		t.Fatalf("expected stage 1 target=2, got %d", target)
	}
	target, _ = profile.targetAt(1500 * time.Millisecond)
	if target != 5 {
		t.Fatalf("expected stage 2 target=5, got %d", target)
	}
	target, _ = profile.targetAt(3500 * time.Millisecond)
	if target != 0 {
		t.Fatalf("expected stage 3 target=0, got %d", target)
	}
}

func TestThinkTimeFixedReducesRequestRate(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	noThink := validConfig(server.URL)
	noThink.Concurrency = 1
	noThink.TestDuration = 1

	withThink := validConfig(server.URL)
	withThink.Concurrency = 1
	withThink.TestDuration = 1
	withThink.ThinkTime = &ThinkTimeConfig{
		Mode:    thinkTimeModeFixed,
		FixedMs: 150,
	}

	noThinkResult, err := runLoadTest(noThink)
	if err != nil {
		t.Fatalf("noThink run failed: %v", err)
	}
	withThinkResult, err := runLoadTest(withThink)
	if err != nil {
		t.Fatalf("withThink run failed: %v", err)
	}
	if withThinkResult.TotalRequests >= noThinkResult.TotalRequests {
		t.Fatalf("expected think time to reduce request rate; noThink=%d withThink=%d", noThinkResult.TotalRequests, withThinkResult.TotalRequests)
	}
}

func TestThinkTimeDistributionSample(t *testing.T) {
	cfg, err := normalizeThinkTime(&ThinkTimeConfig{
		Mode:         thinkTimeModeDistribution,
		Distribution: thinkDistNormal,
		MeanMs:       50,
		StdDevMs:     10,
	})
	if err != nil {
		t.Fatalf("normalizeThinkTime returned error: %v", err)
	}

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		d := sampleThinkTime(cfg, rng)
		if d < 0 {
			t.Fatalf("expected non-negative think time, got %s", d)
		}
	}
}

func TestRunLoadTestThresholdFailure(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	maxP95 := 5.0
	maxErr := 0.0
	minRPS := 20000.0
	cfg := validConfig(server.URL)
	cfg.Thresholds = &ThresholdsConfig{
		MaxP95LatencyMs: &maxP95,
		MaxErrorRate:    &maxErr,
		MinRPS:          &minRPS,
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if !result.Thresholds.Enabled {
		t.Fatal("expected thresholds to be enabled")
	}
	if result.Thresholds.Passed {
		t.Fatal("expected threshold evaluation to fail")
	}
	if result.Passed {
		t.Fatal("expected result.Passed to be false on threshold failure")
	}
}

func TestRunLoadTestEndpointStatusAndErrorMetrics(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	var okCount int64
	var failCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			atomic.AddInt64(&okCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/fail":
			atomic.AddInt64(&failCount, 1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                server.URL + "/ok",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        4,
		TestDuration:       1,
		Scenario: &ScenarioConfig{
			Mode: scenarioModeWeighted,
			Tasks: []ScenarioTask{
				{Name: "ok", Method: fasthttp.MethodGet, URL: server.URL + "/ok", ExpectedStatusCode: http.StatusOK, Weight: 1},
				{Name: "fail", Method: fasthttp.MethodGet, URL: server.URL + "/fail", ExpectedStatusCode: http.StatusOK, Weight: 1},
			},
		},
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if len(result.EndpointMetrics) < 2 {
		t.Fatalf("expected endpoint metrics for both tasks, got %d", len(result.EndpointMetrics))
	}
	endpointSeen := map[string]bool{}
	for _, metric := range result.EndpointMetrics {
		endpointSeen[metric.Endpoint] = true
	}
	if !endpointSeen[fasthttp.MethodGet+" "+server.URL+"/ok"] || !endpointSeen[fasthttp.MethodGet+" "+server.URL+"/fail"] {
		t.Fatalf("expected endpoint metrics to be keyed by method+url, got %#v", result.EndpointMetrics)
	}

	has200 := false
	has500 := false
	for _, metric := range result.StatusMetrics {
		if metric.StatusCode == 200 {
			has200 = true
		}
		if metric.StatusCode == 500 {
			has500 = true
		}
	}
	if !has200 || !has500 {
		t.Fatalf("expected status metrics for 200 and 500, got %#v", result.StatusMetrics)
	}

	hasMismatch := false
	for _, metric := range result.ErrorMetrics {
		if metric.Type == errorTypeStatusMismatch && metric.Count > 0 {
			hasMismatch = true
			break
		}
	}
	if !hasMismatch {
		t.Fatalf("expected unexpected_status errors, got %#v", result.ErrorMetrics)
	}

	if atomic.LoadInt64(&okCount) == 0 || atomic.LoadInt64(&failCount) == 0 {
		t.Fatalf("expected both endpoints called, ok=%d fail=%d", okCount, failCount)
	}
}

func TestDataFeederAndCorrelation(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	tmpDir := t.TempDir()
	feederPath := filepath.Join(tmpDir, "users.json")
	feederContent := []byte(`[
		{"username":"alice","sku":"sku-a"},
		{"username":"bob","sku":"sku-b"}
	]`)
	if err := os.WriteFile(feederPath, feederContent, 0o644); err != nil {
		t.Fatalf("failed writing feeder file: %v", err)
	}

	var orderCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			var payload map[string]interface{}
			_ = json.Unmarshal(readRequestBody(t, r), &payload)
			username, _ := payload["username"].(string)
			resp := map[string]interface{}{
				"token": "tok-" + username,
				"user": map[string]interface{}{
					"id": username + "-id",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)
		case "/order":
			var payload map[string]interface{}
			_ = json.Unmarshal(readRequestBody(t, r), &payload)
			token, _ := payload["token"].(string)
			userID, _ := payload["userId"].(string)
			if token == "" || userID == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			atomic.AddInt64(&orderCount, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                server.URL + "/login",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        1,
		TestDuration:       1,
		DataFeeder: &DataFeederConfig{
			File:   feederPath,
			Format: dataFeederFormatJSON,
			Loop:   true,
		},
		Scenario: &ScenarioConfig{
			Mode: scenarioModeSequential,
			Tasks: []ScenarioTask{
				{
					Name:               "login",
					Method:             fasthttp.MethodPost,
					URL:                server.URL + "/login",
					RequestBody:        `{"username":"{{username}}"}`,
					ExpectedStatusCode: http.StatusOK,
					Capture: map[string]string{
						"token":  "token",
						"userId": "user.id",
					},
				},
				{
					Name:               "order",
					Method:             fasthttp.MethodPost,
					URL:                server.URL + "/order",
					RequestBody:        `{"token":"{{token}}","userId":"{{userId}}","sku":"{{sku}}"}`,
					ExpectedStatusCode: http.StatusCreated,
				},
			},
		},
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		t.Fatalf("runLoadTest returned error: %v", err)
	}
	if result.SuccessRate < 95 {
		t.Fatalf("expected high success with correlation, got %.2f", result.SuccessRate)
	}
	if atomic.LoadInt64(&orderCount) == 0 {
		t.Fatal("expected orders to be executed")
	}
}

func TestExportReportsAndComparison(t *testing.T) {
	isolateState(t)

	reportDir := t.TempDir()
	current := ExportedRunRecord{
		ID:        "current-1",
		Config:    validConfig("https://example.com"),
		Result:    validResult(),
		Timestamp: time.Now(),
	}

	baselineResult := validResult()
	baselineResult.P95Latency = 50
	baselineResult.ErrorRate = 10
	baselineResult.SuccessRate = 90
	baseline := &HistoryEntry{
		ID:        "baseline-1",
		Config:    validConfig("https://example.com"),
		Result:    baselineResult,
		Timestamp: time.Now().Add(-1 * time.Hour),
	}

	artifacts, comparison, err := exportReports(current, baseline, reportDir)
	if err != nil {
		t.Fatalf("exportReports returned error: %v", err)
	}
	if comparison == nil {
		t.Fatal("expected comparison result")
	}
	if _, err := os.Stat(artifacts.JSONPath); err != nil {
		t.Fatalf("json report not created: %v", err)
	}
	if _, err := os.Stat(artifacts.CSVPath); err != nil {
		t.Fatalf("csv report not created: %v", err)
	}
	if _, err := os.Stat(artifacts.HTMLPath); err != nil {
		t.Fatalf("html report not created: %v", err)
	}
}

func TestCLIReturnsThresholdExitCode(t *testing.T) {
	isolateState(t)
	requestTimeout = 500 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	maxP95 := 1.0
	cfg := validConfig(server.URL)
	cfg.Thresholds = &ThresholdsConfig{
		MaxP95LatencyMs: &maxP95,
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed marshaling config: %v", err)
	}
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("failed writing config: %v", err)
	}

	exitCode := runCLIMode(CLIRunOptions{
		ConfigPath:       configPath,
		ReportDir:        tmpDir,
		SaveHistory:      false,
		FailOnThresholds: true,
	})
	if exitCode != exitCodeThresholdFailed {
		t.Fatalf("expected threshold failure exit code %d, got %d", exitCodeThresholdFailed, exitCode)
	}
}

func TestReadConfigFromFileResolvesRelativeDataFeederPath(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("failed creating data directory: %v", err)
	}

	feederPath := filepath.Join(dataDir, "users.csv")
	if err := os.WriteFile(feederPath, []byte("username,sku\nalice,sku-a\n"), 0o644); err != nil {
		t.Fatalf("failed writing feeder file: %v", err)
	}

	configPath := filepath.Join(tmpDir, "config.json")
	config := TestConfig{
		Method:             fasthttp.MethodGet,
		URL:                "https://example.com/health",
		ExpectedStatusCode: http.StatusOK,
		Concurrency:        1,
		TestDuration:       1,
		DataFeeder: &DataFeederConfig{
			File:   filepath.Join("data", "users.csv"),
			Format: dataFeederFormatCSV,
			Loop:   true,
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed marshaling config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("failed writing config file: %v", err)
	}

	loaded, err := readConfigFromFile(configPath)
	if err != nil {
		t.Fatalf("readConfigFromFile returned error: %v", err)
	}
	if loaded.DataFeeder == nil {
		t.Fatal("expected dataFeeder to be present")
	}
	expected := filepath.Clean(feederPath)
	if loaded.DataFeeder.File != expected {
		t.Fatalf("expected feeder path %q, got %q", expected, loaded.DataFeeder.File)
	}
}

func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("failed reading request body: %v", err)
	}
	return body
}
