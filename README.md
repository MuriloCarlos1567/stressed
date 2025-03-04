# Stressed

Stressed is a high-performance load testing tool written in Go. It leverages the [fasthttp](https://github.com/valyala/fasthttp) library to achieve superior concurrency and minimal overhead. The tool provides a web-based frontend to configure tests, view real-time results, validate API responses, and manage test history.

## Main Features

- **Load Testing Engine**  
  Run performance tests with configurable concurrency and duration. It measures key metrics like success rate, average latency, 95th percentile latency, total requests, and requests per second.

- **API Validation**  
  Use the "Validate Test" feature to perform a single API request and view its response (including status code, headers, and body) in a modal. This helps verify if the test parameters are correct before running a full load test.

- **Test History**  
  Automatically saves test results locally in a JSON file. You can view the history, import a test configuration, and delete individual tests (using a unique UUID) or clear the entire history.

- **CLI Configuration**  
  Start the server using command-line flags to specify the host and port. For example, run:  
  ```bash
  go run main.go --host 127.0.0.1 --port 8000
  ```
  
- **Redirect Handling**  
  Supports following HTTP redirects automatically.

- **Multi-Method Support**  
  Works with various HTTP methods (GET, POST, PUT, DELETE, PATCH), including sending request bodies and custom headers.

- **Modern Frontend**  
  The web interface provides an intuitive and responsive design with consistent styling across forms, modals, and result displays.

## Getting Started

### Prerequisites

- Go (version 1.16 or later)
- Git

### Running the Server

Clone the repository and run the server using:

```bash
git clone https://github.com/MuriloCarlos1567/stressed.git
cd stressed
go run main.go --host 127.0.0.1 --port 8000
```

This will start the server at `http://127.0.0.1:8000` and serve the frontend from the `public` directory.

### Using the Application

1. **Configure a Test:**  
   Fill in the form with the API endpoint, HTTP method, expected status code, request body, headers, concurrency, and test duration.

2. **Validate Test:**  
   Click the "Validate Test" button to perform a single request to the configured API. The response (status code, headers, and body) is displayed in a modal for review.

3. **Run Test:**  
   Click the "Start Test" button to run the load test. Results are displayed upon completion, including key metrics such as latency, success rate, and throughput.

4. **Save & Manage History:**  
   Optionally, save the test result. The test history is available for import, deletion (by UUID), or clearing all records.

---
