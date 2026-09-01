package evidence

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/audit"
	"github.com/kyfd/changeguard/internal/model"
)

func linkedEvents(t *testing.T, events ...model.AuditEvent) []model.AuditEvent {
	t.Helper()
	out := make([]model.AuditEvent, 0, len(events))
	for _, event := range events {
		var previous *model.AuditEvent
		if len(out) > 0 {
			previous = &out[len(out)-1]
		}
		linked, err := audit.Link(event, previous)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, linked)
	}
	return out
}

func TestExportVerifyAndTampering(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "audit1", ChangeID: "chg", ActorID: "user", ActorName: "User", Action: "CREATE", Detail: "ok", CreatedAt: now})
	change := model.ChangeRequest{OrganizationID: "org", ID: "chg", ApplicationID: "app", Environment: "prod", ChangeType: "DATABASE", ArtifactSHA256: strings.Repeat("a", 64), RuleSetVersion: "rules", Status: model.StatusApproved, Version: 3, CreatedAt: now, UpdatedAt: now, Artifacts: []model.ChangeArtifact{{ID: "artifact", Kind: model.ArtifactDatabase, Name: "migration", Content: "password=secret", ContentSHA256: strings.Repeat("b", 64)}}}
	content, err := Export(Input{Change: change, Audits: chain, Passports: []model.Passport{{ID: "pass", ChangeID: "chg", TokenSHA256: "secret-token-hash"}}, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("password=secret")) || bytes.Contains(content, []byte("secret-token-hash")) {
		t.Fatal("bundle leaked artifact content or token hash")
	}
	if err := Verify(content); err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatal(err)
	}
	var evidenceChange ChangeEvidence
	if err := json.Unmarshal(bundle.Sections["change"], &evidenceChange); err != nil {
		t.Fatal(err)
	}
	evidenceChange.Environment = "tampered"
	bundle.Sections["change"], _ = json.Marshal(evidenceChange)
	tampered, _ := json.Marshal(bundle)
	if err := Verify(tampered); err == nil {
		t.Fatal("tampered section must fail manifest verification")
	}
}

func TestAuditProofHidesOtherChangesAndRejectsTampering(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	chain := linkedEvents(t,
		model.AuditEvent{OrganizationID: "org", ID: "other-audit", ChangeID: "other-change", ActorID: "other-customer", ActorName: "Other Customer", Action: "CREATE", Detail: "customer-secret-value", CreatedAt: now},
		model.AuditEvent{OrganizationID: "org", ID: "target-audit", ChangeID: "chg", ActorID: "u", Action: "APPROVE", Detail: "Bearer super-secret", CreatedAt: now.Add(time.Second)},
	)
	change := model.ChangeRequest{OrganizationID: "org", ID: "chg", ArtifactSHA256: strings.Repeat("c", 64)}
	content, err := Export(Input{Change: change, Audits: chain, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("other-change")) || bytes.Contains(content, []byte("customer-secret-value")) || bytes.Contains(content, []byte("Other Customer")) || bytes.Contains(content, []byte("super-secret")) {
		t.Fatal("per-change bundle leaked another change or audit free text")
	}
	if err := Verify(content); err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	_ = json.Unmarshal(content, &bundle)
	var proof []AuditProofNode
	_ = json.Unmarshal(bundle.Sections["audit_proof"], &proof)
	proof[1].CanonicalDigest = strings.Repeat("d", 64)
	proof[1].Hash = proof[1].CanonicalDigest
	bundle.Sections["audit_proof"], _ = json.Marshal(proof)
	for index := range bundle.Manifest {
		if bundle.Manifest[index].Name == "audit_proof" {
			bundle.Manifest[index].SHA256 = jsonDigest(bundle.Sections["audit_proof"])
		}
	}
	tampered, _ := json.Marshal(bundle)
	if err := Verify(tampered); err == nil {
		t.Fatal("tampered proof must fail even with recomputed manifest")
	}
}

func recomputeManifestItem(bundle *Bundle, name string) {
	for index := range bundle.Manifest {
		if bundle.Manifest[index].Name == name {
			bundle.Manifest[index].SHA256 = jsonDigest(bundle.Sections[name])
		}
	}
}

func TestAuditSummaryContainsOnlyChainBoundFields(t *testing.T) {
	now := time.Unix(150, 0).UTC()
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "target", ChangeID: "chg", ActorID: "u", Action: "APPROVE", Result: "SUCCESS", ReasonCode: "OK", Detail: "secret", CreatedAt: now})
	content, err := Export(Input{Change: model.ChangeRequest{OrganizationID: "org", ID: "chg", ArtifactSHA256: strings.Repeat("a", 64)}, Audits: chain, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(bundle.Sections["audits"], &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected one target audit, got %d", len(raw))
	}
	for key := range raw[0] {
		switch key {
		case "id", "hash", "prev_hash", "canonical_digest":
		default:
			t.Fatalf("unverified audit field %q must not be exported", key)
		}
	}
	for _, forbidden := range []string{"action", "result", "reason_code", "resource_version_before", "actor_type", "created_at"} {
		if _, exists := raw[0][forbidden]; exists {
			t.Fatalf("field %q must not exist", forbidden)
		}
	}
}

func TestAuditSummaryTamperingFailsAfterManifestRecomputed(t *testing.T) {
	now := time.Unix(175, 0).UTC()
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "target", ChangeID: "chg", ActorID: "u", Action: "APPROVE", CreatedAt: now})
	content, err := Export(Input{Change: model.ChangeRequest{OrganizationID: "org", ID: "chg", ArtifactSHA256: strings.Repeat("b", 64)}, Audits: chain, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*AuditSummary){
		func(item *AuditSummary) { item.ID = "tampered" },
		func(item *AuditSummary) { item.Hash = strings.Repeat("1", 64) },
		func(item *AuditSummary) { item.PrevHash = strings.Repeat("2", 64) },
		func(item *AuditSummary) { item.CanonicalDigest = strings.Repeat("3", 64) },
	}
	for index, mutate := range mutations {
		var bundle Bundle
		_ = json.Unmarshal(content, &bundle)
		var summaries []AuditSummary
		_ = json.Unmarshal(bundle.Sections["audits"], &summaries)
		mutate(&summaries[0])
		bundle.Sections["audits"], _ = json.Marshal(summaries)
		recomputeManifestItem(&bundle, "audits")
		tampered, _ := json.Marshal(bundle)
		if err := Verify(tampered); err == nil {
			t.Fatalf("chain-bound audit mutation %d must fail", index)
		}
	}
}

func TestExternalEnumFieldsUseStrictAllowlists(t *testing.T) {
	now := time.Unix(300, 0).UTC()
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "a", ChangeID: "chg", ActorID: "u", Action: "CREATE", CreatedAt: now})
	malicious := []string{
		"experiment-kind-token=secret-kind",
		"experiment-mode-token=secret-mode",
		"experiment-status-token=secret-status",
		"policy-code-token=secret-code",
		"artifact-kind-token=secret-artifact",
		"provider-token=secret-provider",
		"event-type-token=secret-event",
		"integration-status-token=secret-integration",
		"outcome-kind-token=secret-outcome-kind",
		"outcome-status-token=secret-outcome-status",
		"severity-token=secret-severity",
	}
	change := model.ChangeRequest{OrganizationID: "org", ID: "chg", ArtifactSHA256: strings.Repeat("f", 64), Experiment: &model.ExperimentReport{ID: "exp", Kind: malicious[0], Mode: malicious[1], Status: malicious[2]}}
	content, err := Export(Input{
		Change: change, Audits: chain, GeneratedAt: now,
		Policies:          []model.RiskPolicy{{ID: "policy", Code: malicious[3], ArtifactKinds: []string{malicious[4]}}},
		IntegrationEvents: []model.IntegrationEvent{{ID: "ci", ChangeID: "chg", Provider: malicious[5], EventType: malicious[6], Status: malicious[7]}},
		OutcomeSignals:    []model.OutcomeSignal{{ID: "out", ChangeID: "chg", Kind: model.OutcomeSignalKind(malicious[8]), Status: malicious[9], Severity: malicious[10]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, secret := range malicious {
		if strings.Contains(text, secret) {
			t.Fatalf("bundle leaked unknown enum %q", secret)
		}
	}
	if count := strings.Count(text, `"UNKNOWN"`); count < len(malicious) {
		t.Fatalf("expected at least %d UNKNOWN mappings, got %d", len(malicious), count)
	}
	if err := Verify(content); err != nil {
		t.Fatal(err)
	}
}

func TestKnownExternalEnumValuesArePreserved(t *testing.T) {
	now := time.Unix(400, 0).UTC()
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "a", ChangeID: "chg", ActorID: "u", Action: "CREATE", CreatedAt: now})
	change := model.ChangeRequest{OrganizationID: "org", ID: "chg", ArtifactSHA256: strings.Repeat("1", 64), Experiment: &model.ExperimentReport{ID: "exp", Kind: "SQL_SHADOW_EXPERIMENT", Mode: "POSTGRES", Status: "PASSED"}}
	content, err := Export(Input{
		Change: change, Audits: chain, GeneratedAt: now,
		Policies:          []model.RiskPolicy{{ID: "policy", Code: "DROP_TABLE", ArtifactKinds: []string{"DATABASE"}}},
		IntegrationEvents: []model.IntegrationEvent{{ID: "ci", ChangeID: "chg", Provider: "GITLAB", EventType: "PIPELINE", Status: "SUCCESS"}},
		OutcomeSignals:    []model.OutcomeSignal{{ID: "out", ChangeID: "chg", Kind: model.OutcomeSignalIncident, Status: "OPEN", Severity: "HIGH"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"SQL_SHADOW_EXPERIMENT", "POSTGRES", "PASSED", "DROP_TABLE", "DATABASE", "GITLAB", "PIPELINE", "SUCCESS", "INCIDENT", "OPEN", "HIGH"} {
		if !strings.Contains(string(content), value) {
			t.Fatalf("known enum %q was not preserved", value)
		}
	}
}

func TestStructuredSanitizationRemovesSecretsAndURLCredentials(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	chain := linkedEvents(t, model.AuditEvent{OrganizationID: "org", ID: "a", ChangeID: "chg", ActorID: "u", ActorName: "customer value", Action: "CREATE", Detail: "postgres://admin:pw@db/private?token=secret Bearer hidden", CreatedAt: now})
	change := model.ChangeRequest{OrganizationID: "org", ID: "chg", Environment: "prod", ArtifactSHA256: strings.Repeat("e", 64), Experiment: &model.ExperimentReport{ID: "exp", Status: "FAILED", ExecutionError: "postgres://user:password@db/x?secret=yes", Evidence: []model.Evidence{{Value: "customer-secret"}}}}
	content, err := Export(Input{
		Change: change, Audits: chain, GeneratedAt: now,
		IntegrationEvents: []model.IntegrationEvent{{ID: "ci", ChangeID: "chg", Provider: "gitlab", EventType: "pipeline", Status: "SUCCESS", ExternalURL: "https://user:pass@example.com/job/1?token=secret#fragment", Detail: "Bearer ci-secret", OccurredAt: now, ReceivedAt: now}},
		OutcomeSignals:    []model.OutcomeSignal{{ID: "out", ChangeID: "chg", Source: "ops", Kind: model.OutcomeSignalIncident, Status: "OPEN", ExternalURL: "https://customer:password@example.com/inc/1?auth=secret#x", Detail: "customer-secret-value", OccurredAt: now, ReceivedAt: now}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, secret := range []string{"password@", "token=secret", "auth=secret", "fragment", "Bearer ci-secret", "customer-secret-value", "customer-secret", "ExecutionError", "execution_error", "admin:pw", "user:pass"} {
		if strings.Contains(text, secret) {
			t.Fatalf("bundle leaked %q", secret)
		}
	}
	if !strings.Contains(text, "https://example.com/job/1") || !strings.Contains(text, "https://example.com/inc/1") {
		t.Fatal("sanitized URLs should retain scheme, host and path")
	}
	if err := Verify(content); err != nil {
		t.Fatal(err)
	}
}
