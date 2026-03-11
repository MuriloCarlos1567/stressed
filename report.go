package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ReportArtifacts struct {
	JSONPath string `json:"jsonPath"`
	CSVPath  string `json:"csvPath"`
	HTMLPath string `json:"htmlPath"`
}

type ExportedRunRecord struct {
	ID        string     `json:"id"`
	Config    TestConfig `json:"config"`
	Result    TestResult `json:"result"`
	Timestamp time.Time  `json:"timestamp"`
}

type DeltaMetric struct {
	Current      float64 `json:"current"`
	Baseline     float64 `json:"baseline"`
	Delta        float64 `json:"delta"`
	DeltaPercent float64 `json:"deltaPercent"`
}

type RunComparison struct {
	CurrentID       string      `json:"currentId"`
	BaselineID      string      `json:"baselineId"`
	RequestsPerSec  DeltaMetric `json:"requestsPerSecond"`
	P95LatencyMs    DeltaMetric `json:"p95LatencyMs"`
	ErrorRate       DeltaMetric `json:"errorRate"`
	SuccessRate     DeltaMetric `json:"successRate"`
	TotalRequests   DeltaMetric `json:"totalRequests"`
	CurrentPassed   bool        `json:"currentPassed"`
	BaselinePassed  bool        `json:"baselinePassed"`
	ThresholdStatus string      `json:"thresholdStatus"`
}

func exportReports(current ExportedRunRecord, baseline *HistoryEntry, reportDir string) (ReportArtifacts, *RunComparison, error) {
	if strings.TrimSpace(reportDir) == "" {
		reportDir = "reports"
	}

	absDir, err := filepath.Abs(reportDir)
	if err != nil {
		return ReportArtifacts{}, nil, err
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return ReportArtifacts{}, nil, err
	}

	baseName := buildReportBaseName(current)
	jsonPath := filepath.Join(absDir, baseName+".json")
	csvPath := filepath.Join(absDir, baseName+".csv")
	htmlPath := filepath.Join(absDir, baseName+".html")

	var comparison *RunComparison
	if baseline != nil {
		comparison = buildRunComparison(current, *baseline)
	}

	jsonData, err := buildJSONReport(current, baseline, comparison)
	if err != nil {
		return ReportArtifacts{}, nil, err
	}
	if err := os.WriteFile(jsonPath, jsonData, 0o644); err != nil {
		return ReportArtifacts{}, nil, err
	}

	csvData, err := buildCSVReport(current, baseline, comparison)
	if err != nil {
		return ReportArtifacts{}, nil, err
	}
	if err := os.WriteFile(csvPath, csvData, 0o644); err != nil {
		return ReportArtifacts{}, nil, err
	}

	htmlData, err := buildHTMLReport(current, baseline, comparison)
	if err != nil {
		return ReportArtifacts{}, nil, err
	}
	if err := os.WriteFile(htmlPath, htmlData, 0o644); err != nil {
		return ReportArtifacts{}, nil, err
	}

	return ReportArtifacts{
		JSONPath: jsonPath,
		CSVPath:  csvPath,
		HTMLPath: htmlPath,
	}, comparison, nil
}

func buildReportBaseName(current ExportedRunRecord) string {
	id := strings.TrimSpace(current.ID)
	if id == "" {
		id = "run"
	}
	if len(id) > 8 {
		id = id[:8]
	}
	id = strings.ReplaceAll(id, " ", "_")
	ts := current.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	return fmt.Sprintf("run_%s_%s", ts.Format("20060102_150405"), id)
}

func buildJSONReport(current ExportedRunRecord, baseline *HistoryEntry, comparison *RunComparison) ([]byte, error) {
	payload := map[string]interface{}{
		"generatedAt": time.Now(),
		"current":     current,
		"comparison":  comparison,
	}
	if baseline != nil {
		payload["baseline"] = baseline
	}
	return json.MarshalIndent(payload, "", "  ")
}

func buildCSVReport(current ExportedRunRecord, baseline *HistoryEntry, comparison *RunComparison) ([]byte, error) {
	buf := &bytes.Buffer{}
	writer := csv.NewWriter(buf)

	if err := writer.Write([]string{"section", "name", "value"}); err != nil {
		return nil, err
	}

	summaryRows := [][]string{
		{"summary", "id", current.ID},
		{"summary", "timestamp", current.Timestamp.Format(time.RFC3339)},
		{"summary", "total_requests", strconv.Itoa(current.Result.TotalRequests)},
		{"summary", "success_rate", formatFloat(current.Result.SuccessRate)},
		{"summary", "error_rate", formatFloat(current.Result.ErrorRate)},
		{"summary", "rps", formatFloat(current.Result.RequestsPerSecond)},
		{"summary", "avg_latency_ms", formatFloat(current.Result.AverageLatency)},
		{"summary", "p50_latency_ms", formatFloat(current.Result.P50Latency)},
		{"summary", "p90_latency_ms", formatFloat(current.Result.P90Latency)},
		{"summary", "p95_latency_ms", formatFloat(current.Result.P95Latency)},
		{"summary", "p99_latency_ms", formatFloat(current.Result.P99Latency)},
		{"summary", "passed", strconv.FormatBool(current.Result.Passed)},
	}
	for _, row := range summaryRows {
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}

	if comparison != nil {
		comparisonRows := [][]string{
			{"comparison", "baseline_id", comparison.BaselineID},
			{"comparison", "rps_delta", formatFloat(comparison.RequestsPerSec.Delta)},
			{"comparison", "p95_latency_ms_delta", formatFloat(comparison.P95LatencyMs.Delta)},
			{"comparison", "error_rate_delta", formatFloat(comparison.ErrorRate.Delta)},
			{"comparison", "success_rate_delta", formatFloat(comparison.SuccessRate.Delta)},
			{"comparison", "threshold_status", comparison.ThresholdStatus},
		}
		for _, row := range comparisonRows {
			if err := writer.Write(row); err != nil {
				return nil, err
			}
		}
	}

	if err := writer.Write([]string{}); err != nil {
		return nil, err
	}
	if err := writer.Write([]string{"endpoint", "total_requests", "success_rate", "avg_ms", "p50_ms", "p90_ms", "p95_ms", "p99_ms"}); err != nil {
		return nil, err
	}
	for _, metric := range current.Result.EndpointMetrics {
		row := []string{
			metric.Endpoint,
			strconv.Itoa(metric.TotalRequests),
			formatFloat(metric.SuccessRate),
			formatFloat(metric.AverageLatencyMs),
			formatFloat(metric.P50LatencyMs),
			formatFloat(metric.P90LatencyMs),
			formatFloat(metric.P95LatencyMs),
			formatFloat(metric.P99LatencyMs),
		}
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}

	if err := writer.Write([]string{}); err != nil {
		return nil, err
	}
	if err := writer.Write([]string{"status_code", "count", "rate"}); err != nil {
		return nil, err
	}
	for _, metric := range current.Result.StatusMetrics {
		if err := writer.Write([]string{
			strconv.Itoa(metric.StatusCode),
			strconv.Itoa(metric.Count),
			formatFloat(metric.Rate),
		}); err != nil {
			return nil, err
		}
	}

	if err := writer.Write([]string{}); err != nil {
		return nil, err
	}
	if err := writer.Write([]string{"error_type", "count", "rate"}); err != nil {
		return nil, err
	}
	for _, metric := range current.Result.ErrorMetrics {
		if err := writer.Write([]string{
			metric.Type,
			strconv.Itoa(metric.Count),
			formatFloat(metric.Rate),
		}); err != nil {
			return nil, err
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildHTMLReport(current ExportedRunRecord, baseline *HistoryEntry, comparison *RunComparison) ([]byte, error) {
	const tpl = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Stressed Report</title>
  <style>
    body { font-family: "Segoe UI", Arial, sans-serif; margin: 24px; color: #222; background: #f8fafc; }
    h1,h2 { margin: 0 0 10px 0; }
    .card { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px; padding: 16px; margin-bottom: 16px; }
    table { width: 100%; border-collapse: collapse; margin-top: 8px; }
    th, td { border: 1px solid #e5e7eb; padding: 8px; text-align: left; font-size: 14px; }
    th { background: #f1f5f9; }
    .ok { color: #166534; font-weight: 700; }
    .bad { color: #991b1b; font-weight: 700; }
    .mono { font-family: Consolas, monospace; }
  </style>
</head>
<body>
  <h1>Stressed Run Report</h1>
  <div class="card">
    <h2>Summary</h2>
    <p><strong>Run ID:</strong> <span class="mono">{{.Current.ID}}</span></p>
    <p><strong>Timestamp:</strong> {{.Current.Timestamp}}</p>
    <p><strong>Passed:</strong> {{if .Current.Result.Passed}}<span class="ok">YES</span>{{else}}<span class="bad">NO</span>{{end}}</p>
    <table>
      <tr><th>Total Requests</th><th>RPS</th><th>Success %</th><th>Error %</th><th>Avg ms</th><th>p95 ms</th></tr>
      <tr>
        <td>{{.Current.Result.TotalRequests}}</td>
        <td>{{printf "%.2f" .Current.Result.RequestsPerSecond}}</td>
        <td>{{printf "%.2f" .Current.Result.SuccessRate}}</td>
        <td>{{printf "%.2f" .Current.Result.ErrorRate}}</td>
        <td>{{printf "%.2f" .Current.Result.AverageLatency}}</td>
        <td>{{printf "%.2f" .Current.Result.P95Latency}}</td>
      </tr>
    </table>
  </div>
  {{if .Comparison}}
  <div class="card">
    <h2>Comparison</h2>
    <p><strong>Baseline ID:</strong> <span class="mono">{{.Comparison.BaselineID}}</span></p>
    <p><strong>Status:</strong> {{.Comparison.ThresholdStatus}}</p>
    <table>
      <tr><th>Metric</th><th>Current</th><th>Baseline</th><th>Delta</th></tr>
      <tr><td>RPS</td><td>{{printf "%.2f" .Comparison.RequestsPerSec.Current}}</td><td>{{printf "%.2f" .Comparison.RequestsPerSec.Baseline}}</td><td>{{printf "%.2f" .Comparison.RequestsPerSec.Delta}}</td></tr>
      <tr><td>p95 (ms)</td><td>{{printf "%.2f" .Comparison.P95LatencyMs.Current}}</td><td>{{printf "%.2f" .Comparison.P95LatencyMs.Baseline}}</td><td>{{printf "%.2f" .Comparison.P95LatencyMs.Delta}}</td></tr>
      <tr><td>Error %</td><td>{{printf "%.2f" .Comparison.ErrorRate.Current}}</td><td>{{printf "%.2f" .Comparison.ErrorRate.Baseline}}</td><td>{{printf "%.2f" .Comparison.ErrorRate.Delta}}</td></tr>
    </table>
  </div>
  {{end}}
  <div class="card">
    <h2>Endpoint Metrics</h2>
    <table>
      <tr><th>Endpoint</th><th>Total</th><th>Success %</th><th>Avg ms</th><th>p50</th><th>p90</th><th>p95</th><th>p99</th></tr>
      {{range .Current.Result.EndpointMetrics}}
      <tr>
        <td class="mono">{{.Endpoint}}</td>
        <td>{{.TotalRequests}}</td>
        <td>{{printf "%.2f" .SuccessRate}}</td>
        <td>{{printf "%.2f" .AverageLatencyMs}}</td>
        <td>{{printf "%.2f" .P50LatencyMs}}</td>
        <td>{{printf "%.2f" .P90LatencyMs}}</td>
        <td>{{printf "%.2f" .P95LatencyMs}}</td>
        <td>{{printf "%.2f" .P99LatencyMs}}</td>
      </tr>
      {{end}}
    </table>
  </div>
  <div class="card">
    <h2>Error Groups</h2>
    <table>
      <tr><th>Type</th><th>Count</th><th>Rate %</th></tr>
      {{range .Current.Result.ErrorMetrics}}
      <tr><td>{{.Type}}</td><td>{{.Count}}</td><td>{{printf "%.2f" .Rate}}</td></tr>
      {{end}}
    </table>
  </div>
</body>
</html>`

	t, err := template.New("report").Parse(tpl)
	if err != nil {
		return nil, err
	}

	data := struct {
		Current    ExportedRunRecord
		Baseline   *HistoryEntry
		Comparison *RunComparison
	}{
		Current:    current,
		Baseline:   baseline,
		Comparison: comparison,
	}

	buf := &bytes.Buffer{}
	if err := t.Execute(buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildRunComparison(current ExportedRunRecord, baseline HistoryEntry) *RunComparison {
	comparison := &RunComparison{
		CurrentID:      current.ID,
		BaselineID:     baseline.ID,
		RequestsPerSec: makeDelta(current.Result.RequestsPerSecond, baseline.Result.RequestsPerSecond),
		P95LatencyMs:   makeDelta(current.Result.P95Latency, baseline.Result.P95Latency),
		ErrorRate:      makeDelta(current.Result.ErrorRate, baseline.Result.ErrorRate),
		SuccessRate:    makeDelta(current.Result.SuccessRate, baseline.Result.SuccessRate),
		TotalRequests:  makeDelta(float64(current.Result.TotalRequests), float64(baseline.Result.TotalRequests)),
		CurrentPassed:  current.Result.Passed,
		BaselinePassed: baseline.Result.Passed,
	}

	switch {
	case current.Result.Passed && baseline.Result.Passed:
		comparison.ThresholdStatus = "both passed"
	case current.Result.Passed && !baseline.Result.Passed:
		comparison.ThresholdStatus = "improved (baseline failed)"
	case !current.Result.Passed && baseline.Result.Passed:
		comparison.ThresholdStatus = "regressed (current failed)"
	default:
		comparison.ThresholdStatus = "both failed"
	}

	return comparison
}

func makeDelta(current, baseline float64) DeltaMetric {
	delta := current - baseline
	percent := 0.0
	if baseline != 0 {
		percent = (delta / baseline) * 100
	}
	return DeltaMetric{
		Current:      current,
		Baseline:     baseline,
		Delta:        delta,
		DeltaPercent: percent,
	}
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}
