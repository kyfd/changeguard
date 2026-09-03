package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveHealthIncludesBuildProvenance(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	(&Server{}).handleLive(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Build   struct {
			Version            string `json:"version"`
			GoVersion          string `json:"go_version"`
			ProvenanceVerified bool   `json:"provenance_verified"`
		} `json:"build"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "ok" || payload.Service != "dbguard" || payload.Build.Version == "" || payload.Build.GoVersion == "" {
		t.Fatalf("health provenance is incomplete: %+v", payload)
	}
}

func TestDecodeJSONRejectsTrailingDocuments(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/test", strings.NewReader(`{"value":"first"} {"value":"second"}`))
	var target struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(request, &target); err == nil {
		t.Fatal("multiple JSON documents must be rejected")
	}
}

func TestFrontendEntersWorkspaceAfterLoginOrRegistration(t *testing.T) {
	content, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, marker := range []string{
		`state.session = session;`,
		`state.organization = session.organization;`,
		`location.hash = "#/dashboard";`,
		`location.reload();`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("identity success must persist the authenticated workspace before reloading: missing %q", marker)
		}
	}
	if !strings.Contains(script, `document.querySelector("#authGate .auth-message.error")?.remove();`) {
		t.Fatal("switching login/register tabs must clear stale errors")
	}
}

func TestFrontendKeepsDenseListsReadable(t *testing.T) {
	styles, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	stylesheet := string(styles)
	for _, rule := range []string{
		".data-table { width: 100%; min-width: 900px;",
		".dashboard-changes-panel .data-table { width: 100%; min-width: 0; table-layout: fixed; }",
		".dashboard-changes-panel .data-table td:nth-child(6) { display: none; }",
		".dashboard-changes-panel .change-title span { overflow: hidden; display: block; text-overflow: ellipsis; white-space: nowrap; }",
		".dashboard-release-item {",
		".incident-card {",
	} {
		if !strings.Contains(stylesheet, rule) {
			t.Fatalf("frontend stylesheet must preserve readable dense-list layout: missing %q", rule)
		}
	}

	script, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	javascript := string(script)
	if !strings.Contains(javascript, `class="risk risk-${cls}"`) {
		t.Fatal("risk values must render as semantic text badges")
	}
	if !strings.Contains(javascript, `<code>${escapeHTML(policy.code)}</code>`) {
		t.Fatal("policy codes inserted into dense tables must remain HTML-escaped")
	}
}

func TestFrontendProvidesReadableGovernancePanorama(t *testing.T) {
	scriptBytes, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, marker := range []string{
		`{ route: "panorama", label: "总览", icon: "activity" }`,
		"function panoramaSnapshot()",
		"function renderPanorama(main)",
		`setHeader("总览")`,
		`document.body.classList.toggle("panorama-mode", route === "panorama")`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("governance panorama implementation is incomplete: missing %q", marker)
		}
	}
	if !strings.Contains(script, "const changes = Array.isArray(state.changes) ? state.changes : [];") {
		t.Fatal("panorama metrics must be derived from the authenticated workspace state")
	}

	styleBytes, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	stylesheet := string(styleBytes)
	for _, marker := range []string{
		"body.panorama-mode { overflow: hidden;",
		"body.panorama-mode .sidebar, body.panorama-mode .topbar",
		".panorama-screen {",
		"@media (max-width: 1200px)",
		"@media (max-width: 640px)",
	} {
		if !strings.Contains(stylesheet, marker) {
			t.Fatalf("readable responsive panorama styles are incomplete: missing %q", marker)
		}
	}
}

func TestFrontendUsesSelfContainedEnterpriseShellAndAgentRuntimeView(t *testing.T) {
	indexBytes, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	index := string(indexBytes)
	for _, marker := range []string{`content="light"`, `href="/styles.css?v=20260812-clawbot"`, `id="authGate"`, `src="/ui-utils.mjs?v=20260829-accessibility"`} {
		if !strings.Contains(index, marker) {
			t.Fatalf("enterprise shell is incomplete: missing %q", marker)
		}
	}
	if strings.Contains(index, "static.cloudflareinsights.com") {
		t.Fatal("the origin HTML must not persist a CDN-injected analytics beacon")
	}

	scriptBytes, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, marker := range []string{
		`const icons = {`,
		`function renderIncidentBacktrace(main)`,
		`async function renderSettings(main)`,
		`/api/agent-runtime/summary`,
		`/api/agent-runtime/events?limit=20`,
		`metrics_state`,
		`data-agent-audit-export`,
		`changeguard-agent-audit-export/v1`,
		`document.body.append(anchor)`,
		`window.setTimeout(() =>`,
		`agent-runtime-panel`,
		`agent-runtime-slo`,
		`agent-runtime-events`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("enterprise application is incomplete: missing %q", marker)
		}
	}
}

func TestFrontendUsesResponsiveFullscreenPanorama(t *testing.T) {
	indexBytes, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	index := string(indexBytes)
	if strings.Contains(index, `href="/frontier.css"`) || strings.Contains(index, `href="/luminous.css"`) {
		t.Fatal("obsolete theme layers must not be loaded after the consolidated stylesheet")
	}

	styleBytes, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	stylesheet := string(styleBytes)
	for _, marker := range []string{
		`body.panorama-mode { overflow: hidden;`,
		`body.panorama-mode .app-shell, body.panorama-mode .workspace, body.panorama-mode .workspace-shell`,
		`.panorama-screen {`,
		`min-height: 100vh;`,
		`@media (max-width: 640px)`,
	} {
		if !strings.Contains(stylesheet, marker) {
			t.Fatalf("responsive fullscreen panorama is incomplete: missing %q", marker)
		}
	}

	scriptBytes, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, marker := range []string{
		`document.body.classList.toggle("panorama-mode", route === "panorama")`,
		`if (route === "panorama") rendered = renderPanorama(main);`,
		`<section class="panorama-screen panorama-screen-v2 panorama-screen-v3">`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("panorama fullscreen routing is incomplete: missing %q", marker)
		}
	}
}

func TestSecurityHeadersAndAuthNoStore(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	for header, expected := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
		"Cache-Control":          "no-store",
	} {
		if actual := response.Header().Get(header); actual != expected {
			t.Fatalf("%s = %q, want %q", header, actual, expected)
		}
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("content security policy must prevent framing")
	}
}

func TestDecodeJSONRejectsOversizedBodyEvenWithTrailingWhitespace(t *testing.T) {
	body := "{\"value\":\"ok\"}" + strings.Repeat(" ", int(maxJSONBodyBytes)+32)
	request := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(body))
	var target struct{ Value string }
	if err := decodeJSON(request, &target); err == nil || !strings.Contains(err.Error(), "2 MiB") {
		t.Fatalf("oversized body must be rejected, got %v", err)
	}
}

func TestMetricsEndpointIsNotPublic(t *testing.T) {
	t.Setenv("DBGUARD_METRICS_TOKEN", "")
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	server.handleMetrics(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("metrics without configured token must be hidden, got %d", response.Code)
	}
	t.Setenv("DBGUARD_METRICS_TOKEN", "secret-token")
	request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response = httptest.NewRecorder()
	server.handleMetrics(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without bearer token must be rejected, got %d", response.Code)
	}
}

func TestWriteEventStreamFlushesPayload(t *testing.T) {
	response := httptest.NewRecorder()
	if !writeEventStream(response, response, ": heartbeat\n\n") {
		t.Fatal("event stream write failed")
	}
	if response.Body.String() != ": heartbeat\n\n" || !response.Flushed {
		t.Fatalf("event stream was not written and flushed: body=%q flushed=%t", response.Body.String(), response.Flushed)
	}
}

func TestFrontendLocksFindingActionsAfterApproval(t *testing.T) {
	content, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	if !strings.Contains(script, `const evidenceLocked = ["APPROVED","COMPLETED"].includes(change.status);`) ||
		!strings.Contains(script, `if (canCoordinate && !evidenceLocked && status !== "VERIFIED")`) {
		t.Fatal("approved or completed findings must not expose mutation actions")
	}
	if strings.Contains(script, "data-change-action=\"complete\"") {
		t.Fatal("manual completion action must not be rendered")
	}
	if strings.Contains(script, `api("/api/passport/verify"`) || strings.Contains(script, `check_revocation: true`) {
		t.Fatal("frontend must not call the removed passport verification contract")
	}
	if !strings.Contains(script, `POST /api/gate/verify`) || !strings.Contains(script, `POST /api/gate/consume`) {
		t.Fatal("frontend must describe the implemented deployment gate endpoints")
	}
	if !strings.Contains(script, "${escapeHTML(initials(item.submitter_name))}") {
		t.Fatal("avatar text inserted into HTML must be escaped")
	}
}

func TestGateRequiresStrictBearerAuthorization(t *testing.T) {
	server := &Server{}
	for _, header := range []string{"", "Token abc", "Bearer", "Basic abc"} {
		request := httptest.NewRequest(http.MethodPost, "/api/gate/verify", strings.NewReader("{\"artifact_sha256\":\"abc\",\"environment\":\"生产环境\"}"))
		request.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		server.handleGateVerify(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("header %q must be rejected before service invocation, got %d", header, response.Code)
		}
	}
}

func TestGateRejectsTokenInJSONBody(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/gate/verify", strings.NewReader("{\"token\":\"secret\",\"artifact_sha256\":\"abc\",\"environment\":\"生产环境\"}"))
	request.Header.Set("Authorization", "Bearer header-token")
	response := httptest.NewRecorder()
	server.handleGateVerify(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("token in JSON must be rejected as an unknown field, got %d", response.Code)
	}
}
