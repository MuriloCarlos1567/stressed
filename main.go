package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
)

const (
	defaultRequestTimeout = 10 * time.Second
	maxRedirects          = 5

	minStatusCode = 100
	maxStatusCode = 599

	minConcurrency = 1
	maxConcurrency = 1000

	minDurationSeconds = 1
	maxDurationSeconds = 86400

	minSpawnRate = 0.0

	scenarioModeSequential = "sequential"
	scenarioModeWeighted   = "weighted"

	thinkTimeModeNone         = "none"
	thinkTimeModeFixed        = "fixed"
	thinkTimeModeRandom       = "random"
	thinkTimeModeDistribution = "distribution"

	thinkDistUniform     = "uniform"
	thinkDistNormal      = "normal"
	thinkDistExponential = "exponential"

	dataFeederFormatCSV  = "csv"
	dataFeederFormatJSON = "json"
)

type TestConfig struct {
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	ExpectedStatusCode int               `json:"expectedStatusCode"`
	RequestBody        string            `json:"requestBody"`
	Headers            map[string]string `json:"headers"`
	Concurrency        int               `json:"concurrency"`
	TestDuration       int               `json:"testDurationSeconds"`

	SpawnRate       float64     `json:"spawnRate"`
	RampUpSeconds   int         `json:"rampUpSeconds"`
	RampDownSeconds int         `json:"rampDownSeconds"`
	Stages          []LoadStage `json:"stages"`

	Scenario   *ScenarioConfig   `json:"scenario"`
	ThinkTime  *ThinkTimeConfig  `json:"thinkTime"`
	Thresholds *ThresholdsConfig `json:"thresholds"`
	DataFeeder *DataFeederConfig `json:"dataFeeder"`
}

type LoadStage struct {
	DurationSeconds   int     `json:"durationSeconds"`
	TargetConcurrency int     `json:"targetConcurrency"`
	SpawnRate         float64 `json:"spawnRate"`
}

type ScenarioConfig struct {
	Mode  string         `json:"mode"`
	Tasks []ScenarioTask `json:"tasks"`
}

type ScenarioTask struct {
	Name               string            `json:"name"`
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	ExpectedStatusCode int               `json:"expectedStatusCode"`
	RequestBody        string            `json:"requestBody"`
	Headers            map[string]string `json:"headers"`
	Weight             int               `json:"weight"`
	Capture            map[string]string `json:"capture"`
}

type ThinkTimeConfig struct {
	Mode         string  `json:"mode"`
	FixedMs      int     `json:"fixedMs"`
	MinMs        int     `json:"minMs"`
	MaxMs        int     `json:"maxMs"`
	Distribution string  `json:"distribution"`
	MeanMs       float64 `json:"meanMs"`
	StdDevMs     float64 `json:"stdDevMs"`
}

type DataFeederConfig struct {
	File   string `json:"file"`
	Format string `json:"format"`
	Loop   bool   `json:"loop"`
}

type ThresholdsConfig struct {
	MaxP95LatencyMs *float64 `json:"maxP95LatencyMs"`
	MaxErrorRate    *float64 `json:"maxErrorRate"`
	MinRPS          *float64 `json:"minRps"`
}

type ThresholdCheckResult struct {
	Name     string  `json:"name"`
	Actual   float64 `json:"actual"`
	Expected string  `json:"expected"`
	Passed   bool    `json:"passed"`
}

type ThresholdEvaluation struct {
	Enabled bool                   `json:"enabled"`
	Passed  bool                   `json:"passed"`
	Checks  []ThresholdCheckResult `json:"checks"`
}

type EndpointMetric struct {
	Endpoint         string  `json:"endpoint"`
	TotalRequests    int     `json:"totalRequests"`
	SuccessRate      float64 `json:"successRate"`
	AverageLatencyMs float64 `json:"averageLatencyMs"`
	P50LatencyMs     float64 `json:"p50LatencyMs"`
	P90LatencyMs     float64 `json:"p90LatencyMs"`
	P95LatencyMs     float64 `json:"p95LatencyMs"`
	P99LatencyMs     float64 `json:"p99LatencyMs"`
}

type StatusMetric struct {
	StatusCode int     `json:"statusCode"`
	Count      int     `json:"count"`
	Rate       float64 `json:"rate"`
}

type ErrorMetric struct {
	Type  string  `json:"type"`
	Count int     `json:"count"`
	Rate  float64 `json:"rate"`
}

type TestResult struct {
	SuccessRate       float64   `json:"successRate"`
	ErrorRate         float64   `json:"errorRate"`
	AverageLatency    float64   `json:"averageLatencyMs"`
	P50Latency        float64   `json:"p50LatencyMs"`
	P90Latency        float64   `json:"p90LatencyMs"`
	P95Latency        float64   `json:"p95LatencyMs"`
	P99Latency        float64   `json:"p99LatencyMs"`
	TotalRequests     int       `json:"totalRequests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	StartTime         time.Time `json:"startTime"`
	EndTime           time.Time `json:"endTime"`

	EndpointMetrics []EndpointMetric    `json:"endpointMetrics"`
	StatusMetrics   []StatusMetric      `json:"statusMetrics"`
	ErrorMetrics    []ErrorMetric       `json:"errorMetrics"`
	Thresholds      ThresholdEvaluation `json:"thresholds"`
	Passed          bool                `json:"passed"`
}

type HistoryEntry struct {
	ID        string     `json:"id"`
	Config    TestConfig `json:"config"`
	Result    TestResult `json:"result"`
	Timestamp time.Time  `json:"timestamp"`
}

type ReportExportRequest struct {
	Config            TestConfig  `json:"config"`
	Result            TestResult  `json:"result"`
	EntryID           string      `json:"entryId"`
	ReportDir         string      `json:"reportDir"`
	CompareWithID     string      `json:"compareWithId"`
	CompareWithLatest bool        `json:"compareWithLatest"`
	GeneratedAt       *time.Time  `json:"generatedAt"`
	Metadata          interface{} `json:"metadata"`
}

type ReportExportResponse struct {
	JSONPath   string            `json:"jsonPath"`
	CSVPath    string            `json:"csvPath"`
	HTMLPath   string            `json:"htmlPath"`
	Comparison *RunComparison    `json:"comparison,omitempty"`
	Current    ExportedRunRecord `json:"current"`
	BaselineID string            `json:"baselineId,omitempty"`
}

var (
	resultsFile    = "results.json"
	requestTimeout = defaultRequestTimeout

	historyMu sync.RWMutex
	history   []HistoryEntry

	errHistoryIDNotFound = errors.New("id not found")
	staticFileHandler    = fasthttp.FSHandler("./public", 0)
)

func main() {
	host := flag.String("host", "127.0.0.1", "host to listen on")
	port := flag.String("port", "8090", "port to listen on")
	runConfig := flag.String("run-config", "", "path to load-test config JSON to run once and exit")
	reportDir := flag.String("report-dir", "reports", "directory for generated reports in CLI mode")
	compareWithID := flag.String("compare-with-id", "", "history entry id to compare against in CLI mode")
	compareLatest := flag.Bool("compare-latest", false, "compare against latest history entry in CLI mode")
	saveHistory := flag.Bool("save-history", true, "save one-off CLI runs to history")
	failOnThresholds := flag.Bool("fail-on-thresholds", true, "exit non-zero when thresholds fail in CLI mode")
	flag.Parse()

	loadHistory()

	if strings.TrimSpace(*runConfig) != "" {
		options := CLIRunOptions{
			ConfigPath:        strings.TrimSpace(*runConfig),
			ReportDir:         strings.TrimSpace(*reportDir),
			CompareWithID:     strings.TrimSpace(*compareWithID),
			CompareWithLatest: *compareLatest,
			SaveHistory:       *saveHistory,
			FailOnThresholds:  *failOnThresholds,
		}
		os.Exit(runCLIMode(options))
	}

	addr := fmt.Sprintf("%s:%s", *host, *port)
	log.Println("Starting server at http://" + addr)
	if err := fasthttp.ListenAndServe(addr, requestHandler); err != nil {
		log.Fatalf("Error starting server: %s", err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	switch string(ctx.Path()) {
	case "/api/test":
		if ctx.IsPost() {
			handleTest(ctx)
			return
		}
		ctx.Error("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
	case "/api/history":
		switch string(ctx.Method()) {
		case fasthttp.MethodGet:
			handleHistory(ctx)
		case fasthttp.MethodDelete:
			handleDeleteHistory(ctx)
		default:
			ctx.Error("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
		}
	case "/api/validate":
		if ctx.IsPost() {
			handleValidateTest(ctx)
			return
		}
		ctx.Error("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
	case "/api/saveTest":
		if ctx.IsPost() {
			handleSaveTest(ctx)
			return
		}
		ctx.Error("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
	case "/api/report/export":
		if ctx.IsPost() {
			handleExportReport(ctx)
			return
		}
		ctx.Error("Method Not Allowed", fasthttp.StatusMethodNotAllowed)
	default:
		staticFileHandler(ctx)
	}
}

func handleValidateTest(ctx *fasthttp.RequestCtx) {
	var cfg TestConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	task, err := resolveValidationTask(cfg)
	if err != nil {
		ctx.Error("Invalid test config: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	client := newFastHTTPClient()
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	buildTaskRequest(req, task)
	if err := doRequestWithRedirects(client, req, resp, requestTimeout); err != nil {
		ctx.Error("Error making request: "+err.Error(), fasthttp.StatusBadGateway)
		return
	}

	var bodyObj interface{}
	contentType := strings.ToLower(string(resp.Header.Peek("Content-Type")))
	if strings.Contains(contentType, "application/json") {
		if err := json.Unmarshal(resp.Body(), &bodyObj); err != nil {
			bodyObj = string(resp.Body())
		}
	} else {
		bodyObj = string(resp.Body())
	}

	resHeaders := make(map[string]string)
	resp.Header.VisitAll(func(key, value []byte) {
		resHeaders[string(key)] = string(value)
	})

	writeJSON(ctx, fasthttp.StatusOK, map[string]interface{}{
		"statusCode": resp.StatusCode(),
		"headers":    resHeaders,
		"body":       bodyObj,
	})
}

func handleSaveTest(ctx *fasthttp.RequestCtx) {
	var payload struct {
		Config TestConfig `json:"config"`
		Result TestResult `json:"result"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	if err := validateLoadTestConfig(&payload.Config); err != nil {
		ctx.Error("Invalid config: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	if err := validateTestResult(payload.Result); err != nil {
		ctx.Error("Invalid result: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	entry := HistoryEntry{
		ID:        uuid.NewString(),
		Config:    payload.Config,
		Result:    payload.Result,
		Timestamp: time.Now(),
	}
	if err := appendHistoryEntry(entry); err != nil {
		ctx.Error("Error saving history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	writeJSON(ctx, fasthttp.StatusOK, map[string]string{"message": "Test saved successfully", "id": entry.ID})
}

func handleDeleteHistory(ctx *fasthttp.RequestCtx) {
	var payload struct {
		ID *string `json:"id"`
	}
	if len(ctx.PostBody()) > 0 {
		if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
			ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
			return
		}
	}

	if err := deleteHistoryEntry(payload.ID); err != nil {
		if errors.Is(err, errHistoryIDNotFound) {
			ctx.Error("ID not found", fasthttp.StatusBadRequest)
			return
		}
		ctx.Error("Error saving history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	writeJSON(ctx, fasthttp.StatusOK, map[string]string{"message": "History updated successfully"})
}

func handleTest(ctx *fasthttp.RequestCtx) {
	var cfg TestConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	if err := validateLoadTestConfig(&cfg); err != nil {
		ctx.Error("Invalid test config: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		ctx.Error("Failed to run test: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	writeJSON(ctx, fasthttp.StatusOK, result)
}

func handleHistory(ctx *fasthttp.RequestCtx) {
	historyMu.RLock()
	defer historyMu.RUnlock()
	writeJSON(ctx, fasthttp.StatusOK, history)
}

func handleExportReport(ctx *fasthttp.RequestCtx) {
	var req ReportExportRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	if err := validateLoadTestConfig(&req.Config); err != nil {
		ctx.Error("Invalid config: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	if err := validateTestResult(req.Result); err != nil {
		ctx.Error("Invalid result: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	reportDir := strings.TrimSpace(req.ReportDir)
	if reportDir == "" {
		reportDir = "reports"
	}

	record := ExportedRunRecord{
		ID:        strings.TrimSpace(req.EntryID),
		Config:    req.Config,
		Result:    req.Result,
		Timestamp: time.Now(),
	}
	if req.GeneratedAt != nil {
		record.Timestamp = *req.GeneratedAt
	}
	if record.ID == "" {
		record.ID = uuid.NewString()
	}

	var baseline *HistoryEntry
	var err error
	if strings.TrimSpace(req.CompareWithID) != "" {
		baseline, err = getHistoryEntryByID(strings.TrimSpace(req.CompareWithID))
		if err != nil {
			ctx.Error("Comparison baseline not found: "+err.Error(), fasthttp.StatusBadRequest)
			return
		}
	} else if req.CompareWithLatest {
		baseline = getLatestHistoryEntry(&record.ID)
	}

	artifacts, comparison, err := exportReports(record, baseline, reportDir)
	if err != nil {
		ctx.Error("Failed to export reports: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	response := ReportExportResponse{
		JSONPath:   artifacts.JSONPath,
		CSVPath:    artifacts.CSVPath,
		HTMLPath:   artifacts.HTMLPath,
		Comparison: comparison,
		Current:    record,
	}
	if baseline != nil {
		response.BaselineID = baseline.ID
	}

	writeJSON(ctx, fasthttp.StatusOK, response)
}
