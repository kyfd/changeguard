package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRuntimeUsesGuardedOutboundClient(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	for _, private := range []string{"false", "true"} {
		t.Run(private, func(t *testing.T) {
			t.Setenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM", private)
			runtime := NewFromEnvironment()
			defer runtime.client.CloseIdleConnections()
			_, _, _, err := runtime.completeOnce(context.Background(), nil, nil, LLMConfig{BaseURL: upstream.URL, APIKey: "test", Model: "test"})
			if err == nil || hits != 0 {
				t.Fatalf("err=%v target hits=%d", err, hits)
			}
		})
	}
}
