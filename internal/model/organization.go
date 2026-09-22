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

// ModelProviderKind 上游模型接口形态。
//
// 只支持两种：OpenAI 兼容（/chat/completions + /models）与 Anthropic Messages。
// 不提供"自动探测"：猜错形态会把请求发到一个不存在的路径上，
// 得到的错误信息反而更难排查。
type ModelProviderKind string

const (
	ModelProviderOpenAI    ModelProviderKind = "openai_compatible"
	ModelProviderAnthropic ModelProviderKind = "anthropic"
)

// OrganizationModelConfig 企业自配的模型接入。
//
// APIKeyCiphertext 是密文（见 internal/modelsecret）；明文只在解析给
// Agent 运行时的那一瞬间存在，不落日志、不进响应、不缓存到前端。
type OrganizationModelConfig struct {
	OrganizationID string            `json:"organization_id"`
	Enabled        bool              `json:"enabled"`
	Provider       ModelProviderKind `json:"provider"`
	BaseURL        string            `json:"base_url"`
	Model          string            `json:"model"`
	MaxTokens      int               `json:"max_tokens"`
	APIKeyCipher   string            `json:"api_key_ciphertext,omitempty"`
	// APIKeyHint 只用于界面展示"已保存哪个 Key"，不含可重放内容。
	APIKeyHint   string    `json:"api_key_hint,omitempty"`
	ConfiguredBy string    `json:"configured_by"`
	ConfiguredAt time.Time `json:"configured_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// LastTestOK / LastTestAt / LastTestError 记录最近一次连通性探测结果，
	// 让"配过但连不上"和"没配过"在界面上可区分。
	LastTestOK    bool       `json:"last_test_ok"`
	LastTestAt    *time.Time `json:"last_test_at,omitempty"`
	LastTestError string     `json:"last_test_error,omitempty"`
}

// ModelConfigInput 保存模型接入的请求体。与存储结构的区别是：
// APIKey 是明文（只在请求体内），ClearAPIKey 支持"保留原 Key 但改别的字段"。
type ModelConfigInput struct {
	Enabled     bool              `json:"enabled"`
	Provider    ModelProviderKind `json:"provider"`
	BaseURL     string            `json:"base_url"`
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens"`
	APIKey      string            `json:"api_key"`
	ClearAPIKey bool              `json:"clear_api_key"`
}

// ModelConnectionTest 连通性探测请求。可以带一个尚未保存的 Key，
// 这样"先测再存"成立，不必为了测试先把 Key 写进去。
type ModelConnectionTest struct {
	Provider  ModelProviderKind `json:"provider"`
	BaseURL   string            `json:"base_url"`
	Model     string            `json:"model"`
	APIKey    string            `json:"api_key"`
	MaxTokens int               `json:"max_tokens"`
}

// UpstreamModel 上游返回的模型条目。/models 只取 id 与所有者，
// 不把上游的完整元数据透传给前端。
type UpstreamModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}
