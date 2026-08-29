package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPISpecLintAndRouteCoverage(t *testing.T) {
	specPath := filepath.Join("..", "..", "docs", "openapi.yaml")
	content, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read OpenAPI specification: %v", err)
	}
	spec := string(content)
	for _, required := range []string{
		"openapi: 3.1.0",
		"#/components/schemas/Error",
		"Idempotency-Key",
		"Idempotency-Replayed",
		"Idempotency-Status",
		"text/event-stream",
		"Cache-Control",
	} {
		if !strings.Contains(spec, required) {
			t.Errorf("OpenAPI specification is missing %q", required)
		}
	}

	serverSource, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read route source: %v", err)
	}
	routePattern := regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`)
	registered := map[string]bool{}
	for _, match := range routePattern.FindAllStringSubmatch(string(serverSource), -1) {
		registered[match[1]] = true
	}

	// This coverage gate intentionally tracks the public contract areas described
	// by this specification rather than unrelated health, metrics, configuration,
	// enterprise administration, or online-upgrade endpoints.
	exactRoutes := []string{
		"/api/auth/status", "/api/auth/me", "/api/auth/session", "/api/auth/register",
		"/api/auth/login", "/api/auth/invitations/accept", "/auth/login", "/auth/callback",
		"/auth/logout", "/api/changes", "/api/passports", "/api/gate/verify",
		"/api/gate/consume", "/api/integrations/gitlab/webhook", "/api/integrations/jenkins/events",
		"/api/integrations/operations/webhook", "/api/events", "/api/audits/export",
	}
	for _, route := range exactRoutes {
		if !registered[route] {
			t.Errorf("contract route %s is not registered", route)
		}
		if !strings.Contains(spec, "  "+route+":") {
			t.Errorf("registered contract route %s is absent from OpenAPI", route)
		}
	}

	for _, path := range []string{
		"/api/changes/{change_id}",
		"/api/changes/{change_id}/submit",
		"/api/changes/{change_id}/experiment",
		"/api/changes/{change_id}/approve",
		"/api/changes/{change_id}/reject",
		"/api/changes/{change_id}/comments",
		"/api/changes/{change_id}/findings/{finding_id}/{action}",
		"/api/changes/{change_id}/agent-ask",
		"/api/changes/{change_id}/agent-conversations",
		"/api/changes/{change_id}/agent-conversations/{conversation_id}",
		"/api/changes/{change_id}/passports",
		"/api/changes/{change_id}/passports/{passport_id}/revoke",
		"/api/changes/{change_id}/report",
	} {
		if !strings.Contains(spec, "  "+path+":") {
			t.Errorf("dynamic route %s is absent from OpenAPI", path)
		}
	}
	if !registered["/api/changes/"] {
		t.Error("dynamic change routes are not registered")
	}
}
