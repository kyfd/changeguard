package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

// CanonicalPayload is the stable representation covered by AuditEvent.Hash.
// Hash itself is intentionally excluded; all fields are explicit so additions to
// the API schema cannot silently change existing hashes.
type CanonicalPayload struct {
	OrganizationID        string    `json:"organization_id"`
	ID                    string    `json:"id"`
	ChangeID              string    `json:"change_id,omitempty"`
	RequestID             string    `json:"request_id,omitempty"`
	ActorID               string    `json:"actor_id"`
	ActorName             string    `json:"actor_name"`
	ActorType             string    `json:"actor_type,omitempty"`
	AuthMethod            string    `json:"auth_method,omitempty"`
	Action                string    `json:"action"`
	ResourceType          string    `json:"resource_type,omitempty"`
	ResourceID            string    `json:"resource_id,omitempty"`
	ResourceVersionBefore int       `json:"resource_version_before,omitempty"`
	ResourceVersionAfter  int       `json:"resource_version_after,omitempty"`
	RequestDigest         string    `json:"request_digest,omitempty"`
	Result                string    `json:"result,omitempty"`
	ReasonCode            string    `json:"reason_code,omitempty"`
	RelatedEventID        string    `json:"related_event_id,omitempty"`
	AttemptID             string    `json:"attempt_id,omitempty"`
	PassportID            string    `json:"passport_id,omitempty"`
	Detail                string    `json:"detail"`
	CreatedAt             time.Time `json:"created_at"`
	PrevHash              string    `json:"prev_hash,omitempty"`
}

func Normalize(event model.AuditEvent) model.AuditEvent {
	event.OrganizationID = strings.TrimSpace(event.OrganizationID)
	event.ID = strings.TrimSpace(event.ID)
	event.ChangeID = strings.TrimSpace(event.ChangeID)
	event.RequestID = strings.TrimSpace(event.RequestID)
	event.ActorID = strings.TrimSpace(event.ActorID)
	event.ActorName = strings.TrimSpace(event.ActorName)
	event.ActorType = strings.ToUpper(strings.TrimSpace(event.ActorType))
	event.AuthMethod = strings.ToUpper(strings.TrimSpace(event.AuthMethod))
	event.Action = strings.ToUpper(strings.TrimSpace(event.Action))
	event.ResourceType = strings.ToUpper(strings.TrimSpace(event.ResourceType))
	event.ResourceID = strings.TrimSpace(event.ResourceID)
	event.RequestDigest = strings.ToLower(strings.TrimSpace(event.RequestDigest))
	event.Result = strings.ToUpper(strings.TrimSpace(event.Result))
	event.ReasonCode = strings.ToUpper(strings.TrimSpace(event.ReasonCode))
	event.RelatedEventID = strings.TrimSpace(event.RelatedEventID)
	event.AttemptID = strings.TrimSpace(event.AttemptID)
	event.PassportID = strings.TrimSpace(event.PassportID)
	event.Detail = strings.TrimSpace(event.Detail)
	event.PrevHash = strings.ToLower(strings.TrimSpace(event.PrevHash))
	event.Hash = strings.ToLower(strings.TrimSpace(event.Hash))
	if event.ChangeID != "" {
		if event.ResourceType == "" {
			event.ResourceType = "CHANGE"
		}
		if event.ResourceID == "" {
			event.ResourceID = event.ChangeID
		}
	}
	if event.ActorType == "" {
		switch {
		case strings.HasPrefix(strings.ToLower(event.ActorID), "ci:"):
			event.ActorType = "CI"
		case strings.HasPrefix(strings.ToLower(event.ActorID), "system") || strings.HasPrefix(strings.ToLower(event.ActorID), "integration"):
			event.ActorType = "SYSTEM"
		default:
			event.ActorType = "USER"
		}
	}
	if event.Result == "" {
		event.Result = "SUCCESS"
	}
	return event
}

func Canonical(event model.AuditEvent) ([]byte, error) {
	event = Normalize(event)
	return json.Marshal(CanonicalPayload{
		OrganizationID: event.OrganizationID, ID: event.ID, ChangeID: event.ChangeID,
		RequestID: event.RequestID, ActorID: event.ActorID, ActorName: event.ActorName,
		ActorType: event.ActorType, AuthMethod: event.AuthMethod, Action: event.Action,
		ResourceType: event.ResourceType, ResourceID: event.ResourceID,
		ResourceVersionBefore: event.ResourceVersionBefore, ResourceVersionAfter: event.ResourceVersionAfter,
		RequestDigest: event.RequestDigest, Result: event.Result, ReasonCode: event.ReasonCode,
		RelatedEventID: event.RelatedEventID, AttemptID: event.AttemptID, PassportID: event.PassportID,
		Detail: event.Detail, CreatedAt: event.CreatedAt.UTC(), PrevHash: event.PrevHash,
	})
}

func Digest(event model.AuditEvent) (string, error) {
	payload, err := Canonical(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// Link prepares a new event for append after previous. A legacy previous event
// may have an empty Hash; its canonical digest is used as the transition anchor
// without rewriting old persisted JSON.
func Link(event model.AuditEvent, previous *model.AuditEvent) (model.AuditEvent, error) {
	event = Normalize(event)
	event.Hash = ""
	if previous == nil {
		event.PrevHash = ""
	} else if previous.Hash != "" {
		event.PrevHash = strings.ToLower(previous.Hash)
	} else {
		anchor, err := Digest(*previous)
		if err != nil {
			return model.AuditEvent{}, err
		}
		event.PrevHash = anchor
	}
	digest, err := Digest(event)
	if err != nil {
		return model.AuditEvent{}, err
	}
	event.Hash = digest
	return event, nil
}

func Verify(events []model.AuditEvent) error {
	last := make(map[string]model.AuditEvent)
	hashed := make(map[string]bool)
	for index, raw := range events {
		event := Normalize(raw)
		previous, hasPrevious := last[event.OrganizationID]
		if event.Hash == "" { // Legacy records are allowed only as an organization-chain prefix.
			if hashed[event.OrganizationID] {
				return fmt.Errorf("audit %s at index %d removes hash after hashed chain began", event.ID, index)
			}
			last[event.OrganizationID] = event
			continue
		}
		hashed[event.OrganizationID] = true
		expectedPrev := ""
		if hasPrevious {
			if previous.Hash != "" {
				expectedPrev = previous.Hash
			} else {
				var err error
				expectedPrev, err = Digest(previous)
				if err != nil {
					return err
				}
			}
		}
		if event.PrevHash != expectedPrev {
			return fmt.Errorf("audit %s at index %d has prev_hash %q, want %q", event.ID, index, event.PrevHash, expectedPrev)
		}
		digest, err := Digest(event)
		if err != nil {
			return err
		}
		if event.Hash != digest {
			return fmt.Errorf("audit %s at index %d hash mismatch", event.ID, index)
		}
		last[event.OrganizationID] = event
	}
	return nil
}
