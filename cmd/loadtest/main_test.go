package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMeasureRequestIncludesBodyTransfer(t *testing.T) {
	const bodyDelay = 40 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(bodyDelay)
		_, _ = w.Write([]byte("done"))
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	got := measureRequest(server.Client(), request)
	if got.err != nil || got.status != http.StatusOK || got.duration < bodyDelay {
		t.Fatalf("measurement must include delayed body: %+v", got)
	}
}

func TestMeasureRequestRejectsTruncatedSuccessfulResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	got := measureRequest(server.Client(), request)
	if got.status != http.StatusOK || !errors.Is(got.err, io.ErrUnexpectedEOF) {
		t.Fatalf("a 200 with an incomplete body is a failed request: %+v", got)
	}
}

func TestSummarizeIncludesFailedAttempts(t *testing.T) {
	items := make(chan result, 5)
	items <- result{status: 200, duration: time.Millisecond}
	items <- result{status: 200, duration: 3 * time.Millisecond}
	items <- result{status: 503, duration: 5 * time.Millisecond}
	items <- result{status: 200, err: io.ErrUnexpectedEOF, duration: 7 * time.Millisecond}
	items <- result{err: context.DeadlineExceeded, duration: 9 * time.Millisecond}
	close(items)
	got := summarize(items, time.Now().Add(-time.Second))
	if got.Requests != 5 || got.Success != 2 || got.HTTPFailures != 1 || got.TransportErrors != 2 || got.Timeouts != 1 {
		t.Fatalf("wrong request accounting: %+v", got)
	}
	if got.Statuses[200] != 3 || got.Statuses[503] != 1 || got.ErrorRate != 0.6 {
		t.Fatalf("headers, body failures and timeouts must remain visible: %+v", got)
	}
	if got.SuccessLatency == nil || got.SuccessLatency.MaxMS != 3 || got.AllLatency == nil || got.AllLatency.MaxMS != 9 {
		t.Fatalf("success and all-attempt latency must be separate: %+v", got)
	}
}

func TestSummarizeEmptyRunHasNoInventedLatency(t *testing.T) {
	items := make(chan result)
	close(items)
	got := summarize(items, time.Now())
	if got.Requests != 0 || got.SuccessLatency != nil || got.AllLatency != nil || got.ErrorRate != 0 {
		t.Fatalf("empty run: %+v", got)
	}
}

func TestLoadtestEnvironmentPrefersCanonicalName(t *testing.T) {
	t.Setenv("CHANGEGUARD_LOADTEST_EMAIL", "current@example.com")
	t.Setenv("DBGUARD_LOADTEST_EMAIL", "legacy@example.com")
	if got := loadtestEnv("EMAIL"); got != "current@example.com" {
		t.Fatalf("canonical value = %q", got)
	}
	t.Setenv("CHANGEGUARD_LOADTEST_EMAIL", "")
	if got := loadtestEnv("EMAIL"); got != "legacy@example.com" {
		t.Fatalf("legacy fallback = %q", got)
	}
}

func TestAuthenticateStoresSessionCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbguard_session", Value: "test-session", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/dashboard":
			cookie, err := r.Cookie("dbguard_session")
			if err != nil || cookie.Value != "test-session" {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	if err := authenticate(client, server.URL+"/api/auth/login", "owner@example.com", "owner123"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/dashboard", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated client did not reuse session cookie: %d", response.StatusCode)
	}
}
