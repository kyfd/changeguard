package observability

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizedRouteBoundsUnknownCardinality(t *testing.T) {
	cases := map[string]string{
		"/random-asset-123":                    "/web",
		"/auth/random-token":                   "/auth/{unknown}",
		"/health/random-token":                 "/health/{unknown}",
		"/metrics/random-token":                "/metrics",
		"/api/not-a-route/123":                 "/api/{unknown}",
		"/api/changes/arbitrary-id/submit":     "/api/changes/{id}/{action}",
		"/api/apps/app_anything":               "/api/apps/{id}",
		"/api/enterprise/members/usr_anything": "/api/enterprise/members/{id}",
		"/api/gate/consume":                    "/api/gate/consume",
		"/api/integrations/gitlab/webhook":     "/api/integrations/gitlab",
	}
	for input, expected := range cases {
		if actual := normalizedRoute(input); actual != expected {
			t.Fatalf("normalizedRoute(%q)=%q want %q", input, actual, expected)
		}
	}
}

func TestResponseRecorderUnwrapsForResponseController(t *testing.T) {
	underlying := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: underlying}
	if recorder.Unwrap() != underlying {
		t.Fatal("response recorder must expose the underlying writer")
	}
}

func TestMetricsExposeHistogramAndReadiness(t *testing.T) {
	metrics := New()
	metrics.SetReadiness(true)
	handler := metrics.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}), log.New(io.Discard, "", 0))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/apps", nil))

	response := httptest.NewRecorder()
	metrics.Handler(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		`dbguard_http_request_duration_seconds_bucket{method="GET",route="/api/apps",status="201",le="+Inf"} 1`,
		"dbguard_http_request_duration_seconds_count{method=\"GET\",route=\"/api/apps\",status=\"201\"} 1",
		"dbguard_readiness 1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics output missing %q:\n%s", expected, body)
		}
	}
}
