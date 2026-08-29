package observability

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type metricKey struct {
	Method string
	Route  string
	Status int
}

type requestMetric struct {
	Count    uint64
	Duration time.Duration
	Buckets  [8]uint64
}

var requestDurationBuckets = [...]time.Duration{
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	5 * time.Second,
}

type Metrics struct {
	mu        sync.RWMutex
	requests  map[metricKey]requestMetric
	readiness float64
}

func New() *Metrics { return &Metrics{requests: make(map[metricKey]requestMetric)} }

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseRecorder) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(payload)
}
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (m *Metrics) Middleware(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		duration := time.Since(started)
		route := normalizedRoute(r.URL.Path)
		key := metricKey{Method: r.Method, Route: route, Status: recorder.status}
		m.mu.Lock()
		value := m.requests[key]
		value.Count++
		value.Duration += duration
		for index, upperBound := range requestDurationBuckets {
			if duration <= upperBound {
				value.Buckets[index]++
			}
		}
		m.requests[key] = value

		m.mu.Unlock()
		entry, _ := json.Marshal(map[string]any{"kind": "http_access", "request_id": requestID, "method": r.Method, "path": r.URL.Path, "route": route, "status": recorder.status, "duration_ms": float64(duration.Microseconds()) / 1000, "remote": r.RemoteAddr})
		logger.Print(string(entry))
	})
}

func (m *Metrics) Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m.mu.RLock()
	keys := make([]metricKey, 0, len(m.requests))
	for key := range m.requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := keys[i].Method + keys[i].Route + strconv.Itoa(keys[i].Status)
		right := keys[j].Method + keys[j].Route + strconv.Itoa(keys[j].Status)
		return left < right
	})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintln(w, "# HELP dbguard_http_requests_total Total HTTP requests.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_http_requests_total counter")
	_, _ = fmt.Fprintln(w, "# HELP dbguard_http_request_duration_seconds HTTP request duration histogram.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_http_request_duration_seconds histogram")
	for _, key := range keys {
		value := m.requests[key]
		labels := fmt.Sprintf("method=%q,route=%q,status=%q", key.Method, key.Route, strconv.Itoa(key.Status))
		_, _ = fmt.Fprintf(w, "dbguard_http_requests_total{%s} %d\n", labels, value.Count)
		for index, upperBound := range requestDurationBuckets {
			_, _ = fmt.Fprintf(w, "dbguard_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, prometheusDuration(upperBound), value.Buckets[index])
		}
		_, _ = fmt.Fprintf(w, "dbguard_http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, "+Inf", value.Count)
		_, _ = fmt.Fprintf(w, "dbguard_http_request_duration_seconds_sum{%s} %.6f\n", labels, value.Duration.Seconds())
		_, _ = fmt.Fprintf(w, "dbguard_http_request_duration_seconds_count{%s} %d\n", labels, value.Count)
	}
	_, _ = fmt.Fprintln(w, "# HELP dbguard_readiness Whether the most recent readiness check succeeded.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_readiness gauge")
	_, _ = fmt.Fprintf(w, "dbguard_readiness %.0f\n", m.readiness)
	m.mu.RUnlock()
}

func (m *Metrics) SetReadiness(ready bool) {
	m.mu.Lock()
	if ready {
		m.readiness = 1
	} else {
		m.readiness = 0
	}
	m.mu.Unlock()
}

func prometheusDuration(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', -1, 64)
}

func normalizedRoute(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	if parts[0] != "api" {
		switch parts[0] {
		case "auth":
			if len(parts) == 1 {
				return "/auth"
			}
			switch parts[1] {
			case "login", "callback", "logout":
				return "/auth/" + parts[1]
			default:
				return "/auth/{unknown}"
			}
		case "health":
			if len(parts) == 1 {
				return "/health"
			}
			switch parts[1] {
			case "live", "ready":
				return "/health/" + parts[1]
			default:
				return "/health/{unknown}"
			}
		case "metrics":
			return "/metrics"
		default:
			return "/web"
		}
	}
	if len(parts) < 2 {
		return "/api"
	}
	known := map[string]bool{
		"health": true, "auth": true, "users": true, "apps": true, "dashboard": true,
		"changes": true, "policies": true, "audits": true, "events": true,
		"config": true, "enterprise": true, "operations": true, "outbox": true,
		"conflicts": true, "gate": true, "integrations": true, "passports": true,
	}
	if !known[parts[1]] {
		return "/api/{unknown}"
	}
	switch parts[1] {
	case "changes", "apps", "policies", "outbox":
		if len(parts) > 2 {
			parts[2] = "{id}"
		}
		if len(parts) > 3 {
			parts[3] = "{action}"
			parts = parts[:4]
		}
	case "enterprise":
		if len(parts) > 3 && (parts[2] == "members" || parts[2] == "invites") {
			parts[3] = "{id}"
			parts = parts[:4]
		} else if len(parts) > 3 {
			parts = parts[:3]
		}
	case "gate", "integrations":
		if len(parts) > 2 {
			parts = parts[:3]
		}
	default:
		if len(parts) > 2 {
			parts = parts[:2]
		}
	}
	return "/" + strings.Join(parts, "/")
}
func newRequestID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(value)
}
