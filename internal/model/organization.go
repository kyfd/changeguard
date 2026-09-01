package model

import "time"

const (
	RoleDeveloper = "后端开发"
	RoleReviewer  = "数据库审核人"
	RoleOwner     = "技术负责人"
)

const (
	InvitePending  = "PENDING"
	InviteAccepted = "ACCEPTED"
	InviteRevoked  = "REVOKED"
	InviteExpired  = "EXPIRED"
)

type Organization struct {
	ID                          string          `json:"id"`
	Name                        string          `json:"name"`
	Slug                        string          `json:"slug"`
	EmailDomains                []string        `json:"email_domains,omitempty"`
	AllowDomainJoin             bool            `json:"allow_domain_join"`
	SSOEnforced                 bool            `json:"sso_enforced"`
	ApplicationAccessControlled bool            `json:"application_access_controlled"`
	Retention                   RetentionPolicy `json:"retention"`
	CreatedBy                   string          `json:"created_by"`
	CreatedAt                   time.Time       `json:"created_at"`
	UpdatedAt                   time.Time       `json:"updated_at"`
}

type RetentionPolicy struct {
	AuditDays             int  `json:"audit_days"`
	IntegrationEventDays  int  `json:"integration_event_days"`
	OutcomeSignalDays     int  `json:"outcome_signal_days"`
	IdempotencyHours      int  `json:"idempotency_hours"`
	AgentConversationDays int  `json:"agent_conversation_days"`
	ArtifactBodyDays      int  `json:"artifact_body_days"`
	LegalHold             bool `json:"legal_hold"`
}

type OrganizationInvite struct {
	ID               string     `json:"id"`
	OrganizationID   string     `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	Email            string     `json:"email"`
	Role             string     `json:"role"`
	TokenHash        string     `json:"-"`
	Status           string     `json:"status"`
	ExpiresAt        time.Time  `json:"expires_at"`
	CreatedByID      string     `json:"created_by_id"`
	CreatedByName    string     `json:"created_by_name"`
	CreatedAt        time.Time  `json:"created_at"`
	AcceptedByID     string     `json:"accepted_by_id,omitempty"`
	AcceptedAt       *time.Time `json:"accepted_at,omitempty"`
}

type UserCredential struct {
	UserID           string `json:"user_id"`
	PasswordSalt     string `json:"password_salt,omitempty"`
	PasswordHash     string `json:"password_hash,omitempty"`
	IdentityProvider string `json:"identity_provider,omitempty"`
	Subject          string `json:"subject,omitempty"`
}

type RegisterEnterpriseInput struct {
	OrganizationName string `json:"organization_name"`
	OrganizationSlug string `json:"organization_slug"`
	Name             string `json:"name"`
	Email            string `json:"email"`
	Password         string `json:"password"`
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type CreateInviteInput struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	ExpiresIn int    `json:"expires_in_hours"`
}

type AcceptInviteInput struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type UpdateMemberInput struct {
	Role              string                  `json:"role"`
	Active            bool                    `json:"active"`
	EnterpriseAdmin   bool                    `json:"enterprise_admin"`
	ApplicationGrants []ApplicationGrantInput `json:"application_grants"`
}

type ApplicationGrantInput struct {
	ApplicationID string `json:"application_id"`
	CanSubmit     bool   `json:"can_submit"`
	CanReview     bool   `json:"can_review"`
}

type ApplicationGrant struct {
	OrganizationID string    `json:"organization_id"`
	UserID         string    `json:"user_id"`
	ApplicationID  string    `json:"application_id"`
	CanSubmit      bool      `json:"can_submit"`
	CanReview      bool      `json:"can_review"`
	UpdatedBy      string    `json:"updated_by"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type UpdateOrganizationInput struct {
	Name            string   `json:"name"`
	EmailDomains    []string `json:"email_domains"`
	AllowDomainJoin bool     `json:"allow_domain_join"`
	SSOEnforced     bool     `json:"sso_enforced"`
}

type InviteCreated struct {
	Invite     OrganizationInvite `json:"invite"`
	InviteURL  string             `json:"invite_url"`
	PlainToken string             `json:"plain_token,omitempty"`
}

type AuthSession struct {
	User         User         `json:"user"`
	Organization Organization `json:"organization"`
	CSRFToken    string       `json:"csrf_token"`
}

type MemberAccess struct {
	User              User               `json:"user"`
	ApplicationGrants []ApplicationGrant `json:"application_grants"`
}
