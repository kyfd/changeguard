package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	duration time.Duration
	status   int
	err      error
}

type latencySummary struct {
	MinMS float64 `json:"min_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

type report struct {
	Requests        int             `json:"requests"`
	Success         int             `json:"success"`
	HTTPFailures    int             `json:"http_failures"`
	TransportErrors int             `json:"transport_errors"`
	Timeouts        int             `json:"timeouts"`
	ElapsedSeconds  float64         `json:"elapsed_seconds"`
	SuccessQPS      float64         `json:"success_qps"`
	ErrorRate       float64         `json:"error_rate"`
	Statuses        map[int]int     `json:"status_counts"`
	SuccessLatency  *latencySummary `json:"success_latency,omitempty"`
	AllLatency      *latencySummary `json:"all_attempt_latency,omitempty"`
}

// Measure through EOF, including body transfer and read failures, not just headers.
func measureRequest(client *http.Client, request *http.Request) result {
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return result{duration: time.Since(started), err: err}
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr == nil {
		readErr = closeErr
	}
	return result{duration: time.Since(started), status: response.StatusCode, err: readErr}
}

func summarize(results <-chan result, started time.Time) report {
	out := report{Statuses: make(map[int]int)}
	var successes, all []time.Duration
	for item := range results {
		out.Requests++
		all = append(all, item.duration)
		if item.status != 0 {
			out.Statuses[item.status]++
		}
		if item.err != nil {
			out.TransportErrors++
			var timeout net.Error
			if errors.As(item.err, &timeout) && timeout.Timeout() {
				out.Timeouts++
			}
		} else if item.status < 200 || item.status >= 300 {
			out.HTTPFailures++
		} else {
			out.Success++
			successes = append(successes, item.duration)
		}
	}
	out.ElapsedSeconds = time.Since(started).Seconds()
	if out.ElapsedSeconds > 0 {
		out.SuccessQPS = float64(out.Success) / out.ElapsedSeconds
	}
	if out.Requests > 0 {
		out.ErrorRate = float64(out.Requests-out.Success) / float64(out.Requests)
	}
	out.SuccessLatency = summarizeLatency(successes)
	out.AllLatency = summarizeLatency(all)
	return out
}

func summarizeLatency(values []time.Duration) *latencySummary {
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	ms := func(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }
	return &latencySummary{
		MinMS: ms(values[0]), P50MS: ms(percentile(values, 50)),
		P95MS: ms(percentile(values, 95)), P99MS: ms(percentile(values, 99)), MaxMS: ms(values[len(values)-1]),
	}
}

func loadtestEnv(name string) string {
	if value := strings.TrimSpace(os.Getenv("CHANGEGUARD_LOADTEST_" + name)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("DBGUARD_LOADTEST_" + name))
}

func main() {
	target := flag.String("url", "http://127.0.0.1:8080/api/dashboard", "target URL")
	concurrency := flag.Int("c", 32, "concurrent workers")
	duration := flag.Duration("d", 30*time.Second, "test duration")
	actor := flag.String("actor", "usr_developer", "X-Actor-ID for demo mode")
	email := flag.String("email", loadtestEnv("EMAIL"), "login email")
	password := flag.String("password", loadtestEnv("PASSWORD"), "login password; prefer CHANGEGUARD_LOADTEST_PASSWORD")
	loginURL := flag.String("login-url", "", "login endpoint; defaults to target origin + /api/auth/login")
	timeout := flag.Duration("timeout", 5*time.Second, "request timeout")
	jsonOutput := flag.Bool("json", false, "write one JSON report to stdout")
	flag.Parse()
	if *concurrency < 1 || *duration <= 0 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "c, d and timeout must be positive")
		os.Exit(2)
	}
	parsedTarget, err := url.Parse(*target)
	if err != nil || (parsedTarget.Scheme != "http" && parsedTarget.Scheme != "https") || parsedTarget.Host == "" || parsedTarget.User != nil {
		fmt.Fprintln(os.Stderr, "target must be an HTTP(S) URL without embedded credentials")
		os.Exit(2)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create cookie jar:", err)
		os.Exit(2)
	}
	client := &http.Client{Timeout: *timeout, Jar: jar, Transport: &http.Transport{MaxIdleConns: *concurrency * 2, MaxIdleConnsPerHost: *concurrency, IdleConnTimeout: 30 * time.Second}}
	if (*email == "") != (*password == "") {
		fmt.Fprintln(os.Stderr, "email and password must be provided together")
		os.Exit(2)
	}
	if *email != "" {
		endpoint := strings.TrimSpace(*loginURL)
		if endpoint == "" {
			targetURL, parseErr := url.Parse(*target)
			if parseErr != nil || targetURL.Scheme == "" || targetURL.Host == "" {
				fmt.Fprintln(os.Stderr, "invalid target URL")
				os.Exit(2)
			}
			endpoint = targetURL.Scheme + "://" + targetURL.Host + "/api/auth/login"
		}
		if err := authenticate(client, endpoint, *email, *password); err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			os.Exit(2)
		}
	}
	deadline := time.Now().Add(*duration)
	results := make(chan result, *concurrency*4)
	var workers sync.WaitGroup
	started := time.Now()
	for index := 0; index < *concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for time.Now().Before(deadline) {
				request, err := http.NewRequest(http.MethodGet, *target, nil)
				if err != nil {
					results <- result{err: err}
					return
				}
				if *email == "" {
					request.Header.Set("X-Actor-ID", *actor)
				}
				request.Header.Set("Accept", "application/json")
				results <- measureRequest(client, request)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()
	out := summarize(results, started)
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Printf("concurrency=%d elapsed=%.3fs latency_scope=response-body-eof\n", *concurrency, out.ElapsedSeconds)
	fmt.Printf("requests=%d success=%d http_failures=%d transport_errors=%d timeouts=%d error_rate=%.4f success_qps=%.2f status=%v\n",
		out.Requests, out.Success, out.HTTPFailures, out.TransportErrors, out.Timeouts, out.ErrorRate, out.SuccessQPS, out.Statuses)
	for _, series := range []struct {
		name  string
		value *latencySummary
	}{{"success_latency", out.SuccessLatency}, {"all_attempt_latency", out.AllLatency}} {
		if series.value != nil {
			v := series.value
			fmt.Printf("%s min=%.3fms p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms\n", series.name, v.MinMS, v.P50MS, v.P95MS, v.P99MS, v.MaxMS)
		}
	}
}

func authenticate(client *http.Client, endpoint, email, password string) error {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return nil
}

func percentile(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values) - 1) * percent / 100
	return values[index]
}
