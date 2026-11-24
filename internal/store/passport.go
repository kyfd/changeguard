package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

var (
	ErrPassportExpired       = errors.New("passport expired")
	ErrPassportInactive      = errors.New("passport inactive")
	ErrPassportTokenMismatch = errors.New("passport token mismatch")
	ErrPassportChangeInvalid = errors.New("passport change is not approved")
	ErrPassportReplay        = errors.New("passport already consumed by another consumer")
	ErrPassportRevoked       = errors.New("passport revoked")
	ErrPassportRuleSet       = fmt.Errorf("%w: rule set changed", ErrPassportChangeInvalid)
)

func (s *Store) CreatePassport(passport model.Passport, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stored, expired, linked, err := backend.createPassport(ctx, passport, audit)
		if err != nil {
			return err
		}
		for _, item := range expired {
			s.upsertPassportLocked(item)
		}
		s.upsertPassportLocked(stored)
		s.mergeAuditsLocked(linked...)
		s.localMutationSeq.Add(1)
		return nil
	}
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
// consume=true performs one logical consume. A later call with the same token
// hash and consumer returns the first public snapshot without writing again.
func (s *Store) UsePassport(id, tokenSHA256, consumer string, at time.Time, consume bool, audit model.AuditEvent) (model.Passport, error) {
	pass, _, err := s.UsePassportForJob(id, tokenSHA256, consumer, at, consume, audit, nil, nil)
	return pass, err
}

// UsePassportForJob makes Gate validation, release membership discovery, and
// the optional passport/change/job transition one atomic store operation.
// expected holds the signed fields, not a cached copy of the passport row.
func (s *Store) UsePassportForJob(id, tokenSHA256, consumer string, at time.Time, consume bool, audit model.AuditEvent, expected *model.Passport, job *model.GitLabJob) (model.Passport, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		passport, delta, err := backend.usePassport(ctx, id, tokenSHA256, consumer, at, consume, audit, expected, job)
		if delta.Passport != nil || delta.Change != nil || len(delta.NewAudits) > 0 || len(delta.ChangeAudits) > 0 {
			s.applyPassportDeltaLocked(delta)
			s.localMutationSeq.Add(1)
		}
		return passport, delta.Replayed, err
	}
	if at.IsZero() {
		at = s.clock().UTC()
	}
	for index := range s.data.Passports {
		item := &s.data.Passports[index]
		if item.ID != id {
			continue
		}
		replayed, err := validatePassportUse(*item, expected, tokenSHA256, consumer, consume)
		if err != nil {
			return model.Passport{}, false, err
		}
		if !replayed && !at.Before(item.ExpiresAt) {
			oldItem := *item
			oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
			item.Status = model.PassportExpired
			audit.Action = "PASSPORT_EXPIRED"
			s.appendAuditsLocked(audit)
			if err := s.saveLocked(); err != nil {
				s.data.Passports[index] = oldItem
				s.data.Audits = oldAudits
				return model.Passport{}, false, err
			}
			return model.Passport{}, false, ErrPassportExpired
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
			return model.Passport{}, false, ErrPassportChangeInvalid
		}
		boundChange := s.data.Changes[changeIndex]
		snapshot := releaseSnapshot{data: s.data, plans: s.data.JointReleases}
		if err := validatePassportChange(*item, boundChange, snapshot.rules(item.OrganizationID), replayed); err != nil {
			return model.Passport{}, false, err
		}
		plan, position := snapshot.membership(item.OrganizationID, item.ChangeID)
		updatedPlan := plan
		if position >= 0 {
			updatedPlan, err = snapshot.gate(plan, position, *item, job, at, consume)
			if err != nil {
				return model.Passport{}, false, err
			}
		}
		var existing deploymentJobBinding
		if consume || job != nil {
			for _, binding := range s.data.DeploymentJobs {
				if binding.PassportID == id || job != nil && binding.Source == job.Source && binding.JobID == job.JobID {
					existing = binding
					break
				}
			}
			if err := validateJobBinding(existing, *item, plan, job, replayed); err != nil {
				return model.Passport{}, false, err
			}
		}
		if !consume && expected != nil {
			oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
			s.appendAuditsLocked(audit)
			if err := s.saveLocked(); err != nil {
				s.data.Audits = oldAudits
				return model.Passport{}, false, err
			}
		}
		if consume && !replayed {
			oldItem := *item
			oldChange := s.data.Changes[changeIndex]
			oldChange.Timeline = append([]model.TimelineEntry(nil), oldChange.Timeline...)
			oldAudits := append([]model.AuditEvent(nil), s.data.Audits...)
			oldBindings := s.data.DeploymentJobs

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
			if job != nil {
				s.data.DeploymentJobs = append(s.data.DeploymentJobs, bindJob(job, *item, plan))
			}
			if position >= 0 {
				s.replaceReleaseLocked(updatedPlan)
			}
			if err := s.saveLocked(); err != nil {
				s.data.Passports[index] = oldItem
				s.data.Changes[changeIndex] = oldChange
				s.data.Audits = oldAudits
				s.data.DeploymentJobs = oldBindings
				if position >= 0 {
					s.replaceReleaseLocked(plan)
				}
				return model.Passport{}, false, err
			}
		}
		return publicPassport(*item, false), replayed, nil
	}
	return model.Passport{}, false, ErrNotFound
}

func validatePassportUse(item model.StoredPassport, expected *model.Passport, hash, consumer string, consume bool) (bool, error) {
	if subtle.ConstantTimeCompare([]byte(item.TokenSHA256Stored), []byte(hash)) != 1 {
		return false, ErrPassportTokenMismatch
	}
	if expected != nil && (item.ID != expected.ID || item.OrganizationID != expected.OrganizationID ||
		item.ChangeID != expected.ChangeID || item.ArtifactSHA256 != expected.ArtifactSHA256 ||
		item.Environment != expected.Environment || item.RuleSetVersion != expected.RuleSetVersion ||
		item.ApproverID != expected.ApproverID || !item.IssuedAt.Equal(expected.IssuedAt) || !item.ExpiresAt.Equal(expected.ExpiresAt)) {
		return false, ErrPassportTokenMismatch
	}
	switch item.Status {
	case model.PassportActive:
		return false, nil
	case model.PassportExpired:
		return false, ErrPassportExpired
	case model.PassportConsumed:
		if consume && sameConsumerReplay(item, hash, consumer) {
			return true, nil
		}
		return false, ErrPassportReplay
	case model.PassportRevoked:
		if expected != nil {
			return false, ErrPassportRevoked
		}
	}
	return false, ErrPassportInactive
}

func validatePassportChange(item model.StoredPassport, bound model.ChangeRequest, rules string, replayed bool) error {
	status := model.StatusApproved
	if replayed {
		status = model.StatusCompleted
	}
	if bound.Status != status || bound.ArtifactSHA256 != item.ArtifactSHA256 || bound.Environment != item.Environment ||
		bound.RuleSetVersion != item.RuleSetVersion || item.ArtifactVersion > 0 && bound.MaterialVersion() != item.ArtifactVersion {
		return ErrPassportChangeInvalid
	}
	if rules != item.RuleSetVersion {
		return ErrPassportRuleSet
	}
	return nil
}

func validateJobBinding(existing deploymentJobBinding, item model.StoredPassport, plan model.JointRelease, job *model.GitLabJob, replayed bool) error {
	if replayed {
		if existing.PassportID == "" && job == nil && plan.ID == "" || existing.matchesJob(job, item, plan) {
			return nil
		}
		return ErrPassportReplay
	}
	if job != nil && !validJob(job, item.OrganizationID) {
		return ErrReleaseJob
	}
	if existing.PassportID != "" {
		return ErrPassportReplay
	}
	return nil
}

func sameConsumerReplay(item model.StoredPassport, tokenSHA256, consumer string) bool {
	if item.Status != model.PassportConsumed {
		return false
	}
	if strings.TrimSpace(item.ConsumedBy) == "" || strings.TrimSpace(consumer) == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(item.TokenSHA256Stored), []byte(tokenSHA256)) != 1 {
		return false
	}
	return item.ConsumedBy == consumer
}

func (s *Store) RevokePassport(organizationID, id, actorID string, at time.Time, audit model.AuditEvent) (model.Passport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pub, stored, linked, err := backend.revokePassport(ctx, organizationID, id, actorID, at, audit)
		if err != nil {
			return model.Passport{}, err
		}
		s.upsertPassportLocked(stored)
		s.mergeAuditsLocked(linked...)
		s.localMutationSeq.Add(1)
		return pub, nil
	}
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
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		revoked, linked, err := backend.revokePassportsByChange(ctx, organizationID, changeID, actorID, at, audit)
		if err != nil {
			return err
		}
		for _, item := range revoked {
			s.upsertPassportLocked(item)
		}
		s.mergeAuditsLocked(linked...)
		if len(revoked) > 0 {
			s.localMutationSeq.Add(1)
		}
		return nil
	}
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
		linked, err := backend.appendAudit(ctx, audit)
		if err != nil {
			return err
		}
		s.mergeAuditsLocked(linked)
		s.localMutationSeq.Add(1)
		return nil
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
	if passport.ConsumedAt != nil {
		at := *passport.ConsumedAt
		passport.ConsumedAt = &at
	}
	if passport.RevokedAt != nil {
		at := *passport.RevokedAt
		passport.RevokedAt = &at
	}
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
