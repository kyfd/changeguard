package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/audit"
	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
)

const (
	Format      = "changeguard-evidence/v1"
	unknownEnum = "UNKNOWN"
)

var (
	allowedExperimentKinds       = enumSet("DATABASE_SHADOW_EXPERIMENT", "SQL_SHADOW_EXPERIMENT", "STATIC_GATE", "DEMO_ONLY")
	allowedExperimentModes       = enumSet("POSTGRES", "DEMO_ONLY", "DETERMINISTIC")
	allowedExperimentStatuses    = enumSet("PASSED", "FAILED", "NOT_RUN", "RUNNING")
	allowedArtifactKinds         = enumSet(string(model.ArtifactDatabase), string(model.ArtifactConfig), string(model.ArtifactKubernetes), string(model.ArtifactCode), string(model.ArtifactAPI))
	allowedIntegrationProviders  = enumSet("GITLAB", "JENKINS")
	allowedIntegrationEventTypes = enumSet("PIPELINE", "BUILD")
	allowedIntegrationStatuses   = enumSet("CREATED", "PENDING", "PREPARING", "WAITING_FOR_RESOURCE", "RUNNING", "SUCCESS", "SUCCEEDED", "PASSED", "FAILED", "FAILURE", "CANCELED", "CANCELLED", "SKIPPED", "MANUAL", "BLOCKED")
	allowedOutcomeStatuses       = enumSet("OPEN", "TRIGGERED", "ACKNOWLEDGED", "RESOLVED", "CLOSED", "STARTED", "SUCCEEDED", "FAILED", "CANCELED", "CANCELLED", "OBSERVED")
	allowedOutcomeSeverities     = enumSet("CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO", "WARNING")
	allowedPolicyCodes           = builtinPolicyCodeSet()
)

type Binding struct {
	OrganizationID string `json:"organization_id"`
	ChangeID       string `json:"change_id"`
	ChangeDigest   string `json:"change_digest"`
}

type ManifestItem struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type Bundle struct {
	Format      string                     `json:"format"`
	GeneratedAt time.Time                  `json:"generated_at"`
	Binding     Binding                    `json:"binding"`
	Sections    map[string]json.RawMessage `json:"sections"`
	Manifest    []ManifestItem             `json:"manifest"`
}

type ArtifactSummary struct {
	ID            string             `json:"id"`
	Kind          model.ArtifactKind `json:"kind"`
	ContentSHA256 string             `json:"content_sha256"`
}

type FindingSummary struct {
	ID          string              `json:"id"`
	Code        string              `json:"code"`
	Severity    model.RiskLevel     `json:"severity"`
	Blocking    bool                `json:"blocking"`
	RuleVersion int                 `json:"rule_version"`
	Status      model.FindingStatus `json:"status"`
}

type ExperimentSummary struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind,omitempty"`
	Mode               string    `json:"mode"`
	Status             string    `json:"status"`
	StartedAt          time.Time `json:"started_at"`
	FinishedAt         time.Time `json:"finished_at"`
	DurationMS         int64     `json:"duration_ms"`
	DatasetRows        int64     `json:"dataset_rows"`
	LockWaitMS         int64     `json:"lock_wait_ms"`
	P99BeforeMS        float64   `json:"p99_before_ms"`
	P99AfterMS         float64   `json:"p99_after_ms"`
	FailedTransactions int       `json:"failed_transactions"`
	RollbackVerified   bool      `json:"rollback_verified"`
	ChecksTotal        int       `json:"checks_total,omitempty"`
	ChecksPassed       int       `json:"checks_passed,omitempty"`
	CanaryPercent      int       `json:"canary_percent,omitempty"`
	ObservationMinutes int       `json:"observation_minutes,omitempty"`
	ArtifactSHA256     string    `json:"artifact_sha256,omitempty"`
	RuleSetVersion     string    `json:"rule_set_version,omitempty"`
	AttemptID          string    `json:"attempt_id,omitempty"`
	LeaseGeneration    uint64    `json:"lease_generation,omitempty"`
	InputSHA256        string    `json:"input_sha256,omitempty"`
	ResultDigest       string    `json:"result_digest,omitempty"`
}

type ChangeEvidence struct {
	OrganizationID string             `json:"organization_id"`
	ID             string             `json:"id"`
	ApplicationID  string             `json:"application_id"`
	Environment    string             `json:"environment"`
	ChangeType     string             `json:"change_type"`
	ArtifactSHA256 string             `json:"artifact_sha256"`
	SQLSHA256      string             `json:"sql_sha256,omitempty"`
	RollbackSHA256 string             `json:"rollback_sha256,omitempty"`
	RuleSetVersion string             `json:"rule_set_version,omitempty"`
	Status         model.ChangeStatus `json:"status"`
	Risk           model.RiskLevel    `json:"risk"`
	Version        int                `json:"version"`
	Artifacts      []ArtifactSummary  `json:"artifacts"`
	CheckRun       *model.CheckRun    `json:"check_run,omitempty"`
	Findings       []FindingSummary   `json:"findings"`
	Experiment     *ExperimentSummary `json:"experiment,omitempty"`
	ReviewerID     string             `json:"reviewer_id,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

type RuleSummary struct {
	ID            string          `json:"id"`
	Code          string          `json:"code"`
	Severity      model.RiskLevel `json:"severity"`
	Blocking      bool            `json:"blocking"`
	Enabled       bool            `json:"enabled"`
	Builtin       bool            `json:"builtin"`
	Version       int             `json:"version"`
	ArtifactKinds []string        `json:"artifact_kinds,omitempty"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type PassportSummary struct {
	ID             string               `json:"id"`
	OrganizationID string               `json:"organization_id"`
	ChangeID       string               `json:"change_id"`
	ArtifactSHA256 string               `json:"artifact_sha256"`
	Environment    string               `json:"environment"`
	RuleSetVersion string               `json:"rule_set_version"`
	ApproverID     string               `json:"approver_id"`
	Status         model.PassportStatus `json:"status"`
	IssuedAt       time.Time            `json:"issued_at"`
	ExpiresAt      time.Time            `json:"expires_at"`
	RevokedAt      *time.Time           `json:"revoked_at,omitempty"`
	RevokedByID    string               `json:"revoked_by_id,omitempty"`
	ConsumedAt     *time.Time           `json:"consumed_at,omitempty"`
}

type IntegrationSummary struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	EventType   string    `json:"event_type"`
	Status      string    `json:"status"`
	ChangeID    string    `json:"change_id"`
	CommitSHA   string    `json:"commit_sha,omitempty"`
	ExternalURL string    `json:"external_url,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	ReceivedAt  time.Time `json:"received_at"`
}

type OutcomeSummary struct {
	ID          string                  `json:"id"`
	Kind        model.OutcomeSignalKind `json:"kind"`
	Status      string                  `json:"status"`
	ChangeID    string                  `json:"change_id"`
	Severity    string                  `json:"severity,omitempty"`
	ExternalURL string                  `json:"external_url,omitempty"`
	OccurredAt  time.Time               `json:"occurred_at"`
	ReceivedAt  time.Time               `json:"received_at"`
}

// AuditSummary contains only facts directly duplicated in the chain proof. It
// intentionally carries no business semantics: a digest cannot prove a redacted
// projection of Action, Result, actor, versions, or other canonical fields.
type AuditSummary struct {
	ID              string `json:"id"`
	PrevHash        string `json:"prev_hash,omitempty"`
	Hash            string `json:"hash,omitempty"`
	CanonicalDigest string `json:"canonical_digest"`
}

// AuditProofNode proves append-order continuity without disclosing another
// change's event body. CanonicalDigest is computed from the original event.
type AuditProofNode struct {
	ID              string `json:"id"`
	Hash            string `json:"hash,omitempty"`
	PrevHash        string `json:"prev_hash,omitempty"`
	CanonicalDigest string `json:"canonical_digest"`
	Target          bool   `json:"target,omitempty"`
}

type Input struct {
	Change            model.ChangeRequest
	Policies          []model.RiskPolicy
	Passports         []model.Passport
	IntegrationEvents []model.IntegrationEvent
	OutcomeSignals    []model.OutcomeSignal
	Audits            []model.AuditEvent // complete organization chain in append order
	GeneratedAt       time.Time
}

func Export(input Input) ([]byte, error) {
	if input.Change.ID == "" || input.Change.OrganizationID == "" || input.Change.ArtifactSHA256 == "" {
		return nil, errors.New("change binding is incomplete")
	}
	if input.GeneratedAt.IsZero() {
		input.GeneratedAt = time.Now().UTC()
	}
	audits, proof, err := buildAuditEvidence(input.Change, input.Audits)
	if err != nil {
		return nil, err
	}
	sections := make(map[string]json.RawMessage, 7)
	values := map[string]any{
		"change":      sanitizeChange(input.Change),
		"rules":       sanitizePolicies(input.Policies),
		"passports":   sanitizePassports(input.Passports),
		"audit_proof": proof,
		"audits":      audits,
		"ci_events_outcomes": struct {
			Events   []IntegrationSummary `json:"events"`
			Outcomes []OutcomeSummary     `json:"outcomes"`
		}{sanitizeIntegrations(input.IntegrationEvents), sanitizeOutcomes(input.OutcomeSignals)},
	}
	for name, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		sections[name] = encoded
	}
	binding := Binding{OrganizationID: input.Change.OrganizationID, ChangeID: input.Change.ID, ChangeDigest: input.Change.ArtifactSHA256}
	manifest := make([]ManifestItem, 0, len(sections)+2)
	bindingBytes, _ := json.Marshal(binding)
	manifest = append(manifest, ManifestItem{Name: "binding", SHA256: digest(bindingBytes)})
	generatedBytes, _ := json.Marshal(input.GeneratedAt.UTC())
	manifest = append(manifest, ManifestItem{Name: "generated_at", SHA256: digest(generatedBytes)})
	for name, content := range sections {
		manifest = append(manifest, ManifestItem{Name: name, SHA256: jsonDigest(content)})
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Name < manifest[j].Name })
	bundle := Bundle{Format: Format, GeneratedAt: input.GeneratedAt.UTC(), Binding: binding, Sections: sections, Manifest: manifest}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func Verify(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return fmt.Errorf("decode bundle: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return err
	}
	if bundle.Format != Format || bundle.Binding.OrganizationID == "" || bundle.Binding.ChangeID == "" || bundle.Binding.ChangeDigest == "" {
		return errors.New("invalid bundle binding or format")
	}
	expectedNames := map[string]bool{"binding": true, "generated_at": true, "change": true, "rules": true, "passports": true, "ci_events_outcomes": true, "audits": true, "audit_proof": true}
	if len(bundle.Manifest) != len(expectedNames) || len(bundle.Sections) != len(expectedNames)-2 {
		return errors.New("manifest item set is incomplete")
	}
	actual := make(map[string]string, len(expectedNames))
	bindingBytes, _ := json.Marshal(bundle.Binding)
	actual["binding"] = digest(bindingBytes)
	generatedBytes, _ := json.Marshal(bundle.GeneratedAt.UTC())
	actual["generated_at"] = digest(generatedBytes)
	for name, section := range bundle.Sections {
		if !expectedNames[name] {
			return fmt.Errorf("unexpected section %q", name)
		}
		actual[name] = jsonDigest(section)
	}
	seen := make(map[string]bool, len(bundle.Manifest))
	for _, item := range bundle.Manifest {
		if !expectedNames[item.Name] || seen[item.Name] || item.SHA256 != actual[item.Name] {
			return fmt.Errorf("manifest verification failed for %q", item.Name)
		}
		seen[item.Name] = true
	}
	var change ChangeEvidence
	if err := strictSection(bundle.Sections["change"], &change); err != nil {
		return fmt.Errorf("verify change section: %w", err)
	}
	if change.ID != bundle.Binding.ChangeID || change.OrganizationID != bundle.Binding.OrganizationID || change.ArtifactSHA256 != bundle.Binding.ChangeDigest {
		return errors.New("change and manifest binding mismatch")
	}
	var audits []AuditSummary
	if err := strictSection(bundle.Sections["audits"], &audits); err != nil {
		return fmt.Errorf("verify audits section: %w", err)
	}
	var proof []AuditProofNode
	if err := strictSection(bundle.Sections["audit_proof"], &proof); err != nil {
		return fmt.Errorf("verify audit proof: %w", err)
	}
	if err := verifyAuditProof(bundle.Binding.ChangeID, audits, proof); err != nil {
		return err
	}
	return nil
}

func buildAuditEvidence(change model.ChangeRequest, chain []model.AuditEvent) ([]AuditSummary, []AuditProofNode, error) {
	if err := audit.Verify(chain); err != nil {
		return nil, nil, fmt.Errorf("source audit chain: %w", err)
	}
	summaries := make([]AuditSummary, 0)
	proof := make([]AuditProofNode, 0, len(chain))
	for _, event := range chain {
		if event.OrganizationID != change.OrganizationID {
			return nil, nil, errors.New("audit chain contains another organization")
		}
		canonicalDigest, err := audit.Digest(event)
		if err != nil {
			return nil, nil, err
		}
		target := event.ChangeID == change.ID || (event.ResourceType == "CHANGE" && event.ResourceID == change.ID)
		proof = append(proof, AuditProofNode{ID: event.ID, Hash: event.Hash, PrevHash: event.PrevHash, CanonicalDigest: canonicalDigest, Target: target})
		if target {
			summaries = append(summaries, AuditSummary{ID: event.ID, PrevHash: event.PrevHash, Hash: event.Hash, CanonicalDigest: canonicalDigest})
		}
	}
	return summaries, proof, nil
}

func verifyAuditProof(_ string, audits []AuditSummary, proof []AuditProofNode) error {
	targets := make(map[string]AuditSummary, len(audits))
	for _, event := range audits {
		if event.ID == "" || event.CanonicalDigest == "" || targets[event.ID].ID != "" {
			return errors.New("invalid or duplicate target audit")
		}
		targets[event.ID] = event
	}
	seenTarget := make(map[string]bool, len(targets))
	previousAnchor := ""
	hashed := false
	for index, node := range proof {
		if node.ID == "" || node.CanonicalDigest == "" {
			return fmt.Errorf("invalid audit proof node at index %d", index)
		}
		if node.Hash == "" {
			if hashed || node.PrevHash != "" || node.CanonicalDigest == "" {
				return fmt.Errorf("invalid legacy audit proof node %s", node.ID)
			}
			previousAnchor = node.CanonicalDigest
		} else {
			hashed = true
			if node.PrevHash != previousAnchor || node.Hash != node.CanonicalDigest {
				return fmt.Errorf("audit proof chain mismatch at %s", node.ID)
			}
			previousAnchor = node.Hash
		}
		if node.Target {
			event, ok := targets[node.ID]
			if !ok || event.Hash != node.Hash || event.PrevHash != node.PrevHash || event.CanonicalDigest != node.CanonicalDigest {
				return fmt.Errorf("target audit %s is not embedded in proof", node.ID)
			}
			seenTarget[node.ID] = true
		} else if _, leaked := targets[node.ID]; leaked {
			return fmt.Errorf("audit %s target marker mismatch", node.ID)
		}
	}
	if len(seenTarget) != len(targets) {
		return errors.New("audit proof does not embed every target event")
	}
	return nil
}

func sanitizeChange(change model.ChangeRequest) ChangeEvidence {
	artifacts := make([]ArtifactSummary, 0, len(change.Artifacts))
	for _, item := range change.Artifacts {
		artifacts = append(artifacts, ArtifactSummary{ID: item.ID, Kind: item.Kind, ContentSHA256: item.ContentSHA256})
	}
	findings := make([]FindingSummary, 0, len(change.Findings))
	for _, item := range change.Findings {
		findings = append(findings, FindingSummary{ID: item.ID, Code: item.Code, Severity: item.Severity, Blocking: item.Blocking, RuleVersion: item.RuleVersion, Status: item.Status})
	}
	return ChangeEvidence{OrganizationID: change.OrganizationID, ID: change.ID, ApplicationID: change.ApplicationID, Environment: changegate.Redact(change.Environment), ChangeType: change.ChangeType, ArtifactSHA256: change.ArtifactSHA256, SQLSHA256: change.SQLSHA256, RollbackSHA256: change.RollbackSHA256, RuleSetVersion: change.RuleSetVersion, Status: change.Status, Risk: change.Risk, Version: change.Version, Artifacts: artifacts, CheckRun: change.CheckRun, Findings: findings, Experiment: sanitizeExperiment(change.Experiment), ReviewerID: change.ReviewerID, CreatedAt: change.CreatedAt, UpdatedAt: change.UpdatedAt}
}

func sanitizeExperiment(report *model.ExperimentReport) *ExperimentSummary {
	if report == nil {
		return nil
	}
	return &ExperimentSummary{ID: report.ID, Kind: allowEnum(report.Kind, allowedExperimentKinds), Mode: allowEnum(report.Mode, allowedExperimentModes), Status: allowEnum(report.Status, allowedExperimentStatuses), StartedAt: report.StartedAt, FinishedAt: report.FinishedAt, DurationMS: report.DurationMS, DatasetRows: report.DatasetRows, LockWaitMS: report.LockWaitMS, P99BeforeMS: report.P99BeforeMS, P99AfterMS: report.P99AfterMS, FailedTransactions: report.FailedTransactions, RollbackVerified: report.RollbackVerified, ChecksTotal: report.ChecksTotal, ChecksPassed: report.ChecksPassed, CanaryPercent: report.CanaryPercent, ObservationMinutes: report.ObservationMinutes, ArtifactSHA256: report.ArtifactSHA256, RuleSetVersion: report.RuleSetVersion, AttemptID: report.AttemptID, LeaseGeneration: report.LeaseGeneration, InputSHA256: report.InputSHA256, ResultDigest: report.ResultDigest}
}

func sanitizePolicies(items []model.RiskPolicy) []RuleSummary {
	out := make([]RuleSummary, 0, len(items))
	for _, item := range items {
		artifactKinds := make([]string, 0, len(item.ArtifactKinds))
		for _, kind := range item.ArtifactKinds {
			artifactKinds = append(artifactKinds, allowEnum(kind, allowedArtifactKinds))
		}
		out = append(out, RuleSummary{ID: item.ID, Code: allowEnum(item.Code, allowedPolicyCodes), Severity: item.Severity, Blocking: item.Blocking, Enabled: item.Enabled, Builtin: item.Builtin, Version: item.Version, ArtifactKinds: artifactKinds, UpdatedAt: item.UpdatedAt})
	}
	return out
}

func sanitizePassports(items []model.Passport) []PassportSummary {
	out := make([]PassportSummary, 0, len(items))
	for _, item := range items {
		out = append(out, PassportSummary{ID: item.ID, OrganizationID: item.OrganizationID, ChangeID: item.ChangeID, ArtifactSHA256: item.ArtifactSHA256, Environment: changegate.Redact(item.Environment), RuleSetVersion: item.RuleSetVersion, ApproverID: item.ApproverID, Status: item.Status, IssuedAt: item.IssuedAt, ExpiresAt: item.ExpiresAt, RevokedAt: item.RevokedAt, RevokedByID: item.RevokedByID, ConsumedAt: item.ConsumedAt})
	}
	return out
}

func sanitizeIntegrations(items []model.IntegrationEvent) []IntegrationSummary {
	out := make([]IntegrationSummary, 0, len(items))
	for _, item := range items {
		out = append(out, IntegrationSummary{ID: item.ID, Provider: allowEnum(item.Provider, allowedIntegrationProviders), EventType: allowEnum(item.EventType, allowedIntegrationEventTypes), Status: allowEnum(item.Status, allowedIntegrationStatuses), ChangeID: item.ChangeID, CommitSHA: item.CommitSHA, ExternalURL: sanitizeURL(item.ExternalURL), OccurredAt: item.OccurredAt, ReceivedAt: item.ReceivedAt})
	}
	return out
}

func sanitizeOutcomes(items []model.OutcomeSignal) []OutcomeSummary {
	out := make([]OutcomeSummary, 0, len(items))
	for _, item := range items {
		out = append(out, OutcomeSummary{ID: item.ID, Kind: model.OutcomeSignalKind(allowEnum(string(item.Kind), enumSet(string(model.OutcomeSignalIncident), string(model.OutcomeSignalRollback), string(model.OutcomeSignalBusinessSLI)))), Status: allowEnum(item.Status, allowedOutcomeStatuses), ChangeID: item.ChangeID, Severity: allowOptionalEnum(item.Severity, allowedOutcomeSeverities), ExternalURL: sanitizeURL(item.ExternalURL), OccurredAt: item.OccurredAt, ReceivedAt: item.ReceivedAt})
	}
	return out
}

func enumSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[strings.ToUpper(strings.TrimSpace(value))] = struct{}{}
	}
	return result
}

func allowEnum(value string, allowed map[string]struct{}) string {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if _, ok := allowed[normalized]; ok {
		return normalized
	}
	return unknownEnum
}

func allowOptionalEnum(value string, allowed map[string]struct{}) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return allowEnum(value, allowed)
}

func builtinPolicyCodeSet() map[string]struct{} {
	policies := model.DefaultRiskPolicies(time.Unix(0, 0).UTC())
	codes := make(map[string]struct{}, len(policies))
	for _, policy := range policies {
		codes[policy.Code] = struct{}{}
	}
	return codes
}

func sanitizeURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return changegate.Redact(parsed.String())
}

func jsonDigest(content []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		return ""
	}
	return digest(compact.Bytes())
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func strictSection(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureEOF(decoder)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
