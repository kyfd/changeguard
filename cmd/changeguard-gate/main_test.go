package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type gateFlowRunner struct{}

func (gateFlowRunner) Run(_ context.Context, change model.ChangeRequest) model.ExperimentReport {
	return model.ExperimentReport{ID: "exp_gate_flow", Mode: "POSTGRES", Status: "PASSED", RollbackVerified: true, ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: change.RuleSetVersion}
}

type gateFlowAnalyzer struct{}

func (gateFlowAnalyzer) Analyze(_ context.Context, change model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{Provider: "test", Risk: model.RiskHigh, Summary: "advisory only", GeneratedAt: time.Now()}
}

func TestGoldenConfigFlowBindsApprovalToActualCIFiles(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("g", 32))
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
		Title: "配置门禁黄金链路", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
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
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: ciDigest.ArtifactSHA256, Environment: ciDigest.Environment, Consumer: "ci-verify"}, false); err != nil {
		t.Fatalf("verify must accept the exact reviewed files: %v", err)
	}

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
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: ciDigest.ArtifactSHA256, Environment: ciDigest.Environment, Consumer: "ci-consume"}, true); err != nil {
		t.Fatalf("exact reviewed files must consume once: %v", err)
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: ciDigest.ArtifactSHA256, Environment: ciDigest.Environment, Consumer: "ci-replay"}, true); !errors.Is(err, service.ErrPassportReplay) {
		t.Fatalf("second consume must be rejected as replay, got %v", err)
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
