# Stressed

Stressed is a load testing tool written in Go with a web UI for configuring runs, validating requests, and managing history.

## Features

- Load testing with configurable method, headers, body, concurrency, and duration
- Real load shaping with `spawnRate`, `rampUpSeconds`, `rampDownSeconds`, and custom `stages`
- Scenario execution with multiple tasks and modes:
  - `sequential` for user journeys (for example: login -> search -> buy)
  - `weighted` for probabilistic task mix
- Think time controls per virtual user:
  - `fixed`
  - `random` (min/max)
  - `distribution` (`uniform`, `normal`, `exponential`)
- Thresholds/SLO pass-fail:
  - `maxP95LatencyMs`
  - `maxErrorRate`
  - `minRps`
  - CI exit code support in CLI mode
- Detailed metrics:
  - Global: success rate, error rate, avg latency, p50/p90/p95/p99, total requests, RPS
  - By endpoint: total, success rate, avg latency, p50/p90/p95/p99
  - By status code: count and rate
  - Errors grouped by type (`timeout`, `network_error`, `unexpected_status`, `missing_variable`, `capture_error`)
- Data feeder + correlation:
  - CSV/JSON feeders
  - Template variables in URL/body/headers (`{{variable}}`)
  - Capture response fields and reuse in later tasks
- Rich report export:
  - JSON/CSV/HTML artifacts
  - Comparison against previous runs (by ID or latest history entry)
- Request validation endpoint (single request + response preview)
- Redirect following (with cap) and request timeout protection
- Server-side input validation for all API endpoints
- Local history persistence (`results.json`) with thread-safe writes

## Requirements

- Go 1.23+

## Run

```bash
git clone https://github.com/MuriloCarlos1567/stressed.git
cd stressed
go run main.go --host 127.0.0.1 --port 8000
```

The app will be available at `http://127.0.0.1:8000`.

## CLI Mode (CI-friendly)

Run one test config and exit with code:

- `0`: success
- `1`: runtime/config/export error
- `2`: thresholds failed

```bash
go run . \
  --run-config ./config/load-test.json \
  --report-dir ./reports \
  --compare-latest \
  --save-history \
  --fail-on-thresholds
```

## Development

```bash
go test ./...
go vet ./...
go build ./...
```

CI runs the same checks on every push and pull request.

## Main Endpoints

- `POST /api/test` -> run load test
- `POST /api/validate` -> run one validation request
- `POST /api/saveTest` -> save test result in history
- `GET /api/history` -> list saved tests
- `DELETE /api/history` -> delete one test by `id` or clear all
- `POST /api/report/export` -> export JSON/CSV/HTML report and optional comparison

## Example: Scenario + Stages + Think Time + Thresholds + Data Feeder/Correlation

```json
{
  "method": "GET",
  "url": "https://api.example.com/health",
  "expectedStatusCode": 200,
  "headers": {
    "Content-Type": "application/json"
  },
  "concurrency": 50,
  "testDurationSeconds": 40,
  "spawnRate": 10,
  "rampUpSeconds": 5,
  "rampDownSeconds": 5,
  "stages": [
    { "durationSeconds": 10, "targetConcurrency": 10, "spawnRate": 5 },
    { "durationSeconds": 20, "targetConcurrency": 50, "spawnRate": 10 },
    { "durationSeconds": 10, "targetConcurrency": 0, "spawnRate": 20 }
  ],
  "scenario": {
    "mode": "sequential",
    "tasks": [
      {
        "name": "login",
        "method": "POST",
        "url": "https://api.example.com/login",
        "requestBody": "{\"email\":\"{{email}}\",\"password\":\"{{password}}\"}",
        "expectedStatusCode": 200,
        "capture": {
          "token": "token",
          "userId": "user.id"
        }
      },
      {
        "name": "search",
        "method": "GET",
        "url": "https://api.example.com/search?q={{query}}",
        "expectedStatusCode": 200
      },
      {
        "name": "buy",
        "method": "POST",
        "url": "https://api.example.com/orders",
        "requestBody": "{\"sku\":\"{{sku}}\",\"qty\":1,\"token\":\"{{token}}\",\"userId\":\"{{userId}}\"}",
        "expectedStatusCode": 201
      }
    ]
  },
  "thinkTime": {
    "mode": "distribution",
    "distribution": "normal",
    "meanMs": 250,
    "stdDevMs": 80
  },
  "thresholds": {
    "maxP95LatencyMs": 800,
    "maxErrorRate": 2,
    "minRps": 50
  },
  "dataFeeder": {
    "file": "./data/users.json",
    "format": "json",
    "loop": true
  }
}
```
