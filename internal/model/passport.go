package model

import "time"

type CheckRun struct {
	ID             string    `json:"id"`
	ArtifactSHA256 string    `json:"artifact_sha256"`
	RuleSetVersion string    `json:"rule_set_version"`
	Status         string    `json:"status"`
	Findings       int       `json:"findings"`
	Blocking       int       `json:"blocking"`
	CheckedAt      time.Time `json:"checked_at"`
}

type PassportStatus string

const (
	PassportActive   PassportStatus = "ACTIVE"
	PassportConsumed PassportStatus = "CONSUMED"
	PassportRevoked  PassportStatus = "REVOKED"
	PassportExpired  PassportStatus = "EXPIRED"
)

// Passport is the persisted, non-secret record of a short-lived production
// change authorization. The bearer token is returned only when issued and is
// never stored; TokenSHA256 binds a presented token to this record.
type Passport struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	ChangeID       string         `json:"change_id"`
	ArtifactSHA256 string         `json:"artifact_sha256"`
	Environment    string         `json:"environment"`
	RuleSetVersion string         `json:"rule_set_version"`
	ApproverID     string         `json:"approver_id"`
	ApproverName   string         `json:"approver_name"`
	Status         PassportStatus `json:"status"`
	TokenSHA256    string         `json:"-"`
	IssuedAt       time.Time      `json:"issued_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	RevokedAt      *time.Time     `json:"revoked_at,omitempty"`
	RevokedByID    string         `json:"revoked_by_id,omitempty"`
	ConsumedAt     *time.Time     `json:"consumed_at,omitempty"`
	ConsumedBy     string         `json:"consumed_by,omitempty"`
}

// StoredPassport preserves the token hash in the state backend without ever
// exposing it through API JSON.
type StoredPassport struct {
	Passport
	TokenSHA256Stored string `json:"token_sha256"`
}

type PassportCredential struct {
	Passport Passport `json:"passport"`
	Token    string   `json:"token"`
}

type IssuePassportInput struct {
	TTLSeconds int `json:"ttl_seconds"`
}

type GateRequest struct {
	Token          string `json:"-"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	Environment    string `json:"environment"`
	Consumer       string `json:"consumer,omitempty"`
}

type GateResult struct {
	Allowed  bool      `json:"allowed"`
	Code     string    `json:"code"`
	Reason   string    `json:"reason"`
	Passport *Passport `json:"passport,omitempty"`
	Replayed bool      `json:"-"`
}
