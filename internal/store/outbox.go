package store

import (
	"errors"
	"sort"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func (s *Store) UpdateChangeWithOutbox(id string, update func(*model.ChangeRequest) error, event model.OutboxEvent, audits ...model.AuditEvent) (model.ChangeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Changes {
		if s.data.Changes[index].ID != id {
			continue
		}
		if err := update(&s.data.Changes[index]); err != nil {
			return model.ChangeRequest{}, err
		}
		now := time.Now()
		s.data.Changes[index].UpdatedAt = now
		s.data.Changes[index].Version++
		if event.ID == "" {
			event.ID = NewID("out_")
		}
		if event.Status == "" {
			event.Status = model.OutboxPending
		}
		if event.MaxAttempts <= 0 {
			event.MaxAttempts = 5
		}
		if event.NextAttemptAt.IsZero() {
			event.NextAttemptAt = now
		}
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		event.UpdatedAt = now
		s.data.Outbox = append(s.data.Outbox, event)
		s.data.Audits = append(s.data.Audits, audits...)
		if err := s.saveLocked(); err != nil {
			return model.ChangeRequest{}, err
		}
		return s.data.Changes[index], nil
	}
	return model.ChangeRequest{}, ErrNotFound
}

func (s *Store) EnsureExperimentOutbox() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now()
	for _, change := range s.data.Changes {
		if change.Status != model.StatusExperimentQueued && change.Status != model.StatusExperimentRunning {
			continue
		}
		found := false
		for _, event := range s.data.Outbox {
			if event.EventType == "experiment.requested" && event.AggregateID == change.ID && event.Status != model.OutboxCompleted && event.Status != model.OutboxDead {
				found = true
				break
			}
		}
		if found {
			continue
		}
		s.data.Outbox = append(s.data.Outbox, model.OutboxEvent{
			ID: NewID("out_"), OrganizationID: change.OrganizationID, AggregateType: "change", AggregateID: change.ID,
			EventType: "experiment.requested", Status: model.OutboxPending, MaxAttempts: 5, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		})
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) ClaimOutbox(workerID string, lease time.Duration) (model.OutboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	selected := -1
	for index := range s.data.Outbox {
		event := s.data.Outbox[index]
		eligible := event.Status == model.OutboxPending || (event.Status == model.OutboxProcessing && event.LockedUntil != nil && now.After(*event.LockedUntil))
		if !eligible || now.Before(event.NextAttemptAt) {
			continue
		}
		if selected < 0 || event.NextAttemptAt.Before(s.data.Outbox[selected].NextAttemptAt) {
			selected = index
		}
	}
	if selected < 0 {
		return model.OutboxEvent{}, ErrNotFound
	}
	lockedUntil := now.Add(lease)
	event := &s.data.Outbox[selected]
	event.Status = model.OutboxProcessing
	event.Attempts++
	event.LockedBy = workerID
	event.LockedUntil = &lockedUntil
	event.UpdatedAt = now
	if err := s.saveLocked(); err != nil {
		return model.OutboxEvent{}, err
	}
	return *event, nil
}

func (s *Store) CompleteOutbox(id, workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for index := range s.data.Outbox {
		event := &s.data.Outbox[index]
		if event.ID != id {
			continue
		}
		if event.Status != model.OutboxProcessing || event.LockedBy != workerID {
			return ErrConcurrentWrite
		}
		event.Status = model.OutboxCompleted
		event.CompletedAt = &now
		event.LockedBy = ""
		event.LockedUntil = nil
		event.LastError = ""
		event.UpdatedAt = now
		return s.saveLocked()
	}
	return ErrNotFound
}

func (s *Store) RenewOutbox(id, workerID string, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for index := range s.data.Outbox {
		event := &s.data.Outbox[index]
		if event.ID != id {
			continue
		}
		if event.Status != model.OutboxProcessing || event.LockedBy != workerID {
			return ErrConcurrentWrite
		}
		lockedUntil := now.Add(lease)
		event.LockedUntil = &lockedUntil
		event.UpdatedAt = now
		return s.saveLocked()
	}
	return ErrNotFound
}

func (s *Store) FailOutbox(id, workerID string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for index := range s.data.Outbox {
		event := &s.data.Outbox[index]
		if event.ID != id {
			continue
		}
		if event.Status != model.OutboxProcessing || event.LockedBy != workerID {
			return ErrConcurrentWrite
		}
		event.LastError = cause.Error()
		event.LockedBy = ""
		event.LockedUntil = nil
		event.UpdatedAt = now
		if event.Attempts >= event.MaxAttempts {
			event.Status = model.OutboxDead
		} else {
			event.Status = model.OutboxPending
			delay := time.Duration(1<<minInt(event.Attempts, 6)) * time.Second
			event.NextAttemptAt = now.Add(delay)
		}
		return s.saveLocked()
	}
	return ErrNotFound
}

func (s *Store) RetryOutbox(organizationID, id string, audit ...model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for index := range s.data.Outbox {
		event := &s.data.Outbox[index]
		if event.ID != id || event.OrganizationID != organizationID {
			continue
		}
		if event.Status != model.OutboxDead {
			return errors.New("event is not dead")
		}
		event.Status = model.OutboxPending
		event.Attempts = 0
		event.LastError = ""
		event.NextAttemptAt = now
		event.UpdatedAt = now
		if len(audit) > 0 {
			entry := audit[0]
			entry.OrganizationID = organizationID
			if entry.ID == "" {
				entry.ID = NewID("audit_")
			}
			if entry.CreatedAt.IsZero() {
				entry.CreatedAt = now
			}
			s.data.Audits = append(s.data.Audits, entry)
		}
		return s.saveLocked()
	}
	return ErrNotFound
}

func (s *Store) OutboxByOrganization(organizationID string, includeCompleted bool, limit int) []model.OutboxEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.OutboxEvent, 0)
	for _, event := range s.data.Outbox {
		if event.OrganizationID == organizationID && (includeCompleted || event.Status != model.OutboxCompleted) {
			items = append(items, event)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) OperationsSummary(organizationID string) model.OperationsSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := model.OperationsSummary{StoreMode: s.Mode()}
	for _, event := range s.data.Outbox {
		if organizationID != "" && event.OrganizationID != organizationID {
			continue
		}
		switch event.Status {
		case model.OutboxPending:
			result.PendingEvents++
			if result.OldestPendingAt == nil || event.CreatedAt.Before(*result.OldestPendingAt) {
				created := event.CreatedAt
				result.OldestPendingAt = &created
			}
		case model.OutboxProcessing:
			result.ProcessingEvents++
		case model.OutboxDead:
			result.DeadEvents++
		}
	}
	return result
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
