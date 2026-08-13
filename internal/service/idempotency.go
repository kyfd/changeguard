package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

var (
	ErrIdempotencyConflict   = errors.New("幂等键已用于不同请求")
	ErrIdempotencyInProgress = errors.New("相同幂等请求正在处理中")
)

const PassportAlreadyIssuedCode = "PASSPORT_ALREADY_ISSUED_TOKEN_NOT_REPLAYABLE"

type PassportIssueResult struct {
	Credential *model.PassportCredential `json:"credential,omitempty"`
	Passport   *model.Passport           `json:"passport,omitempty"`
	Code       string                    `json:"code,omitempty"`
	Message    string                    `json:"message,omitempty"`
}

func (s *Service) QueueExperimentIdempotent(id, actorID, key, digest string) (model.ChangeRequest, bool, error) {
	return executeIdempotent(s, actorID, "QUEUE_EXPERIMENT", id, key, digest, http.StatusOK, func() (model.ChangeRequest, error) {
		return s.QueueExperiment(id, actorID)
	}, func(record model.IdempotencyRecord) (any, string, int, bool, error) {
		change, err := s.store.Change(id)
		if err != nil {
			return nil, "", 0, false, err
		}
		if change.Status != model.StatusExperimentQueued && change.Status != model.StatusExperimentRunning && change.Status != model.StatusWaitingApproval && change.Status != model.StatusApproved && change.Status != model.StatusCompleted {
			return nil, "", 0, false, nil
		}
		for _, event := range s.store.OutboxByOrganization(record.OrganizationID, true, 0) {
			if event.AggregateID == id && event.EventType == "experiment.requested" && !event.CreatedAt.Before(record.CreatedAt) {
				return change, "change:" + id, http.StatusOK, true, nil
			}
		}
		return nil, "", 0, false, nil
	})
}

func (s *Service) ApproveIdempotent(id, actorID, key, digest, comment string) (model.ChangeRequest, bool, error) {
	return executeIdempotent(s, actorID, "APPROVE", id, key, digest, http.StatusOK, func() (model.ChangeRequest, error) {
		return s.Approve(id, actorID, comment)
	}, func(record model.IdempotencyRecord) (any, string, int, bool, error) {
		change, err := s.store.Change(id)
		if err != nil {
			return nil, "", 0, false, err
		}
		if change.Status == model.StatusApproved && change.ReviewerID == record.ActorID && !change.UpdatedAt.Before(record.CreatedAt) {
			return change, "change:" + id, http.StatusOK, true, nil
		}
		return nil, "", 0, false, nil
	})
}

func (s *Service) IssuePassportIdempotent(changeID, actorID, key, digest string, ttlSeconds int) (PassportIssueResult, bool, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return PassportIssueResult{}, false, ErrForbidden
	}
	record := model.IdempotencyRecord{OrganizationID: actor.OrganizationID, ActorID: actor.ID, Operation: "ISSUE_PASSPORT", Resource: changeID, Key: key, RequestDigest: digest}
	s.recordIdempotencyAudit(actor, record, "IDEMPOTENCY_CLAIM")
	existing, claimed, err := s.claimIdempotency(record, func(pending model.IdempotencyRecord) (any, string, int, bool, error) {
		passports := s.store.PassportsByChange(pending.OrganizationID, changeID)
		for _, passport := range passports {
			if passport.ApproverID == pending.ActorID && !passport.IssuedAt.Before(pending.CreatedAt.Truncate(time.Second)) {
				return passport, "passport:" + passport.ID, http.StatusCreated, true, nil
			}
		}
		return nil, "", 0, false, nil
	})
	if err != nil {
		return PassportIssueResult{}, false, err
	}
	if claimed {
		record = existing
	}
	if !claimed {
		var passport model.Passport
		if err := json.Unmarshal(existing.Result, &passport); err != nil {
			return PassportIssueResult{}, true, err
		}
		return PassportIssueResult{Passport: &passport, Code: PassportAlreadyIssuedCode, Message: "通行证已签发；明文 Token 仅在首次成功响应中显示，不能重显或重新签发"}, true, nil
	}
	credential, err := s.IssuePassport(changeID, actorID, ttlSeconds)
	if err != nil {
		_ = s.store.AbortIdempotency(record)
		return PassportIssueResult{}, false, err
	}
	// Persist only public passport metadata and a resource reference. The bearer
	// token and even its hash never enter the idempotency record.
	if _, err := s.completeIdempotency(record, credential.Passport, "passport:"+credential.Passport.ID, http.StatusCreated); err != nil {
		return PassportIssueResult{}, false, err
	}
	return PassportIssueResult{Credential: &credential}, false, nil
}

func (s *Service) recordIdempotencyAudit(actor model.User, record model.IdempotencyRecord, action string) {
	event := audit(actor, record.Resource, action, "幂等请求 "+record.Operation)
	event.RequestID = record.Key
	event.RequestDigest = record.RequestDigest
	event.ResourceID = record.Resource
	_ = s.store.RecordAudit(event)
}

func executeIdempotent[T any](s *Service, actorID, operation, resource, key, digest string, status int, execute func() (T, error), reconcile func(model.IdempotencyRecord) (any, string, int, bool, error)) (T, bool, error) {
	var zero T
	actor, err := s.activeActor(actorID)
	if err != nil {
		return zero, false, ErrForbidden
	}
	record := model.IdempotencyRecord{OrganizationID: actor.OrganizationID, ActorID: actor.ID, Operation: operation, Resource: resource, Key: key, RequestDigest: digest}
	existing, claimed, err := s.claimIdempotency(record, reconcile)
	if err != nil {
		return zero, false, err
	}
	if claimed {
		record = existing
	}
	if !claimed {
		var result T
		if err := json.Unmarshal(existing.Result, &result); err != nil {
			return zero, true, err
		}
		return result, true, nil
	}
	result, err := execute()
	if err != nil {
		_ = s.store.AbortIdempotency(record)
		return zero, false, err
	}
	if _, err := s.completeIdempotency(record, result, "change:"+resource, status); err != nil {
		return zero, false, err
	}
	return result, false, nil
}

func (s *Service) completeIdempotency(record model.IdempotencyRecord, result any, responseRef string, status int) (model.IdempotencyRecord, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		completed, err := s.store.CompleteIdempotency(record, result, responseRef, status)
		if errors.Is(err, store.ErrConcurrentWrite) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return completed, err
	}
}

func (s *Service) claimIdempotency(record model.IdempotencyRecord, reconcile func(model.IdempotencyRecord) (any, string, int, bool, error)) (model.IdempotencyRecord, bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		existing, claimed, err := s.store.BeginIdempotency(record)
		switch {
		case err == nil:
			return existing, claimed, nil
		case errors.Is(err, store.ErrIdempotencyConflict):
			return model.IdempotencyRecord{}, false, ErrIdempotencyConflict
		case errors.Is(err, store.ErrConcurrentWrite) && time.Now().Before(deadline):
			time.Sleep(10 * time.Millisecond)
			continue
		case errors.Is(err, store.ErrIdempotencyPending):
			if reconcile != nil {
				result, responseRef, status, recovered, reconcileErr := reconcile(existing)
				if reconcileErr != nil {
					return model.IdempotencyRecord{}, false, reconcileErr
				}
				if recovered {
					takeover, takeoverErr := s.store.TakeoverIdempotency(existing)
					if errors.Is(takeoverErr, store.ErrIdempotencyClaimLost) && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					if takeoverErr != nil {
						return model.IdempotencyRecord{}, false, takeoverErr
					}
					completed, completeErr := s.completeIdempotency(takeover, result, responseRef, status)
					return completed, false, completeErr
				}
			}
			if time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return model.IdempotencyRecord{}, false, ErrIdempotencyInProgress
		default:
			return model.IdempotencyRecord{}, false, fmt.Errorf("claim idempotency key: %w", err)
		}
	}
}
