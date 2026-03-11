package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	exitCodeSuccess         = 0
	exitCodeRuntimeError    = 1
	exitCodeThresholdFailed = 2
)

type CLIRunOptions struct {
	ConfigPath        string
	ReportDir         string
	CompareWithID     string
	CompareWithLatest bool
	SaveHistory       bool
	FailOnThresholds  bool
}

func runCLIMode(options CLIRunOptions) int {
	cfg, err := readConfigFromFile(options.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed loading config: %v\n", err)
		return exitCodeRuntimeError
	}

	if err := validateLoadTestConfig(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		return exitCodeRuntimeError
	}

	result, err := runLoadTest(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run failed: %v\n", err)
		return exitCodeRuntimeError
	}

	record := ExportedRunRecord{
		ID:        uuid.NewString(),
		Config:    cfg,
		Result:    result,
		Timestamp: time.Now(),
	}

	if options.SaveHistory {
		entry := HistoryEntry{
			ID:        record.ID,
			Config:    record.Config,
			Result:    record.Result,
			Timestamp: record.Timestamp,
		}
		if err := appendHistoryEntry(entry); err != nil {
			fmt.Fprintf(os.Stderr, "failed saving history: %v\n", err)
			return exitCodeRuntimeError
		}
	}

	var baseline *HistoryEntry
	if strings.TrimSpace(options.CompareWithID) != "" {
		baseline, err = getHistoryEntryByID(strings.TrimSpace(options.CompareWithID))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed loading baseline by id: %v\n", err)
			return exitCodeRuntimeError
		}
	} else if options.CompareWithLatest {
		baseline = getLatestHistoryEntry(&record.ID)
	}

	reportDir := strings.TrimSpace(options.ReportDir)
	if reportDir == "" {
		reportDir = "reports"
	}
	artifacts, comparison, err := exportReports(record, baseline, reportDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed exporting reports: %v\n", err)
		return exitCodeRuntimeError
	}

	summary := map[string]interface{}{
		"id":                 record.ID,
		"totalRequests":      result.TotalRequests,
		"successRate":        result.SuccessRate,
		"errorRate":          result.ErrorRate,
		"requestsPerSecond":  result.RequestsPerSecond,
		"p95LatencyMs":       result.P95Latency,
		"passed":             result.Passed,
		"thresholds":         result.Thresholds,
		"jsonReportPath":     artifacts.JSONPath,
		"csvReportPath":      artifacts.CSVPath,
		"htmlReportPath":     artifacts.HTMLPath,
		"comparisonBaseline": "",
	}
	if baseline != nil {
		summary["comparisonBaseline"] = baseline.ID
	}
	if comparison != nil {
		summary["comparison"] = comparison
	}

	encoded, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(encoded))

	if options.FailOnThresholds && result.Thresholds.Enabled && !result.Thresholds.Passed {
		return exitCodeThresholdFailed
	}
	return exitCodeSuccess
}

func readConfigFromFile(path string) (TestConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TestConfig{}, err
	}
	var cfg TestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return TestConfig{}, err
	}
	resolveConfigRelativePaths(&cfg, path)
	return cfg, nil
}

func resolveConfigRelativePaths(cfg *TestConfig, configPath string) {
	if cfg == nil || cfg.DataFeeder == nil {
		return
	}

	file := strings.TrimSpace(cfg.DataFeeder.File)
	if file == "" || filepath.IsAbs(file) {
		return
	}

	baseDir := filepath.Dir(configPath)
	cfg.DataFeeder.File = filepath.Clean(filepath.Join(baseDir, file))
}
