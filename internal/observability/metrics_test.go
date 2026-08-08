package observability

import (
	"net/http/httptest"
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
