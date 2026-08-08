package store

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

var ErrConflict = errors.New("record conflict")

func (s *Store) Organizations() []model.Organization {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.Organization(nil), s.data.Organizations...)
}

func (s *Store) Organization(id string) (model.Organization, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Organizations {
		if item.ID == id {
			return item, nil
		}
	}
	return model.Organization{}, ErrNotFound
}

func (s *Store) OrganizationBySlug(slug string) (model.Organization, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Organizations {
		if strings.EqualFold(item.Slug, strings.TrimSpace(slug)) {
			return item, nil
		}
	}
	return model.Organization{}, ErrNotFound
}

func (s *Store) OrganizationByDomain(domain string) (model.Organization, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Organizations {
		if !item.AllowDomainJoin {
			continue
		}
		for _, allowed := range item.EmailDomains {
			if strings.EqualFold(strings.TrimSpace(allowed), domain) {
				return item, nil
			}
		}
	}
	return model.Organization{}, ErrNotFound
}

func (s *Store) UsersByOrganization(organizationID string) []model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.User, 0)
	for _, item := range s.data.Users {
		if item.OrganizationID == organizationID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Active != items[j].Active {
			return items[i].Active
		}
		if items[i].EnterpriseAdmin != items[j].EnterpriseAdmin {
			return items[i].EnterpriseAdmin
		}
		return items[i].Name < items[j].Name
	})
	return items
}

func (s *Store) ApplicationsByOrganization(organizationID string) []model.Application {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.Application, 0)
	for _, item := range s.data.Applications {
		if item.OrganizationID == organizationID {
			items = append(items, item)
		}
	}
	return items
}

func (s *Store) ChangesByOrganization(organizationID string) []model.ChangeRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.ChangeRequest, 0)
	for _, item := range s.data.Changes {
		if item.OrganizationID == organizationID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

func (s *Store) AuditsByOrganization(organizationID string, limit int) []model.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.AuditEvent, 0)
	for _, item := range s.data.Audits {
		if item.OrganizationID == organizationID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) AuditsByChange(organizationID, changeID string) []model.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.AuditEvent, 0)
	for _, item := range s.data.Audits {
		if item.OrganizationID == organizationID && item.ChangeID == changeID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items
}

func (s *Store) PoliciesByOrganization(organizationID string) []model.RiskPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.RiskPolicy, 0)
	for _, item := range s.data.Policies {
		if item.OrganizationID == organizationID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Builtin != items[j].Builtin {
			return items[i].Builtin
		}
		if items[i].Enabled != items[j].Enabled {
			return items[i].Enabled
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items
}

func (s *Store) PolicyForOrganization(organizationID, id string) (model.RiskPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Policies {
		if item.OrganizationID == organizationID && item.ID == id {
			return item, nil
		}
	}
	return model.RiskPolicy{}, ErrNotFound
}

func (s *Store) PolicyByCodeForOrganization(organizationID, code string) (model.RiskPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Policies {
		if item.OrganizationID == organizationID && item.Code == code {
			return item, nil
		}
	}
	return model.RiskPolicy{}, ErrNotFound
}

func (s *Store) RecordPolicyHitsForOrganization(organizationID string, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unique := make(map[string]bool, len(codes))
	for _, code := range codes {
		unique[code] = true
	}
	now := time.Now()
	for index := range s.data.Policies {
		policy := &s.data.Policies[index]
		if policy.OrganizationID == organizationID && unique[policy.Code] {
			policy.HitCount++
			policy.LastHitAt = &now
		}
	}
	return s.saveLocked()
}

func (s *Store) DashboardByOrganization(organizationID string) model.Dashboard {
	changes := s.ChangesByOrganization(organizationID)
	dashboard := model.Dashboard{
		RiskDistribution: map[model.RiskLevel]int{
			model.RiskLow: 0, model.RiskMedium: 0, model.RiskHigh: 0, model.RiskUnknown: 0,
		},
	}
	var experimentCount, experimentPass int
	var durationTotal int64
	for _, item := range changes {
		dashboard.RiskDistribution[item.Risk]++
		if item.Risk == model.RiskHigh {
			dashboard.HighRiskCount++
		}
		switch item.Status {
		case model.StatusDraft, model.StatusChecking, model.StatusReadyForExperiment,
			model.StatusExperimentQueued, model.StatusExperimentRunning, model.StatusWaitingApproval:
			dashboard.PendingCount++
		}
		if item.Status == model.StatusWaitingApproval {
			dashboard.PendingApprovals = append(dashboard.PendingApprovals, item)
		}
		if item.Experiment != nil {
			experimentCount++
			durationTotal += item.Experiment.DurationMS
			if item.Experiment.Status == "PASSED" {
				experimentPass++
			}
		}
	}
	if len(changes) > 6 {
		dashboard.RecentChanges = changes[:6]
	} else {
		dashboard.RecentChanges = changes
	}
	if experimentCount > 0 {
		dashboard.ExperimentPassRate = float64(experimentPass) / float64(experimentCount) * 100
		dashboard.AverageExperimentSec = float64(durationTotal) / float64(experimentCount) / 1000
	}
	return dashboard
}

func (s *Store) UserByEmail(email string) (model.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Users {
		if strings.EqualFold(item.Email, email) {
			return item, nil
		}
	}
	return model.User{}, ErrNotFound
}

func (s *Store) Credential(userID string) (model.UserCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Credentials {
		if item.UserID == userID {
			return item, nil
		}
	}
	return model.UserCredential{}, ErrNotFound
}

func (s *Store) SSOUser(provider, subject string) (model.User, model.UserCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, credential := range s.data.Credentials {
		if credential.IdentityProvider != provider || credential.Subject != subject {
			continue
		}
		for _, user := range s.data.Users {
			if user.ID == credential.UserID {
				return user, credential, nil
			}
		}
	}
	return model.User{}, model.UserCredential{}, ErrNotFound
}

func (s *Store) CreateEnterprise(organization model.Organization, user model.User, credential model.UserCredential, policies []model.RiskPolicy, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Organizations {
		if strings.EqualFold(item.Slug, organization.Slug) {
			return ErrConflict
		}
	}
	for _, item := range s.data.Users {
		if strings.EqualFold(item.Email, user.Email) {
			return ErrConflict
		}
	}
	s.data.Organizations = append(s.data.Organizations, organization)
	s.data.Users = append(s.data.Users, user)
	s.data.Credentials = append(s.data.Credentials, credential)
	s.data.Policies = append(s.data.Policies, policies...)
	s.data.Audits = append(s.data.Audits, audit)
	return s.saveLocked()
}

func (s *Store) UpdateOrganization(id string, update func(*model.Organization) error, audit model.AuditEvent) (model.Organization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Organizations {
		if s.data.Organizations[index].ID != id {
			continue
		}
		candidate := s.data.Organizations[index]
		if err := update(&candidate); err != nil {
			return model.Organization{}, err
		}
		if candidate.AllowDomainJoin {
			for otherIndex, other := range s.data.Organizations {
				if otherIndex == index || !other.AllowDomainJoin {
					continue
				}
				for _, candidateDomain := range candidate.EmailDomains {
					for _, otherDomain := range other.EmailDomains {
						if strings.EqualFold(strings.TrimSpace(candidateDomain), strings.TrimSpace(otherDomain)) {
							return model.Organization{}, ErrConflict
						}
					}
				}
			}
		}
		candidate.UpdatedAt = time.Now()
		s.data.Organizations[index] = candidate
		s.data.Audits = append(s.data.Audits, audit)
		if err := s.saveLocked(); err != nil {
			return model.Organization{}, err
		}
		return candidate, nil
	}
	return model.Organization{}, ErrNotFound
}

func (s *Store) CreateInvite(invite model.OrganizationInvite, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Invites {
		if item.OrganizationID == invite.OrganizationID &&
			strings.EqualFold(item.Email, invite.Email) &&
			item.Status == model.InvitePending &&
			time.Now().Before(item.ExpiresAt) {
			return ErrConflict
		}
	}
	s.data.Invites = append(s.data.Invites, invite)
	s.data.Audits = append(s.data.Audits, audit)
	return s.saveLocked()
}

func (s *Store) InvitesByOrganization(organizationID string) []model.OrganizationInvite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.OrganizationInvite, 0)
	now := time.Now()
	for _, item := range s.data.Invites {
		if item.OrganizationID != organizationID {
			continue
		}
		if item.Status == model.InvitePending && now.After(item.ExpiresAt) {
			item.Status = model.InviteExpired
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

func (s *Store) InviteByTokenHash(tokenHash string) (model.OrganizationInvite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Invites {
		if item.TokenHash == tokenHash && item.Status == model.InvitePending && time.Now().Before(item.ExpiresAt) {
			return item, nil
		}
	}
	return model.OrganizationInvite{}, ErrNotFound
}

func (s *Store) PendingInviteByEmail(email string) (model.OrganizationInvite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Invites {
		if strings.EqualFold(item.Email, strings.TrimSpace(email)) &&
			item.Status == model.InvitePending && time.Now().Before(item.ExpiresAt) {
			return item, nil
		}
	}
	return model.OrganizationInvite{}, ErrNotFound
}

func (s *Store) AcceptInvite(inviteID string, user model.User, credential model.UserCredential, audit model.AuditEvent) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Users {
		if strings.EqualFold(item.Email, user.Email) {
			return model.User{}, ErrConflict
		}
	}
	for index := range s.data.Invites {
		invite := &s.data.Invites[index]
		if invite.ID != inviteID || invite.Status != model.InvitePending || time.Now().After(invite.ExpiresAt) {
			continue
		}
		if invite.OrganizationID != user.OrganizationID || !strings.EqualFold(invite.Email, user.Email) || invite.Role != user.Role {
			return model.User{}, ErrConflict
		}
		now := time.Now()
		invite.Status = model.InviteAccepted
		invite.AcceptedByID = user.ID
		invite.AcceptedAt = &now
		s.data.Users = append(s.data.Users, user)
		s.data.Credentials = append(s.data.Credentials, credential)
		s.data.Audits = append(s.data.Audits, audit)
		if err := s.saveLocked(); err != nil {
			return model.User{}, err
		}
		return user, nil
	}
	return model.User{}, ErrNotFound
}

func (s *Store) CreateSSOUser(user model.User, credential model.UserCredential, inviteID string, audit model.AuditEvent) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Users {
		if strings.EqualFold(item.Email, user.Email) {
			return model.User{}, ErrConflict
		}
	}
	if inviteID != "" {
		found := false
		for index := range s.data.Invites {
			invite := &s.data.Invites[index]
			if invite.ID == inviteID && invite.Status == model.InvitePending && time.Now().Before(invite.ExpiresAt) {
				now := time.Now()
				invite.Status = model.InviteAccepted
				invite.AcceptedByID = user.ID
				invite.AcceptedAt = &now
				found = true
				break
			}
		}
		if !found {
			return model.User{}, ErrNotFound
		}
	}
	s.data.Users = append(s.data.Users, user)
	s.data.Credentials = append(s.data.Credentials, credential)
	s.data.Audits = append(s.data.Audits, audit)
	if err := s.saveLocked(); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func (s *Store) UpdateIdentityLogin(userID string, updateUser func(*model.User), updateCredential func(*model.UserCredential), audit model.AuditEvent) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var user *model.User
	for index := range s.data.Users {
		if s.data.Users[index].ID == userID {
			user = &s.data.Users[index]
			break
		}
	}
	if user == nil {
		return model.User{}, ErrNotFound
	}
	updateUser(user)
	foundCredential := false
	for index := range s.data.Credentials {
		if s.data.Credentials[index].UserID == userID {
			updateCredential(&s.data.Credentials[index])
			foundCredential = true
			break
		}
	}
	if !foundCredential {
		credential := model.UserCredential{UserID: userID}
		updateCredential(&credential)
		s.data.Credentials = append(s.data.Credentials, credential)
	}
	s.data.Audits = append(s.data.Audits, audit)
	if err := s.saveLocked(); err != nil {
		return model.User{}, err
	}
	return *user, nil
}

func (s *Store) UpdateMember(organizationID, userID string, update func(*model.User) error, grants []model.ApplicationGrantInput, audit model.AuditEvent) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Users {
		if s.data.Users[index].ID != userID || s.data.Users[index].OrganizationID != organizationID {
			continue
		}
		candidate := s.data.Users[index]
		if err := update(&candidate); err != nil {
			return model.User{}, err
		}
		applicationExists := func(id string) bool {
			for _, application := range s.data.Applications {
				if application.ID == id && application.OrganizationID == organizationID {
					return true
				}
			}
			return false
		}
		now := time.Now()
		replacement := make([]model.ApplicationGrant, 0, len(grants))
		seen := make(map[string]bool)
		for _, input := range grants {
			applicationID := strings.TrimSpace(input.ApplicationID)
			if applicationID == "" || seen[applicationID] || !applicationExists(applicationID) {
				if applicationID != "" && !applicationExists(applicationID) {
					return model.User{}, ErrNotFound
				}
				continue
			}
			seen[applicationID] = true
			if !input.CanSubmit && !input.CanReview {
				continue
			}
			replacement = append(replacement, model.ApplicationGrant{OrganizationID: organizationID, UserID: userID, ApplicationID: applicationID, CanSubmit: input.CanSubmit, CanReview: input.CanReview, UpdatedBy: audit.ActorID, UpdatedAt: now})
		}
		kept := make([]model.ApplicationGrant, 0, len(s.data.ApplicationGrants)+len(replacement))
		for _, grant := range s.data.ApplicationGrants {
			if grant.OrganizationID != organizationID || grant.UserID != userID {
				kept = append(kept, grant)
			}
		}
		s.data.ApplicationGrants = append(kept, replacement...)
		for organizationIndex := range s.data.Organizations {
			if s.data.Organizations[organizationIndex].ID == organizationID {
				s.data.Organizations[organizationIndex].ApplicationAccessControlled = true
				s.data.Organizations[organizationIndex].UpdatedAt = now
				break
			}
		}
		s.data.Users[index] = candidate
		s.data.Audits = append(s.data.Audits, audit)
		if err := s.saveLocked(); err != nil {
			return model.User{}, err
		}
		return candidate, nil
	}
	return model.User{}, ErrNotFound
}

func (s *Store) ApplicationGrantsByUser(organizationID, userID string) []model.ApplicationGrant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.ApplicationGrant, 0)
	for _, grant := range s.data.ApplicationGrants {
		if grant.OrganizationID == organizationID && grant.UserID == userID {
			items = append(items, grant)
		}
	}
	return items
}

func (s *Store) HasApplicationGrants(organizationID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, organization := range s.data.Organizations {
		if organization.ID == organizationID && organization.ApplicationAccessControlled {
			return true
		}
	}
	for _, grant := range s.data.ApplicationGrants {
		if grant.OrganizationID == organizationID {
			return true
		}
	}
	return false
}
func (s *Store) ActiveEnterpriseAdminCount(organizationID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, user := range s.data.Users {
		if user.OrganizationID == organizationID && user.Active && user.EnterpriseAdmin {
			count++
		}
	}
	return count
}

func (s *Store) RevokeInvite(organizationID, inviteID string, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Invites {
		invite := &s.data.Invites[index]
		if invite.ID == inviteID && invite.OrganizationID == organizationID && invite.Status == model.InvitePending {
			invite.Status = model.InviteRevoked
			s.data.Audits = append(s.data.Audits, audit)
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

func (s *Store) UpsertSSOUser(identity model.User, audit model.AuditEvent) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	updateExisting := func(user *model.User, credential *model.UserCredential) (model.User, error) {
		if !user.Active {
			return model.User{}, ErrConflict
		}
		if user.IdentityProvider != "" && !strings.EqualFold(user.IdentityProvider, identity.IdentityProvider) {
			return model.User{}, ErrConflict
		}
		if credential.IdentityProvider != "" && !strings.EqualFold(credential.IdentityProvider, identity.IdentityProvider) {
			return model.User{}, ErrConflict
		}
		if credential.Subject != "" && credential.Subject != identity.Subject {
			return model.User{}, ErrConflict
		}
		user.Name = identity.Name
		if identity.Email != "" {
			user.Email = identity.Email
		}
		user.IdentityProvider = identity.IdentityProvider
		user.LastLoginAt = &now
		credential.IdentityProvider = identity.IdentityProvider
		credential.Subject = identity.Subject
		audit.OrganizationID = user.OrganizationID
		audit.ActorID = user.ID
		audit.ActorName = user.Name
		s.data.Audits = append(s.data.Audits, audit)
		if err := s.saveLocked(); err != nil {
			return model.User{}, err
		}
		return *user, nil
	}
	for credentialIndex := range s.data.Credentials {
		credential := &s.data.Credentials[credentialIndex]
		if credential.IdentityProvider != identity.IdentityProvider || credential.Subject != identity.Subject {
			continue
		}
		for userIndex := range s.data.Users {
			if s.data.Users[userIndex].ID == credential.UserID {
				return updateExisting(&s.data.Users[userIndex], credential)
			}
		}
	}
	for userIndex := range s.data.Users {
		user := &s.data.Users[userIndex]
		if !strings.EqualFold(user.Email, identity.Email) {
			continue
		}
		for credentialIndex := range s.data.Credentials {
			if s.data.Credentials[credentialIndex].UserID == user.ID {
				return updateExisting(user, &s.data.Credentials[credentialIndex])
			}
		}
		credential := model.UserCredential{UserID: user.ID}
		s.data.Credentials = append(s.data.Credentials, credential)
		return updateExisting(user, &s.data.Credentials[len(s.data.Credentials)-1])
	}
	var organization model.Organization
	var role string
	var acceptedInvite *model.OrganizationInvite
	for inviteIndex := range s.data.Invites {
		invite := &s.data.Invites[inviteIndex]
		if strings.EqualFold(invite.Email, identity.Email) &&
			invite.Status == model.InvitePending && now.Before(invite.ExpiresAt) {
			for _, item := range s.data.Organizations {
				if item.ID == invite.OrganizationID {
					organization = item
					role = invite.Role
					acceptedInvite = invite
					break
				}
			}
		}
		if organization.ID != "" {
			break
		}
	}
	if organization.ID == "" {
		parts := strings.Split(strings.ToLower(identity.Email), "@")
		if len(parts) == 2 {
			for _, item := range s.data.Organizations {
				if !item.AllowDomainJoin {
					continue
				}
				for _, domain := range item.EmailDomains {
					if strings.EqualFold(domain, parts[1]) {
						organization = item
						break
					}
				}
				if organization.ID != "" {
					break
				}
			}
		}
	}
	if organization.ID == "" {
		return model.User{}, ErrNotFound
	}
	if role == "" {
		role = identity.Role
	}
	if role != model.RoleDeveloper && role != model.RoleReviewer && role != model.RoleOwner {
		role = model.RoleDeveloper
	}
	identity.ID = NewID("usr_")
	identity.OrganizationID = organization.ID
	identity.OrganizationName = organization.Name
	identity.Role = role
	identity.Active = true
	identity.EnterpriseAdmin = false
	identity.LastLoginAt = &now
	credential := model.UserCredential{
		UserID: identity.ID, IdentityProvider: identity.IdentityProvider, Subject: identity.Subject,
	}
	if acceptedInvite != nil {
		acceptedInvite.Status = model.InviteAccepted
		acceptedInvite.AcceptedByID = identity.ID
		acceptedInvite.AcceptedAt = &now
	}
	audit.OrganizationID = organization.ID
	audit.ActorID = identity.ID
	audit.ActorName = identity.Name
	s.data.Users = append(s.data.Users, identity)
	s.data.Credentials = append(s.data.Credentials, credential)
	s.data.Audits = append(s.data.Audits, audit)
	if err := s.saveLocked(); err != nil {
		return model.User{}, err
	}
	return identity, nil
}

func (s *Store) DomainClaimedByOther(organizationID, domain string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, organization := range s.data.Organizations {
		if organization.ID == organizationID || !organization.AllowDomainJoin {
			continue
		}
		for _, item := range organization.EmailDomains {
			if strings.EqualFold(strings.TrimSpace(item), domain) {
				return true
			}
		}
	}
	return false
}

func (s *Store) HasActiveSSOAdmin(organizationID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, user := range s.data.Users {
		if user.OrganizationID != organizationID || !user.Active || !user.EnterpriseAdmin {
			continue
		}
		for _, credential := range s.data.Credentials {
			if credential.UserID == user.ID && credential.IdentityProvider != "" && credential.Subject != "" {
				return true
			}
		}
	}
	return false
}

func (s *Store) CreateApplication(application model.Application, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Applications {
		if item.OrganizationID == application.OrganizationID &&
			(strings.EqualFold(item.Name, application.Name) ||
				(strings.TrimSpace(application.Database) != "" && strings.EqualFold(item.Database, application.Database) && strings.EqualFold(item.Schema, application.Schema))) {
			return ErrConflict
		}
	}
	s.data.Applications = append(s.data.Applications, application)
	for index := range s.data.Organizations {
		if s.data.Organizations[index].ID == application.OrganizationID {
			s.data.Organizations[index].ApplicationAccessControlled = true
			s.data.Organizations[index].UpdatedAt = time.Now()
			break
		}
	}
	s.data.Audits = append(s.data.Audits, audit)
	return s.saveLocked()
}

func (s *Store) UpdateApplication(organizationID, applicationID string, update func(*model.Application) error, audit model.AuditEvent) (model.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Applications {
		application := &s.data.Applications[index]
		if application.ID != applicationID || application.OrganizationID != organizationID {
			continue
		}
		candidate := *application
		if err := update(&candidate); err != nil {
			return model.Application{}, err
		}
		for otherIndex, other := range s.data.Applications {
			if otherIndex == index || other.OrganizationID != organizationID {
				continue
			}
			if strings.EqualFold(other.Name, candidate.Name) ||
				(strings.TrimSpace(candidate.Database) != "" && strings.EqualFold(other.Database, candidate.Database) && strings.EqualFold(other.Schema, candidate.Schema)) {
				return model.Application{}, ErrConflict
			}
		}
		s.data.Applications[index] = candidate
		application = &s.data.Applications[index]
		s.data.Audits = append(s.data.Audits, audit)
		if err := s.saveLocked(); err != nil {
			return model.Application{}, err
		}
		return *application, nil
	}
	return model.Application{}, ErrNotFound
}
