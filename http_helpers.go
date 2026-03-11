package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

func newFastHTTPClient() *fasthttp.Client {
	return &fasthttp.Client{
		ReadTimeout:        requestTimeout,
		WriteTimeout:       requestTimeout,
		MaxConnWaitTimeout: requestTimeout,
	}
}

func writeJSON(ctx *fasthttp.RequestCtx, statusCode int, payload interface{}) {
	ctx.SetStatusCode(statusCode)
	ctx.Response.Header.Set("Content-Type", "application/json")

	body, err := json.Marshal(payload)
	if err != nil {
		ctx.Error("Error encoding response: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}
	ctx.SetBody(body)
}

func buildTaskRequest(req *fasthttp.Request, task compiledTask) {
	req.Reset()
	req.SetRequestURI(task.URL)
	req.Header.SetMethod(task.Method)
	req.Header.Set("Accept", "*/*")

	if task.RequestBody != "" && methodAllowsBody(task.Method) {
		req.SetBodyString(task.RequestBody)
	}
	for key, value := range task.Headers {
		req.Header.Set(key, value)
	}
}

func doRequestWithRedirects(client *fasthttp.Client, req *fasthttp.Request, resp *fasthttp.Response, timeout time.Duration) error {
	for redirects := 0; redirects <= maxRedirects; redirects++ {
		if err := client.DoTimeout(req, resp, timeout); err != nil {
			return err
		}

		status := resp.StatusCode()
		if status < 300 || status > 399 {
			return nil
		}
		if redirects == maxRedirects {
			return fmt.Errorf("too many redirects")
		}

		location := strings.TrimSpace(string(resp.Header.Peek("Location")))
		if location == "" {
			return nil
		}

		nextURL, err := resolveRedirectURL(string(req.URI().FullURI()), location)
		if err != nil {
			return err
		}

		if status == fasthttp.StatusSeeOther {
			req.Header.SetMethod(fasthttp.MethodGet)
			req.SetBody(nil)
		}

		req.SetRequestURI(nextURL)
		resp.Reset()
	}
	return nil
}

func resolveRedirectURL(currentURL, location string) (string, error) {
	current, err := url.Parse(currentURL)
	if err != nil {
		return "", fmt.Errorf("invalid current URL: %w", err)
	}
	target, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("invalid redirect location: %w", err)
	}
	return current.ResolveReference(target).String(), nil
}
