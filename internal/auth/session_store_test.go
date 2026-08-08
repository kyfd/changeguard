package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMemorySessionRepositoryRateLimit(t *testing.T) {
	repository := newMemorySessionRepository()
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		allowed, _, err := repository.AllowAttempt(ctx, "member@example.com", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != (index < 2) {
			t.Fatalf("attempt %d allowed=%t", index+1, allowed)
		}
	}
	if err := repository.ResetAttempts(ctx, "member@example.com"); err != nil {
		t.Fatal(err)
	}
	allowed, _, _ := repository.AllowAttempt(ctx, "member@example.com", 2, time.Minute)
	if !allowed {
		t.Fatal("reset did not clear attempts")
	}
}

func TestClientAddressOnlyTrustsConfiguredProxyHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	request.RemoteAddr = "192.0.2.10:4567"
	request.Header.Set("X-Real-IP", "198.51.100.20")

	t.Setenv("DBGUARD_TRUST_PROXY_HEADERS", "false")
	if actual := clientAddress(request); actual != "192.0.2.10" {
		t.Fatalf("untrusted proxy header changed client address: %q", actual)
	}
	t.Setenv("DBGUARD_TRUST_PROXY_HEADERS", "true")
	if actual := clientAddress(request); actual != "198.51.100.20" {
		t.Fatalf("trusted proxy header was not used: %q", actual)
	}
}
func TestMemorySessionRepositoryLoginFlowIsOneTime(t *testing.T) {
	repository := newMemorySessionRepository()
	ctx := context.Background()
	flow := loginFlow{Nonce: "nonce", CodeVerifier: "verifier", Next: "/#/dashboard", ExpiresAt: time.Now().Add(time.Minute)}
	if err := repository.PutLoginFlow(ctx, "state", flow, time.Minute); err != nil {
		t.Fatal(err)
	}
	taken, err := repository.TakeLoginFlow(ctx, "state")
	if err != nil {
		t.Fatal(err)
	}
	if taken.Nonce != flow.Nonce || taken.CodeVerifier != flow.CodeVerifier {
		t.Fatalf("unexpected flow: %+v", taken)
	}
	if _, err := repository.TakeLoginFlow(ctx, "state"); err == nil {
		t.Fatal("OIDC flow must be consumed atomically")
	}
}

func TestInvalidAuthModeFallsBackToSessionAuthentication(t *testing.T) {
	t.Setenv("DBGUARD_AUTH_MODE", "oidcc")
	config := FromEnvironment()
	if config.Mode != "local" {
		t.Fatalf("invalid auth mode must fall back to local, got %q", config.Mode)
	}
}
