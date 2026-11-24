package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/httpapi"
	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

func writeTestManifest(t *testing.T, directory string, input manifest) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, ".changeguard.json")
	if err := os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestDigestFromManifestRejectsTraversal(t *testing.T) {
	manifestDir := t.TempDir()
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "secret.yaml")
	if err := os.WriteFile(externalFile, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(manifestDir, externalFile)
	if err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, manifestDir, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: filepath.ToSlash(relative)}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRejectsAbsolutePath(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(artifact, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: artifact}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("absolute artifact path must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRejectsOversizedArtifact(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "large.yaml")
	if err := os.WriteFile(artifact, bytes.Repeat([]byte("x"), int(maxArtifactFileBytes)+1), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: "large.yaml"}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized artifact must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRehashesActualFile(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(artifact, []byte("debug: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Name: "config", Path: "config.yaml"}}})
	first, err := digestFromManifest(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := digestFromManifest(filename)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArtifactSHA256 == second.ArtifactSHA256 {
		t.Fatal("CI digest must bind exact current repository bytes")
	}
}

func TestGitLabJobProofOptInAndRedirectRefusal(t *testing.T) {
	const passport, jobToken = "fixture-passport-secret", "fixture-job-secret"
	t.Setenv("CI_JOB_TOKEN", jobToken)
	t.Setenv("CI_JOB_ID", "1234")
	t.Setenv("CHANGEGUARD_TOKEN", passport)
	manifestFile := filepath.Join("..", "..", "examples", "ci-demo", ".changeguard.application.json")
	digest, err := digestFromManifest(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	var redirected, standardCalls, provedCalls atomic.Int32
	gate := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sink":
			redirected.Add(1)
		case "/redirect/api/gate/consume":
			http.Redirect(w, r, "/sink", http.StatusTemporaryRedirect)
		case "/unusable/api/gate/consume":
			_, _ = w.Write([]byte(`{"allowed":false,"code":"NOT_AUTHORIZED"}`))
		case "/api/gate/consume":
			if r.Header.Get("Authorization") != "Bearer "+passport {
				t.Error("invalid or missing passport header")
			}
			consumer := "1234"
			switch r.Header.Get(integration.GitLabJobTokenHeader) {
			case "":
				standardCalls.Add(1)
			case jobToken:
				provedCalls.Add(1)
				consumer = "gitlab-job:1234"
			default:
				t.Error("unexpected GitLab token header")
			}
			result := gateSuccessFixture(digest, consumer)
			result.Reason = passport + " / " + r.Header.Get(integration.GitLabJobTokenHeader)
			_ = json.NewEncoder(w).Encode(result)
		default:
			t.Errorf("unexpected gate path %s", r.URL.Path)
		}
	}))
	defer gate.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = gate.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	var out bytes.Buffer
	for _, opt := range [][]string{nil, {"-gitlab-job", "-consumer", "ignored-label"}} {
		out.Reset()
		if err := run(append([]string{"consume", "-manifest", manifestFile, "-url", gate.URL}, opt...), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), passport) || strings.Contains(out.String(), jobToken) {
			t.Fatal("Gate output leaked credentials")
		}
	}
	if standardCalls.Load() != 1 || provedCalls.Load() != 1 {
		t.Fatal("ordinary Gate must not forward GitLab tokens, and opt-in must prove the job")
	}
	if err := run([]string{"consume", "-manifest", manifestFile, "-gitlab-job", "-url", gate.URL + "/redirect"}, &out); err == nil || !strings.Contains(err.Error(), "redirect refused") || redirected.Load() != 0 {
		t.Fatalf("unexpected redirect result: %v", err)
	}
	if err := run([]string{"consume", "-manifest", manifestFile, "-gitlab-job", "-url", "http://127.0.0.1"}, &out); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("GitLab job proof must require HTTPS: %v", err)
	}
	if err := run([]string{"consume", "-manifest", manifestFile, "-gitlab-job", "-url", gate.URL + "/unusable"}, &out); err == nil || !strings.Contains(err.Error(), "no explicit authorization") {
		t.Fatalf("HTTP 200 alone is not Gate authorization: %v", err)
	}
	t.Setenv("CI_JOB_ID", "4321")
	out.Reset()
	if err := run([]string{"consume", "-manifest", manifestFile, "-gitlab-job", "-url", gate.URL}, &out); err == nil || out.Len() != 0 {
		t.Fatalf("consume accepted a different GitLab job: %v", err)
	}
	t.Setenv("CI_JOB_ID", "")
	if err := run([]string{"consume", "-manifest", manifestFile, "-gitlab-job", "-url", gate.URL}, &out); err == nil ||
		!strings.Contains(err.Error(), "CI_JOB_ID") || provedCalls.Load() != 2 {
		t.Fatalf("consume must not call Gate without an expected GitLab job ID: %v", err)
	}
}

func gateSuccessFixture(digest digestResult, consumer string) model.GateResult {
	issued := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	passport := &model.Passport{ID: "psp_fixture", OrganizationID: "org_fixture", ChangeID: "chg_fixture",
		ArtifactSHA256: digest.ArtifactSHA256, Environment: digest.Environment, RuleSetVersion: "fixture-v1",
		ApproverID: "usr_reviewer", ApproverName: "Reviewer", IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
		Status: model.PassportActive}
	if consumer != "" {
		at := issued.Add(time.Minute)
		passport.Status, passport.ConsumedBy, passport.ConsumedAt = model.PassportConsumed, consumer, &at
	}
	// Older valid responses omit artifact_version and never serialize Replayed.
	return model.GateResult{Allowed: true, Code: "GATE_ALLOWED", Reason: "fixture authorization", Passport: passport}
}

func TestGateRequiresUnambiguousOperationAndPassportBinding(t *testing.T) {
	manifestFile := filepath.Join("..", "..", "examples", "ci-demo", ".changeguard.application.json")
	digest, err := digestFromManifest(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(result model.GateResult, alter func(*model.GateResult)) string {
		t.Helper()
		if alter != nil {
			alter(&result)
		}
		body, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	active := encode(gateSuccessFixture(digest, ""), nil)
	consumed := func(alter func(*model.GateResult)) string {
		return encode(gateSuccessFixture(digest, "ci-gate"), alter)
	}
	valid := consumed(nil)
	for _, tc := range []struct {
		name, command, body string
		status              int
		allowed             bool
	}{
		{"legacy verify", "verify", active, http.StatusOK, true},
		{"legacy consume with default consumer", "consume", valid, http.StatusOK, true},
		{"missing passport on consume", "consume", `{"allowed":true,"code":"GATE_ALLOWED"}`, http.StatusOK, false},
		{"missing passport on verify", "verify", `{"allowed":true,"code":"GATE_ALLOWED"}`, http.StatusOK, false},
		{"ambiguous authorization", "consume", `{"Allowed":false,` + valid[1:], http.StatusOK, false},
		{"ambiguous passport status", "consume", strings.Replace(valid, `"status":`, `"status":"ACTIVE","status":`, 1), http.StatusOK, false},
		{"trailing JSON", "consume", valid + `{}`, http.StatusOK, false},
		{"HTML", "consume", "<html><body>OK</body></html>", http.StatusOK, false},
		{"allowed false", "consume", consumed(func(r *model.GateResult) { r.Allowed = false }), http.StatusOK, false},
		{"not completed", "consume", valid, http.StatusAccepted, false},
		{"verify response to consume", "consume", active, http.StatusOK, false},
		{"consume response to verify", "verify", valid, http.StatusOK, false},
		{"wrong digest", "consume", consumed(func(r *model.GateResult) { r.Passport.ArtifactSHA256 = strings.Repeat("f", 64) }), http.StatusOK, false},
		{"wrong environment", "consume", consumed(func(r *model.GateResult) { r.Passport.Environment = "unexpected-env" }), http.StatusOK, false},
		{"wrong consumer", "consume", consumed(func(r *model.GateResult) { r.Passport.ConsumedBy = "another-job" }), http.StatusOK, false},
		{"missing consumption time", "consume", consumed(func(r *model.GateResult) { r.Passport.ConsumedAt = nil }), http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/gate/"+tc.command {
					t.Errorf("unexpected Gate request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var out bytes.Buffer
			err := run([]string{tc.command, "-manifest", manifestFile, "-url", server.URL, "-token", "fixture-bearer", "-consumer", ""}, &out)
			if tc.allowed {
				if err != nil || out.String() != tc.body {
					t.Fatalf("expected unchanged success response, error=%v output=%q", err, out.String())
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatalf("unusable response must fail without output, error=%v output=%q", err, out.String())
			}
		})
	}
}

type gateFlowRunner struct{}

func (gateFlowRunner) Run(_ context.Context, change model.ChangeRequest) model.ExperimentReport {
	return model.ExperimentReport{ID: "exp_gate_flow", Mode: "POSTGRES", Status: "PASSED", RollbackVerified: true, ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: change.RuleSetVersion}
}

type gateFlowAnalyzer struct{}

func (gateFlowAnalyzer) Analyze(_ context.Context, change model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{Provider: "test", Risk: model.RiskHigh, Summary: "advisory only", GeneratedAt: time.Now()}
}

func TestGoldenConfigFlowBindsApprovalToActualCIFiles(t *testing.T) {
	t.Setenv("CHANGEGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("g", 32))
	directory := t.TempDir()
	artifactPath := filepath.Join(directory, "application.yaml")
	content := "debug: false\nauth_enabled: true\ntls_verify: true\n"
	if err := os.WriteFile(artifactPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := writeTestManifest(t, directory, manifest{
		Version: 1, Environment: "生产环境", ChangeType: "配置变更",
		Artifacts:    []manifestArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Source: "config/application.yaml", Language: "YAML", Path: "application.yaml"}},
		RollbackPlan: "恢复上一稳定版本配置并重新加载服务。",
	})
	ciDigest, err := digestFromManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	data := store.NewMemory()
	svc := service.New(data, gateFlowRunner{}, gateFlowAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "订单服务配置变更", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Source: "config/application.yaml", Language: "YAML", Content: content}},
		RollbackPlan: "恢复上一稳定版本配置并重新加载服务。",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"错误率"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if ciDigest.ArtifactSHA256 != change.ArtifactSHA256 {
		t.Fatalf("CI digest %s must match reviewed artifact %s", ciDigest.ArtifactSHA256, change.ArtifactSHA256)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil || change.Status != model.StatusWaitingApproval {
		t.Fatalf("safe config must wait for approval: status=%s err=%v", change.Status, err)
	}
	if _, err = svc.Approve(change.ID, "usr_developer", "self approve"); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("submitter self approval must fail, got %v", err)
	}
	change, err = svc.Approve(change.ID, "usr_reviewer", "确定性证据已核对")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := svc.IssuePassport(change.ID, "usr_reviewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	gate := httptest.NewServer(httpapi.New(svc, auth.New(auth.Config{Mode: "disabled"}, data, logger), logger))
	defer gate.Close()
	call := func(command, consumer string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run([]string{command, "-manifest", manifestPath, "-url", gate.URL, "-token", credential.Token, "-consumer", consumer}, &out); err != nil {
			t.Fatalf("%s must accept the exact reviewed files: %v", command, err)
		}
		return out.String()
	}
	call("verify", "ci-verify")

	if err := os.WriteFile(artifactPath, []byte(content+"request_timeout_ms: 5000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tampered, err := digestFromManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: tampered.ArtifactSHA256, Environment: tampered.Environment, Consumer: "ci-tampered"}, true); !errors.Is(err, service.ErrArtifactMismatch) {
		t.Fatalf("tampered file must not consume the passport, got %v", err)
	}
	if err := os.WriteFile(artifactPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	first := call("consume", "ci-consume")
	if replay := call("consume", "ci-consume"); replay != first {
		t.Fatal("same-consumer retry must return the original consumption record")
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: ciDigest.ArtifactSHA256, Environment: ciDigest.Environment, Consumer: "ci-replay"}, true); !errors.Is(err, service.ErrPassportReplay) {
		t.Fatalf("second consume from a different consumer must be rejected as replay, got %v", err)
	}
	completed, err := svc.Change(change.ID)
	if err != nil || completed.Status != model.StatusCompleted {
		t.Fatalf("consume must atomically complete the change: status=%s err=%v", completed.Status, err)
	}

	actions := map[string]bool{}
	for _, entry := range data.AuditsByChange(completed.OrganizationID, completed.ID) {
		actions[entry.Action] = true
	}
	for _, action := range []string{"SUBMIT_CHECK", "APPROVE", "PASSPORT_ISSUED", "PASSPORT_VERIFIED", "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED"} {
		if !actions[action] {
			t.Fatalf("golden evidence chain is missing audit action %s: %+v", action, actions)
		}
	}
}
