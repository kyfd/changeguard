package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	duration time.Duration
	status   int
	err      error
}

func main() {
	target := flag.String("url", "http://127.0.0.1:8080/api/dashboard", "target URL")
	concurrency := flag.Int("c", 32, "concurrent workers")
	duration := flag.Duration("d", 30*time.Second, "test duration")
	actor := flag.String("actor", "usr_developer", "X-Actor-ID for demo mode")
	email := flag.String("email", strings.TrimSpace(os.Getenv("DBGUARD_LOADTEST_EMAIL")), "enterprise login email")
	password := flag.String("password", strings.TrimSpace(os.Getenv("DBGUARD_LOADTEST_PASSWORD")), "enterprise login password; prefer DBGUARD_LOADTEST_PASSWORD")
	loginURL := flag.String("login-url", "", "login endpoint; defaults to target origin + /api/auth/login")
	timeout := flag.Duration("timeout", 5*time.Second, "request timeout")
	flag.Parse()
	if *concurrency < 1 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "c and d must be positive")
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
	var sent atomic.Uint64
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
				requestStarted := time.Now()
				response, requestErr := client.Do(request)
				elapsed := time.Since(requestStarted)
				if requestErr != nil {
					results <- result{duration: elapsed, err: requestErr}
					continue
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				sent.Add(1)
				results <- result{duration: elapsed, status: response.StatusCode}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()
	latencies := make([]time.Duration, 0, 100000)
	statuses := make(map[int]int)
	errorsCount := 0
	for item := range results {
		if item.err != nil {
			errorsCount++
			continue
		}
		statuses[item.status]++
		if item.status < 200 || item.status >= 300 {
			errorsCount++
			continue
		}
		latencies = append(latencies, item.duration)
	}
	elapsed := time.Since(started)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	fmt.Printf("target=%s concurrency=%d elapsed=%s\n", *target, *concurrency, elapsed.Round(time.Millisecond))
	success := len(latencies)
	fmt.Printf("requests=%d success=%d errors=%d success_qps=%.2f status=%v\n", sent.Load(), success, errorsCount, float64(success)/elapsed.Seconds(), statuses)
	if len(latencies) > 0 {
		fmt.Printf("latency min=%s p50=%s p95=%s p99=%s max=%s\n", latencies[0], percentile(latencies, 50), percentile(latencies, 95), percentile(latencies, 99), latencies[len(latencies)-1])
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
