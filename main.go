package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
)

type TestConfig struct {
	Method             string            `json:"method"`
	URL                string            `json:"url"`
	ExpectedStatusCode int               `json:"expectedStatusCode"`
	RequestBody        string            `json:"requestBody"`
	Headers            map[string]string `json:"headers"`
	Concurrency        int               `json:"concurrency"`
	TestDuration       int               `json:"testDurationSeconds"`
}

type TestResult struct {
	SuccessRate       float64   `json:"successRate"`
	AverageLatency    float64   `json:"averageLatencyMs"`
	P95Latency        float64   `json:"p95LatencyMs"`
	TotalRequests     int       `json:"totalRequests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	StartTime         time.Time `json:"startTime"`
	EndTime           time.Time `json:"endTime"`
}

type HistoryEntry struct {
	ID        string     `json:"id"`
	Config    TestConfig `json:"config"`
	Result    TestResult `json:"result"`
	Timestamp time.Time  `json:"timestamp"`
}

var resultsFile = "results.json"
var mu sync.Mutex
var history []HistoryEntry

func main() {
	host := flag.String("host", "127.0.0.1", "host to listen on")
	port := flag.String("port", "8080", "port to listen on")
	flag.Parse()
	loadHistory()
	addr := fmt.Sprintf("%s:%s", *host, *port)
	log.Println("Starting server at http://" + addr)
	if err := fasthttp.ListenAndServe(addr, requestHandler); err != nil {
		log.Fatalf("Error starting server: %s", err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	switch path {
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
	default:
		fs := fasthttp.FSHandler("./public", 0)
		fs(ctx)
	}
}

func handleValidateTest(ctx *fasthttp.RequestCtx) {
	var cfg TestConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	client := &fasthttp.Client{}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(cfg.URL)
	req.Header.SetMethod(cfg.Method)
	req.Header.Set("Accept", "*/*")
	if cfg.RequestBody != "" && (cfg.Method == "POST" || cfg.Method == "PUT" || cfg.Method == "PATCH") {
		req.SetBody([]byte(cfg.RequestBody))
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	if err := client.Do(req, resp); err != nil {
		ctx.Error("Error making request: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	if resp.StatusCode() == 301 || resp.StatusCode() == 302 {
		location := string(resp.Header.Peek("Location"))
		if location != "" {
			req.SetRequestURI(location)
			if err := client.Do(req, resp); err != nil {
				ctx.Error("Error following redirect: "+err.Error(), fasthttp.StatusInternalServerError)
				return
			}
		}
	}

	var bodyObj interface{}
	contentType := string(resp.Header.Peek("Content-Type"))
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

	responseObj := struct {
		StatusCode int               `json:"statusCode"`
		Headers    map[string]string `json:"headers"`
		Body       interface{}       `json:"body"`
	}{
		StatusCode: resp.StatusCode(),
		Headers:    resHeaders,
		Body:       bodyObj,
	}

	ctx.Response.Header.Set("Content-Type", "application/json")
	b, err := json.Marshal(responseObj)
	if err != nil {
		ctx.Error("Error encoding response: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetBody(b)
}

func handleSaveTest(ctx *fasthttp.RequestCtx) {
	var payload struct {
		Config TestConfig `json:"config"`
		Result TestResult `json:"result"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &payload); err != nil {
		fmt.Printf("Failed to decode JSON: %s\n", err)
		ctx.Error("Invalid JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	entry := HistoryEntry{
		ID:        uuid.NewString(),
		Config:    payload.Config,
		Result:    payload.Result,
		Timestamp: time.Now(),
	}

	mu.Lock()
	history = append(history, entry)
	mu.Unlock()

	file, err := os.Create(resultsFile)
	if err != nil {
		fmt.Printf("Failed to save history: %s\n", err)
		ctx.Error("Error saving history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(history); err != nil {
		fmt.Printf("Failed to encode history: %s\n", err)
		ctx.Error("Error encoding history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody([]byte(`{"message": "Test saved successfully"}`))
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

	mu.Lock()
	defer mu.Unlock()

	if payload.ID != nil {
		idToDelete := *payload.ID
		found := false
		for i, entry := range history {
			if entry.ID == idToDelete {
				history = append(history[:i], history[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			ctx.Error("ID not found", fasthttp.StatusBadRequest)
			return
		}
	} else {
		history = []HistoryEntry{}
	}

	file, err := os.Create(resultsFile)
	if err != nil {
		ctx.Error("Error saving history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(history); err != nil {
		ctx.Error("Error encoding history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBody([]byte(`{"message": "History updated successfully"}`))
}

func handleTest(ctx *fasthttp.RequestCtx) {
	var cfg TestConfig
	if err := json.Unmarshal(ctx.PostBody(), &cfg); err != nil {
		ctx.Error("Failed to decode JSON: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		ctx.Error("Failed to run test: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	time.Sleep(1 * time.Second)

	ctx.Response.Header.Set("Content-Type", "application/json")
	b, err := json.Marshal(result)
	if err != nil {
		ctx.Error("Error encoding result: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetBody(b)
}

func handleHistory(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Type", "application/json")
	mu.Lock()
	defer mu.Unlock()
	b, err := json.Marshal(history)
	if err != nil {
		ctx.Error("Error encoding history: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetBody(b)
}

func runLoadTest(cfg TestConfig) (TestResult, error) {
	var totalRequests int64
	var successCount int64

	latencyChan := make(chan time.Duration, 10000)
	stopChan := make(chan struct{})
	var wg sync.WaitGroup
	client := &fasthttp.Client{}
	startTime := time.Now()

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopChan:
					return
				default:
					t0 := time.Now()
					req := fasthttp.AcquireRequest()
					resp := fasthttp.AcquireResponse()
					req.SetRequestURI(cfg.URL)
					req.Header.SetMethod(cfg.Method)
					if cfg.RequestBody != "" {
						req.SetBody([]byte(cfg.RequestBody))
					}
					for k, v := range cfg.Headers {
						req.Header.Set(k, v)
					}
					err := client.Do(req, resp)
					t1 := time.Now()
					fasthttp.ReleaseRequest(req)
					fasthttp.ReleaseResponse(resp)
					if err == nil && resp.StatusCode() == cfg.ExpectedStatusCode {
						atomic.AddInt64(&successCount, 1)
					}
					atomic.AddInt64(&totalRequests, 1)
					select {
					case latencyChan <- t1.Sub(t0):
					default:
					}
				}
			}
		}()
	}

	time.Sleep(time.Duration(cfg.TestDuration) * time.Second)
	close(stopChan)
	wg.Wait()
	close(latencyChan)

	var latencies []time.Duration
	for l := range latencyChan {
		latencies = append(latencies, l)
	}

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	avgLatency := 0.0
	if len(latencies) > 0 {
		avgLatency = float64(sum.Milliseconds()) / float64(len(latencies))
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var p95 float64
	if len(latencies) > 0 {
		index95 := int(float64(len(latencies)) * 0.95)
		if index95 >= len(latencies) {
			index95 = len(latencies) - 1
		}
		p95 = float64(latencies[index95].Milliseconds())
	}

	endTime := time.Now()
	durationSeconds := endTime.Sub(startTime).Seconds()
	rps := 0.0
	if durationSeconds > 0 {
		rps = float64(totalRequests) / durationSeconds
	}

	tr := float64(totalRequests)
	sc := float64(successCount)
	successRate := 0.0
	if tr > 0 {
		successRate = (sc / tr) * 100.0
	}

	return TestResult{
		SuccessRate:       successRate,
		AverageLatency:    avgLatency,
		P95Latency:        p95,
		TotalRequests:     int(totalRequests),
		RequestsPerSecond: rps,
		StartTime:         startTime,
		EndTime:           endTime,
	}, nil
}

func loadHistory() {
	mu.Lock()
	defer mu.Unlock()
	file, err := os.Open(resultsFile)
	if os.IsNotExist(err) {
		log.Println("History file does not exist, starting empty.")
		history = []HistoryEntry{}
		return
	} else if err != nil {
		log.Println("Error opening history file:", err)
		history = []HistoryEntry{}
		return
	}
	defer file.Close()
	var loaded []HistoryEntry
	if err := json.NewDecoder(file).Decode(&loaded); err != nil {
		log.Println("Error decoding history:", err)
		history = []HistoryEntry{}
		return
	}
	history = loaded
	fmt.Printf("History loaded with %d entries.\n", len(history))
}
