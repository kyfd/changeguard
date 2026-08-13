package model

import (
	"encoding/json"
	"time"
)

// IdempotencyRecord persists the outcome of one mutating API request. Result is
// a bounded, non-secret response snapshot; sensitive credentials must use a
// safe resource reference instead.
type IdempotencyRecord struct {
	OrganizationID   string          `json:"organization_id"`
	ActorID          string          `json:"actor_id"`
	Operation        string          `json:"operation"`
	Resource         string          `json:"resource"`
	Key              string          `json:"key"`
	RequestDigest    string          `json:"request_digest"`
	Status           string          `json:"status"`
	Generation       uint64          `json:"generation"`
	ClaimToken       string          `json:"-"`
	ClaimTokenSHA256 string          `json:"claim_token_sha256,omitempty"`
	HTTPStatus       int             `json:"http_status,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	ResultDigest     string          `json:"result_digest,omitempty"`
	ResponseRef      string          `json:"response_ref,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
}
