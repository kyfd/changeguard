package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"sort"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
)

var (
	ErrPassportExpired       = errors.New("passport expired")
	ErrPassportInactive      = errors.New("passport inactive")
	ErrPassportTokenMismatch = errors.New("passport token mismatch")
	ErrPassportChangeInvalid = errors.New("passport change is not approved")
)

func (s *Store) CreatePassport(passport model.Passport, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, item := range s.data.Passports {
		if item.ID == passport.ID {
			return ErrConflict
		}
		if item.OrganizationID == passport.OrganizationID && item.ChangeID == passport.ChangeID && item.Status == model.PassportActive && now.Before(item.ExpiresAt) {
			return ErrConflict
		}
	}
	oldPassports := append([]model.StoredPassport(nil), s.data.Passports...)
	oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
	for index := range s.data.Passports {
		item := &s.data.Passports[index]
		if item.OrganizationID != passport.OrganizationID || item.ChangeID != passport.ChangeID || item.Status != model.PassportActive || now.Before(item.ExpiresAt) {
			continue
		}
		item.Status = model.PassportExpired
		expiryAudit := model.AuditEvent{
			OrganizationID: item.OrganizationID,
			ChangeID:       item.ChangeID,
			ActorID:        "system",
			ActorName:      "ChangeGuard",
			ActorType:      "SYSTEM",
			Action:         "PASSPORT_EXPIRED",
			PassportID:     item.ID,
			Detail:         "签发新通行证前物化既有通行证自然过期状态",
			CreatedAt:      now,
		}
		s.appendAuditsLocked(expiryAudit)
	}
	stored := model.StoredPassport{Passport: passport, TokenSHA256Stored: passport.TokenSHA256}
	stored.Passport.TokenSHA256 = ""
	s.data.Passports = append(s.data.Passports, stored)
	s.appendAuditsLocked(audit)
	if err := s.saveLocked(); err != nil {
		s.data.Passports = oldPassports
		s.data.Audits = oldAudits
		return err
	}
	return nil
}

func (s *Store) Passport(id string) (model.Passport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Passports {
		if item.ID == id {
			return publicPassport(item, true), nil
		}
	}
	return model.Passport{}, ErrNotFound
}

func (s *Store) PassportsByChange(organizationID, changeID string) []model.Passport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.Passport, 0)
	for _, item := range s.data.Passports {
		if item.OrganizationID != organizationID || item.ChangeID != changeID {
			continue
		}
		items = append(items, publicPassport(item, false))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].IssuedAt.After(items[j].IssuedAt) })
	return items
}

func (s *Store) PassportsByOrganization(organizationID string) []model.Passport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.Passport, 0)
	for _, item := range s.data.Passports {
		if item.OrganizationID != organizationID {
			continue
		}
		items = append(items, publicPassport(item, false))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].IssuedAt.After(items[j].IssuedAt) })
	return items
}

// UsePassport validates state and token binding while holding the store lock.
// consume=true performs the one-time state transition atomically, preventing
// concurrent CI jobs from replaying the same authorization.
func (s *Store) UsePassport(id, tokenSHA256, consumer string, at time.Time, consume bool, audit model.AuditEvent) (model.Passport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		passport, payload, version, err := backend.usePassport(ctx, id, tokenSHA256, consumer, at, consume, audit)
		if len(payload) > 0 {
			if installErr := s.installPostgresSnapshot(payload, version); installErr != nil {
				return model.Passport{}, installErr
			}
		}
		return passport, err
	}
	for index := range s.data.Passports {
		item := &s.data.Passports[index]
		if item.ID != id {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(item.TokenSHA256Stored), []byte(tokenSHA256)) != 1 {
			return model.Passport{}, ErrPassportTokenMismatch
		}
		if item.Status == model.PassportExpired {
			return model.Passport{}, ErrPassportExpired
		}
		if item.Status != model.PassportActive {
			return model.Passport{}, ErrPassportInactive
		}
		if !at.Before(item.ExpiresAt) {
			oldItem := *item
			oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
			item.Status = model.PassportExpired
			audit.Action = "PASSPORT_EXPIRED"
			s.appendAuditsLocked(audit)
			if err := s.saveLocked(); err != nil {
				s.data.Passports[index] = oldItem
				s.data.Audits = oldAudits
				return model.Passport{}, err
			}
			return model.Passport{}, ErrPassportExpired
		}
		changeIndex := -1
		for candidate := range s.data.Changes {
			change := &s.data.Changes[candidate]
			if change.ID == item.ChangeID && change.OrganizationID == item.OrganizationID {
				changeIndex = candidate
				break
			}
		}
		if changeIndex < 0 {
			return model.Passport{}, ErrPassportChangeInvalid
		}
		boundChange := s.data.Changes[changeIndex]
		currentPolicies := make([]model.RiskPolicy, 0)
		for _, policy := range s.data.Policies {
			if policy.OrganizationID == item.OrganizationID {
				currentPolicies = append(currentPolicies, policy)
			}
		}
		if boundChange.Status != model.StatusApproved ||
			boundChange.ArtifactSHA256 != item.ArtifactSHA256 ||
			boundChange.Environment != item.Environment ||
			boundChange.RuleSetVersion != item.RuleSetVersion ||
			changegate.RuleSetVersion(currentPolicies) != item.RuleSetVersion {
			return model.Passport{}, ErrPassportChangeInvalid
		}
		if consume {
			oldItem := *item
			oldChange := s.data.Changes[changeIndex]
			oldChange.Timeline = append([]model.TimelineEntry(nil), oldChange.Timeline...)
			oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)

			item.Status = model.PassportConsumed
			item.ConsumedAt = &at
			item.ConsumedBy = consumer
			change := &s.data.Changes[changeIndex]
			change.Status = model.StatusCompleted
			change.UpdatedAt = at
			change.Version++
			change.Timeline = append(change.Timeline, model.TimelineEntry{
				ID: NewID("timeline_"), Status: model.StatusCompleted, Title: "CI 通行证已消费",
				Detail: "制品摘要、环境与规则版本验证通过，生产变更自动闭环", Actor: consumer, CreatedAt: at,
			})
			audit.Action = "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED"
			audit.Detail += "；变更状态已原子更新为 COMPLETED"
			s.appendAuditsLocked(audit)
			if err := s.saveLocked(); err != nil {
				s.data.Passports[index] = oldItem
				s.data.Changes[changeIndex] = oldChange
				s.data.Audits = oldAudits
				return model.Passport{}, err
			}
		}
		return publicPassport(*item, false), nil
	}
	return model.Passport{}, ErrNotFound
}

func (s *Store) RevokePassport(organizationID, id, actorID string, at time.Time, audit model.AuditEvent) (model.Passport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Passports {
		item := &s.data.Passports[index]
		if item.ID != id || item.OrganizationID != organizationID {
			continue
		}
		if item.Status != model.PassportActive {
			return model.Passport{}, ErrPassportInactive
		}
		oldItem := *item
		oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
		item.Status = model.PassportRevoked
		item.RevokedAt = &at
		item.RevokedByID = actorID
		s.appendAuditsLocked(audit)
		if err := s.saveLocked(); err != nil {
			s.data.Passports[index] = model.StoredPassport{Passport: oldItem.Passport, TokenSHA256Stored: oldItem.TokenSHA256Stored}
			s.data.Audits = oldAudits
			return model.Passport{}, err
		}
		return publicPassport(*item, false), nil
	}
	return model.Passport{}, ErrNotFound
}

func (s *Store) RevokePassportsByChange(organizationID, changeID, actorID string, at time.Time, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	oldPassports := append([]model.StoredPassport(nil), s.data.Passports...)
	oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
	for index := range s.data.Passports {
		item := &s.data.Passports[index]
		if item.OrganizationID != organizationID || item.ChangeID != changeID || item.Status != model.PassportActive {
			continue
		}
		item.Status = model.PassportRevoked
		item.RevokedAt = &at
		item.RevokedByID = actorID
		changed = true
	}
	if !changed {
		return nil
	}
	s.appendAuditsLocked(audit)
	if err := s.saveLocked(); err != nil {
		s.data.Passports = oldPassports
		s.data.Audits = oldAudits
		return err
	}
	return nil
}

func (s *Store) RecordAudit(audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, payload, version, err := backend.appendAudit(ctx, audit)
		if err != nil {
			return err
		}
		return s.installPostgresSnapshot(payload, version)
	}
	oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
	s.appendAuditsLocked(audit)
	if err := s.saveLocked(); err != nil {
		s.data.Audits = oldAudits
		return err
	}
	return nil
}

func publicPassport(item model.StoredPassport, includeTokenHash bool) model.Passport {
	passport := item.Passport
	if includeTokenHash {
		passport.TokenSHA256 = item.TokenSHA256Stored
	} else {
		passport.TokenSHA256 = ""
	}
	if !includeTokenHash && passport.Status == model.PassportActive && !time.Now().Before(passport.ExpiresAt) {
		passport.Status = model.PassportExpired
	}
	return passport
}
