package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

var (
	ErrIdempotencyConflict       = errors.New("idempotency key request conflict")
	ErrIdempotencyPending        = errors.New("idempotency request is already in progress")
	ErrIdempotencyClaimLost      = errors.New("idempotency claim fenced by a newer generation")
	ErrIdempotencyResultConflict = errors.New("idempotency successful result conflict")
)

const (
	IdempotencyPending   = "PENDING"
	IdempotencySucceeded = "SUCCEEDED"
)

// BeginIdempotency atomically claims a scoped key. The scope deliberately
// includes organization, actor, operation and resource to prevent cross-tenant
// or cross-action response disclosure.
func (s *Store) BeginIdempotency(record model.IdempotencyRecord) (model.IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		claimed, created, payload, version, err := backend.beginIdempotency(ctx, record)
		if err != nil {
			return claimed, created, err
		}
		if created {
			if err := s.installPostgresSnapshot(payload, version); err != nil {
				return model.IdempotencyRecord{}, false, err
			}
		}
		return claimed, created, nil
	}
	for _, existing := range s.data.IdempotencyRecords {
		if !sameIdempotencyScope(existing, record) {
			continue
		}
		if existing.RequestDigest != record.RequestDigest {
			return publicIdempotencyRecord(existing), false, ErrIdempotencyConflict
		}
		if existing.Status == IdempotencySucceeded {
			return publicIdempotencyRecord(existing), false, nil
		}
		return publicIdempotencyRecord(existing), false, ErrIdempotencyPending
	}
	record.Status = IdempotencyPending
	record.Generation = 1
	if err := assignIdempotencyClaim(&record); err != nil {
		return model.IdempotencyRecord{}, false, err
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = record.CreatedAt
	previous := append([]model.IdempotencyRecord(nil), s.data.IdempotencyRecords...)
	s.data.IdempotencyRecords = append(s.data.IdempotencyRecords, record)
	if err := s.saveLocked(); err != nil {
		s.data.IdempotencyRecords = previous
		return model.IdempotencyRecord{}, false, err
	}
	return record, true, nil
}

// CompleteIdempotency saves a successful, non-secret replay snapshot.
func (s *Store) CompleteIdempotency(record model.IdempotencyRecord, result any, responseRef string, status int) (model.IdempotencyRecord, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return model.IdempotencyRecord{}, err
	}
	resultDigest := idempotencyResultDigest(encoded, responseRef, status)
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		completed, payload, version, err := backend.completeIdempotency(ctx, record, encoded, responseRef, status, resultDigest)
		if err != nil {
			return model.IdempotencyRecord{}, err
		}
		if len(payload) > 0 {
			if err := s.installPostgresSnapshot(payload, version); err != nil {
				return model.IdempotencyRecord{}, err
			}
		}
		return completed, nil
	}
	for index := range s.data.IdempotencyRecords {
		item := &s.data.IdempotencyRecords[index]
		if !sameIdempotencyScope(*item, record) || item.RequestDigest != record.RequestDigest {
			continue
		}
		if item.Status == IdempotencySucceeded {
			if item.ResponseRef != responseRef || item.HTTPStatus != status || item.ResultDigest == "" || subtle.ConstantTimeCompare([]byte(item.ResultDigest), []byte(resultDigest)) != 1 {
				return model.IdempotencyRecord{}, ErrIdempotencyResultConflict
			}
			return publicIdempotencyRecord(*item), nil
		}
		if !matchingIdempotencyClaim(*item, record) {
			return model.IdempotencyRecord{}, ErrIdempotencyClaimLost
		}
		previousRecords := append([]model.IdempotencyRecord(nil), s.data.IdempotencyRecords...)
		now := time.Now().UTC()
		item.Status = IdempotencySucceeded
		item.HTTPStatus = status
		item.Result = append(item.Result[:0], encoded...)
		item.ResultDigest = resultDigest
		item.ResponseRef = responseRef
		item.UpdatedAt = now
		item.CompletedAt = &now
		if err := s.saveLocked(); err != nil {
			s.data.IdempotencyRecords = previousRecords
			return model.IdempotencyRecord{}, err
		}
		return publicIdempotencyRecord(*item), nil
	}
	return model.IdempotencyRecord{}, ErrNotFound
}

// AbortIdempotency releases a failed claim so a retry can execute normally.
func (s *Store) AbortIdempotency(record model.IdempotencyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		payload, version, err := backend.abortIdempotency(ctx, record)
		if err != nil {
			return err
		}
		return s.installPostgresSnapshot(payload, version)
	}
	for index := range s.data.IdempotencyRecords {
		item := s.data.IdempotencyRecords[index]
		if !sameIdempotencyScope(item, record) || item.RequestDigest != record.RequestDigest || item.Status != IdempotencyPending {
			continue
		}
		if !matchingIdempotencyClaim(item, record) {
			return ErrIdempotencyClaimLost
		}
		previous := append([]model.IdempotencyRecord(nil), s.data.IdempotencyRecords...)
		s.data.IdempotencyRecords = append(s.data.IdempotencyRecords[:index], s.data.IdempotencyRecords[index+1:]...)
		if err := s.saveLocked(); err != nil {
			s.data.IdempotencyRecords = previous
			return err
		}
		return nil
	}
	return nil
}

// TakeoverIdempotency fences the previous pending executor and returns a new
// claim for reconciliation or retry. Only a caller holding the observed claim
// can rotate it, preventing concurrent reconcilers from sharing a generation.
func (s *Store) TakeoverIdempotency(observed model.IdempotencyRecord) (model.IdempotencyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		taken, payload, version, err := backend.takeoverIdempotency(ctx, observed)
		if err != nil {
			return model.IdempotencyRecord{}, err
		}
		if err := s.installPostgresSnapshot(payload, version); err != nil {
			return model.IdempotencyRecord{}, err
		}
		return taken, nil
	}
	for index := range s.data.IdempotencyRecords {
		item := &s.data.IdempotencyRecords[index]
		if !sameIdempotencyScope(*item, observed) || item.RequestDigest != observed.RequestDigest {
			continue
		}
		if item.Status != IdempotencyPending || observed.Generation == 0 || item.Generation != observed.Generation {
			return model.IdempotencyRecord{}, ErrIdempotencyClaimLost
		}
		previous := append([]model.IdempotencyRecord(nil), s.data.IdempotencyRecords...)
		item.Generation++
		if err := assignIdempotencyClaim(item); err != nil {
			return model.IdempotencyRecord{}, err
		}
		item.UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			s.data.IdempotencyRecords = previous
			return model.IdempotencyRecord{}, err
		}
		return *item, nil
	}
	return model.IdempotencyRecord{}, ErrNotFound
}

func assignIdempotencyClaim(record *model.IdempotencyRecord) error {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	record.ClaimToken = hex.EncodeToString(token[:])
	digest := sha256.Sum256([]byte(record.ClaimToken))
	record.ClaimTokenSHA256 = hex.EncodeToString(digest[:])
	return nil
}

func matchingIdempotencyClaim(stored, claimed model.IdempotencyRecord) bool {
	if stored.Generation == 0 || claimed.Generation != stored.Generation || claimed.ClaimToken == "" || stored.ClaimTokenSHA256 == "" {
		return false
	}
	digest := sha256.Sum256([]byte(claimed.ClaimToken))
	provided := hex.EncodeToString(digest[:])
	return subtle.ConstantTimeCompare([]byte(provided), []byte(stored.ClaimTokenSHA256)) == 1
}

func idempotencyResultDigest(result []byte, responseRef string, status int) string {
	digest := sha256.New()
	_, _ = digest.Write(result)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(responseRef))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(fmt.Sprintf("%d", status)))
	return hex.EncodeToString(digest.Sum(nil))
}

func publicIdempotencyRecord(record model.IdempotencyRecord) model.IdempotencyRecord {
	record.ClaimToken = ""
	return record
}

func sameIdempotencyScope(left, right model.IdempotencyRecord) bool {
	return left.OrganizationID == right.OrganizationID && left.ActorID == right.ActorID && left.Operation == right.Operation && left.Resource == right.Resource && left.Key == right.Key
}
