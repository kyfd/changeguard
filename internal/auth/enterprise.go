package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

const passwordIterations = 210000

func (m *Manager) SessionAuthEnabled() bool {
	return m.config.Mode == "local" || m.config.Mode == "oidc" || m.config.Mode == "hybrid"
}

func (m *Manager) OIDCEnabled() bool {
	return (m.config.Mode == "oidc" || m.config.Mode == "hybrid") &&
		m.config.Issuer != "" && m.config.ClientID != "" && m.config.RedirectURL != ""
}

func (m *Manager) HandleRegisterEnterprise(w http.ResponseWriter, r *http.Request) {
	if m.config.Mode == "oidc" {
		writeAuthError(w, http.StatusForbidden, "纯 SSO 模式不开放密码注册，请由平台管理员预置企业后通过身份平台加入")
		return
	}
	if r.Method != http.MethodPost {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	allowed, retryAfter, limitErr := m.sessionStore.AllowAttempt(r.Context(), tokenKey("register|"+clientAddress(r)), 5, time.Hour)
	if limitErr != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "注册安全服务暂时不可用")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(maxInt(1, int(retryAfter.Seconds()))))
		writeAuthError(w, http.StatusTooManyRequests, "当前来源创建企业过于频繁，请稍后再试")
		return
	}
	var input model.RegisterEnterpriseInput
	if err := decodeAuthJSON(r, &input); err != nil {
		writeAuthError(w, http.StatusBadRequest, "注册参数格式不正确")
		return
	}
	input.OrganizationName = strings.TrimSpace(input.OrganizationName)
	input.OrganizationSlug = strings.ToLower(strings.TrimSpace(input.OrganizationSlug))
	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if len([]rune(input.OrganizationName)) > 100 || len([]rune(input.Name)) > 80 ||
		input.OrganizationName == "" || input.Name == "" || !validSlug(input.OrganizationSlug) {
		writeAuthError(w, http.StatusBadRequest, "请填写企业名称、管理员姓名和合法的企业标识")
		return
	}
	if !validEmail(input.Email) || !validPassword(input.Password) {
		writeAuthError(w, http.StatusBadRequest, "邮箱格式不正确，或密码未达到至少 8 位且包含字母和数字的要求")
		return
	}
	salt, passwordHash, err := hashPassword(input.Password)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "无法安全保存登录凭据")
		return
	}
	now := time.Now()
	organizationID := store.NewID("org_")
	userID := store.NewID("usr_")
	domain := emailDomain(input.Email)
	organization := model.Organization{
		ID: organizationID, Name: input.OrganizationName, Slug: input.OrganizationSlug,
		EmailDomains: []string{domain}, AllowDomainJoin: false, SSOEnforced: false,
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	user := model.User{
		ID: userID, OrganizationID: organizationID, OrganizationName: organization.Name,
		Name: input.Name, Email: input.Email, Role: model.RoleOwner,
		EnterpriseAdmin: true, Active: true, LastLoginAt: &now,
	}
	credential := model.UserCredential{UserID: userID, PasswordSalt: salt, PasswordHash: passwordHash}
	policies := model.DefaultRiskPolicies(now)
	for index := range policies {
		policies[index].ID = store.NewID("pol_")
		policies[index].OrganizationID = organizationID
	}
	audit := model.AuditEvent{
		OrganizationID: organizationID, ID: store.NewID("audit_"), ActorID: userID,
		ActorName: user.Name, Action: "REGISTER_ENTERPRISE",
		Detail: "创建企业工作空间并成为企业管理员", CreatedAt: now,
	}
	if err := m.store.CreateEnterprise(organization, user, credential, policies, audit); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeAuthError(w, http.StatusConflict, "企业标识或管理员邮箱已经被使用")
			return
		}
		writeAuthError(w, http.StatusInternalServerError, "企业注册失败")
		return
	}
	csrfToken, sessionErr := m.createSession(w, user.ID)
	if sessionErr != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "企业已创建，但会话服务暂时不可用；恢复后请直接使用邮箱密码登录，无需重复注册")
		return
	}
	writeAuthJSON(w, http.StatusCreated, model.AuthSession{User: user, Organization: organization, CSRFToken: csrfToken})
}

func (m *Manager) HandleLocalLogin(w http.ResponseWriter, r *http.Request) {
	if m.config.Mode == "oidc" {
		writeAuthError(w, http.StatusForbidden, "当前部署只允许企业 SSO 登录")
		return
	}
	if r.Method != http.MethodPost {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	var input model.LoginInput
	if err := decodeAuthJSON(r, &input); err != nil {
		writeAuthError(w, http.StatusBadRequest, "登录参数格式不正确")
		return
	}
	normalizedEmail := strings.ToLower(strings.TrimSpace(input.Email))
	attemptKey := tokenKey(normalizedEmail + "|" + clientAddress(r))
	allowed, retryAfter, limitErr := m.sessionStore.AllowAttempt(r.Context(), attemptKey, 8, 15*time.Minute)
	if limitErr != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "登录安全服务暂时不可用")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(maxInt(1, int(retryAfter.Seconds()))))
		writeAuthError(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试")
		return
	}
	user, err := m.store.UserByEmail(normalizedEmail)
	if err != nil || !user.Active {
		writeAuthError(w, http.StatusUnauthorized, "邮箱或密码不正确")
		return
	}
	organization, err := m.store.Organization(user.OrganizationID)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "企业工作空间不存在")
		return
	}
	if organization.SSOEnforced {
		writeAuthError(w, http.StatusForbidden, "该企业已强制使用 SSO，请从企业身份平台登录")
		return
	}
	credential, err := m.store.Credential(user.ID)
	if err != nil || credential.PasswordHash == "" || !verifyPassword(input.Password, credential.PasswordSalt, credential.PasswordHash) {
		writeAuthError(w, http.StatusUnauthorized, "邮箱或密码不正确")
		return
	}
	now := time.Now()
	user, err = m.store.UpdateIdentityLogin(user.ID, func(current *model.User) {
		current.LastLoginAt = &now
	}, func(current *model.UserCredential) {}, model.AuditEvent{
		OrganizationID: user.OrganizationID, ID: store.NewID("audit_"),
		ActorID: user.ID, ActorName: user.Name, Action: "LOCAL_LOGIN",
		Detail: "使用企业邮箱和密码登录", CreatedAt: now,
	})
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "登录状态保存失败")
		return
	}
	_ = m.sessionStore.ResetAttempts(r.Context(), attemptKey)
	csrfToken, sessionErr := m.createSession(w, user.ID)
	if sessionErr != nil {
		writeAuthError(w, http.StatusInternalServerError, "无法创建登录会话")
		return
	}
	writeAuthJSON(w, http.StatusOK, model.AuthSession{User: user, Organization: organization, CSRFToken: csrfToken})
}

func (m *Manager) HandleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	userID, ok := ActorID(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "尚未登录")
		return
	}
	user, err := m.store.User(userID)
	if err != nil || !user.Active {
		writeAuthError(w, http.StatusUnauthorized, "当前成员已停用")
		return
	}
	organization, err := m.store.Organization(user.OrganizationID)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "企业工作空间不存在")
		return
	}
	value, _ := m.sessionData(r)
	writeAuthJSON(w, http.StatusOK, model.AuthSession{User: user, Organization: organization, CSRFToken: value.CSRFToken})
}

func (m *Manager) HandleOrganization(w http.ResponseWriter, r *http.Request) {
	user, organization, ok := m.currentEnterprise(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeAuthJSON(w, http.StatusOK, organization)
	case http.MethodPut:
		if !user.EnterpriseAdmin {
			writeAuthError(w, http.StatusForbidden, "只有企业管理员可以修改企业设置")
			return
		}
		var input model.UpdateOrganizationInput
		if err := decodeAuthJSON(r, &input); err != nil {
			writeAuthError(w, http.StatusBadRequest, "企业设置格式不正确")
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			writeAuthError(w, http.StatusBadRequest, "企业名称不能为空")
			return
		}
		domains := normalizeDomains(input.EmailDomains)
		if input.AllowDomainJoin && len(domains) == 0 {
			writeAuthError(w, http.StatusBadRequest, "启用同域名自动加入前，至少配置一个企业邮箱域名")
			return
		}
		for _, domain := range domains {
			if input.AllowDomainJoin && m.store.DomainClaimedByOther(organization.ID, domain) {
				writeAuthError(w, http.StatusConflict, "邮箱域名 "+domain+" 已被其他企业用于自动加入")
				return
			}
		}
		if input.SSOEnforced && !organization.SSOEnforced {
			if !m.OIDCEnabled() {
				writeAuthError(w, http.StatusConflict, "当前部署尚未配置 OIDC 身份平台，不能强制 SSO")
				return
			}
			if !m.store.HasActiveSSOAdmin(organization.ID) {
				writeAuthError(w, http.StatusConflict, "请先由至少一名企业管理员通过 SSO 登录完成身份绑定，再启用强制 SSO")
				return
			}
		}
		updated, err := m.store.UpdateOrganization(organization.ID, func(current *model.Organization) error {
			current.Name = name
			current.EmailDomains = domains
			current.AllowDomainJoin = input.AllowDomainJoin
			current.SSOEnforced = input.SSOEnforced
			return nil
		}, model.AuditEvent{
			OrganizationID: organization.ID, ID: store.NewID("audit_"), ActorID: user.ID,
			ActorName: user.Name, Action: "UPDATE_ORGANIZATION",
			Detail: "更新企业名称、邮箱域名和 SSO 策略", CreatedAt: time.Now(),
		})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeAuthError(w, http.StatusConflict, "企业邮箱域名已被其他工作空间用于自动加入")
				return
			}
			writeAuthError(w, http.StatusInternalServerError, "企业设置保存失败")
			return
		}
		writeAuthJSON(w, http.StatusOK, updated)
	default:
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
	}
}

func (m *Manager) HandleMembers(w http.ResponseWriter, r *http.Request) {
	_, organization, ok := m.currentEnterprise(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	writeAuthJSON(w, http.StatusOK, m.store.UsersByOrganization(organization.ID))
}

func (m *Manager) HandleMember(w http.ResponseWriter, r *http.Request) {
	user, organization, ok := m.currentEnterprise(w, r)
	if !ok {
		return
	}
	if !user.EnterpriseAdmin {
		writeAuthError(w, http.StatusForbidden, "只有企业管理员可以管理成员")
		return
	}
	memberID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/enterprise/members/"), "/")
	if memberID == "" {
		writeAuthError(w, http.StatusNotFound, "成员不存在")
		return
	}
	member, err := m.store.User(memberID)
	if err != nil || member.OrganizationID != organization.ID {
		writeAuthError(w, http.StatusNotFound, "成员不存在")
		return
	}
	if r.Method == http.MethodGet {
		writeAuthJSON(w, http.StatusOK, model.MemberAccess{User: member, ApplicationGrants: m.store.ApplicationGrantsByUser(organization.ID, member.ID)})
		return
	}
	if r.Method != http.MethodPut {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	var input model.UpdateMemberInput
	if err := decodeAuthJSON(r, &input); err != nil || !validRole(input.Role) {
		writeAuthError(w, http.StatusBadRequest, "成员角色不正确")
		return
	}
	if member.ID == user.ID && (!input.Active || !input.EnterpriseAdmin) {
		writeAuthError(w, http.StatusConflict, "不能停用当前登录账号或撤销自己的企业管理员身份")
		return
	}
	if member.EnterpriseAdmin && (!input.Active || !input.EnterpriseAdmin) && m.store.ActiveEnterpriseAdminCount(organization.ID) <= 1 {
		writeAuthError(w, http.StatusConflict, "企业至少需要保留一名启用状态的企业管理员")
		return
	}
	updated, err := m.store.UpdateMember(organization.ID, memberID, func(current *model.User) error {
		current.Role = input.Role
		current.Active = input.Active
		current.EnterpriseAdmin = input.EnterpriseAdmin
		return nil
	}, input.ApplicationGrants, model.AuditEvent{
		OrganizationID: organization.ID, ID: store.NewID("audit_"), ActorID: user.ID,
		ActorName: user.Name, Action: "UPDATE_MEMBER",
		Detail:    fmt.Sprintf("调整成员 %s 的角色为 %s，启用状态为 %t，企业管理员为 %t", member.Name, input.Role, input.Active, input.EnterpriseAdmin),
		CreatedAt: time.Now(),
	})
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "成员信息保存失败")
		return
	}
	writeAuthJSON(w, http.StatusOK, model.MemberAccess{User: updated, ApplicationGrants: m.store.ApplicationGrantsByUser(organization.ID, updated.ID)})
}

func (m *Manager) HandleInvites(w http.ResponseWriter, r *http.Request) {
	user, organization, ok := m.currentEnterprise(w, r)
	if !ok {
		return
	}
	if !user.EnterpriseAdmin {
		writeAuthError(w, http.StatusForbidden, "只有企业管理员可以管理邀请")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeAuthJSON(w, http.StatusOK, m.store.InvitesByOrganization(organization.ID))
	case http.MethodPost:
		var input model.CreateInviteInput
		if err := decodeAuthJSON(r, &input); err != nil {
			writeAuthError(w, http.StatusBadRequest, "邀请参数格式不正确")
			return
		}
		email := strings.ToLower(strings.TrimSpace(input.Email))
		if !validEmail(email) || !validRole(input.Role) {
			writeAuthError(w, http.StatusBadRequest, "邀请邮箱或角色不正确")
			return
		}
		if existing, err := m.store.UserByEmail(email); err == nil {
			if existing.OrganizationID == organization.ID {
				writeAuthError(w, http.StatusConflict, "该邮箱已经是企业成员")
			} else {
				writeAuthError(w, http.StatusConflict, "该邮箱已经加入其他企业")
			}
			return
		}
		hours := input.ExpiresIn
		if hours <= 0 {
			hours = 72
		}
		if hours > 720 {
			hours = 720
		}
		rawToken, err := randomToken(32)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "无法创建邀请令牌")
			return
		}
		now := time.Now()
		invite := model.OrganizationInvite{
			ID: store.NewID("inv_"), OrganizationID: organization.ID, OrganizationName: organization.Name,
			Email: email, Role: input.Role, TokenHash: tokenKey(rawToken), Status: model.InvitePending,
			ExpiresAt: now.Add(time.Duration(hours) * time.Hour), CreatedByID: user.ID,
			CreatedByName: user.Name, CreatedAt: now,
		}
		if err := m.store.CreateInvite(invite, model.AuditEvent{
			OrganizationID: organization.ID, ID: store.NewID("audit_"), ActorID: user.ID,
			ActorName: user.Name, Action: "CREATE_INVITE",
			Detail: fmt.Sprintf("邀请 %s 以 %s 身份加入企业", email, input.Role), CreatedAt: now,
		}); err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeAuthError(w, http.StatusConflict, "该邮箱已经有一条有效邀请")
				return
			}
			writeAuthError(w, http.StatusInternalServerError, "邀请创建失败")
			return
		}
		baseURL := m.config.PublicURL
		if baseURL == "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			baseURL = scheme + "://" + r.Host
		}
		inviteURL := strings.TrimRight(baseURL, "/") + "/?invite=" + url.QueryEscape(rawToken)
		writeAuthJSON(w, http.StatusCreated, model.InviteCreated{Invite: invite, InviteURL: inviteURL, PlainToken: rawToken})
	default:
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
	}
}

func (m *Manager) HandleInvite(w http.ResponseWriter, r *http.Request) {
	user, organization, ok := m.currentEnterprise(w, r)
	if !ok {
		return
	}
	if !user.EnterpriseAdmin {
		writeAuthError(w, http.StatusForbidden, "只有企业管理员可以撤销邀请")
		return
	}
	inviteID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/enterprise/invites/"), "/")
	if inviteID == "" || r.Method != http.MethodDelete {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	err := m.store.RevokeInvite(organization.ID, inviteID, model.AuditEvent{
		OrganizationID: organization.ID, ID: store.NewID("audit_"), ActorID: user.ID,
		ActorName: user.Name, Action: "REVOKE_INVITE", Detail: "撤销企业成员邀请", CreatedAt: time.Now(),
	})
	if err != nil {
		writeAuthError(w, http.StatusNotFound, "有效邀请不存在")
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (m *Manager) HandleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	if m.config.Mode == "oidc" {
		writeAuthError(w, http.StatusForbidden, "当前部署只允许通过企业 SSO 接受邀请")
		return
	}
	if r.Method != http.MethodPost {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	allowed, retryAfter, limitErr := m.sessionStore.AllowAttempt(r.Context(), tokenKey("invite|"+clientAddress(r)), 12, 15*time.Minute)
	if limitErr != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "邀请安全服务暂时不可用")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(maxInt(1, int(retryAfter.Seconds()))))
		writeAuthError(w, http.StatusTooManyRequests, "邀请验证尝试过于频繁，请稍后再试")
		return
	}
	var input model.AcceptInviteInput
	if err := decodeAuthJSON(r, &input); err != nil {
		writeAuthError(w, http.StatusBadRequest, "邀请加入参数格式不正确")
		return
	}
	invite, err := m.store.InviteByTokenHash(tokenKey(strings.TrimSpace(input.Token)))
	if err != nil {
		writeAuthError(w, http.StatusNotFound, "邀请不存在、已使用或已过期")
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if !strings.EqualFold(email, invite.Email) {
		writeAuthError(w, http.StatusForbidden, "注册邮箱必须与受邀邮箱一致")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || !validPassword(input.Password) {
		writeAuthError(w, http.StatusBadRequest, "请填写姓名，并使用至少 8 位且包含字母和数字的密码")
		return
	}
	salt, passwordHash, err := hashPassword(input.Password)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "无法安全保存登录凭据")
		return
	}
	organization, err := m.store.Organization(invite.OrganizationID)
	if err != nil {
		writeAuthError(w, http.StatusNotFound, "邀请对应的企业不存在")
		return
	}
	now := time.Now()
	user := model.User{
		ID: store.NewID("usr_"), OrganizationID: organization.ID, OrganizationName: organization.Name,
		Name: name, Email: email, Role: invite.Role, Active: true, LastLoginAt: &now,
	}
	credential := model.UserCredential{UserID: user.ID, PasswordSalt: salt, PasswordHash: passwordHash}
	user, err = m.store.AcceptInvite(invite.ID, user, credential, model.AuditEvent{
		OrganizationID: organization.ID, ID: store.NewID("audit_"), ActorID: user.ID,
		ActorName: user.Name, Action: "ACCEPT_INVITE",
		Detail: fmt.Sprintf("接受邀请并以 %s 身份加入企业", user.Role), CreatedAt: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeAuthError(w, http.StatusConflict, "该邮箱已经注册")
			return
		}
		writeAuthError(w, http.StatusInternalServerError, "加入企业失败")
		return
	}
	csrfToken, sessionErr := m.createSession(w, user.ID)
	if sessionErr != nil {
		writeAuthError(w, http.StatusInternalServerError, "加入成功，但登录会话创建失败")
		return
	}
	writeAuthJSON(w, http.StatusCreated, model.AuthSession{User: user, Organization: organization, CSRFToken: csrfToken})
}

func (m *Manager) createSession(w http.ResponseWriter, userID string) (string, error) {
	rawSession, err := randomToken(32)
	if err != nil {
		return "", err
	}
	csrfToken, err := randomToken(24)
	if err != nil {
		return "", err
	}
	value := session{UserID: userID, CSRFToken: csrfToken, ExpiresAt: time.Now().Add(m.config.SessionTTL)}
	if err := m.sessionStore.Put(context.Background(), tokenKey(rawSession), value, m.config.SessionTTL); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: rawSession, Path: "/",
		MaxAge: int(m.config.SessionTTL.Seconds()), HttpOnly: true,
		Secure: m.config.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	return csrfToken, nil
}
func (m *Manager) currentEnterprise(w http.ResponseWriter, r *http.Request) (model.User, model.Organization, bool) {
	userID, ok := ActorID(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "尚未登录")
		return model.User{}, model.Organization{}, false
	}
	user, err := m.store.User(userID)
	if err != nil || !user.Active {
		writeAuthError(w, http.StatusUnauthorized, "当前成员不存在或已停用")
		return model.User{}, model.Organization{}, false
	}
	organization, err := m.store.Organization(user.OrganizationID)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "企业工作空间不存在")
		return model.User{}, model.Organization{}, false
	}
	return user, organization, true
}

func hashPassword(password string) (string, string, error) {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", "", err
	}
	derived := pbkdf2SHA256([]byte(password), saltBytes, passwordIterations, 32)
	return base64.RawStdEncoding.EncodeToString(saltBytes), base64.RawStdEncoding.EncodeToString(derived), nil
}

func verifyPassword(password, encodedSalt, encodedHash string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(encodedSalt)
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(encodedHash)
	if err != nil || len(expected) == 0 {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, passwordIterations, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	hashLength := 32
	blocks := (keyLength + hashLength - 1) / hashLength
	result := make([]byte, 0, blocks*hashLength)
	for block := 1; block <= blocks; block++ {
		counter := make([]byte, 4)
		binary.BigEndian.PutUint32(counter, uint32(block))
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(counter)
		previous := mac.Sum(nil)
		accumulator := append([]byte(nil), previous...)
		for iteration := 1; iteration < iterations; iteration++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(previous)
			previous = mac.Sum(nil)
			for index := range accumulator {
				accumulator[index] ^= previous[index]
			}
		}
		result = append(result, accumulator...)
	}
	return result[:keyLength]
}

const maxAuthJSONBodyBytes int64 = 2 << 20

func decodeAuthJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	limited := &io.LimitedReader{R: r.Body, N: maxAuthJSONBodyBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if limited.N <= 0 {
			return errors.New("请求体超过 2 MiB 限制")
		}
		return err
	}
	trailingErr := decoder.Decode(&struct{}{})
	if limited.N <= 0 {
		return errors.New("请求体超过 2 MiB 限制")
	}
	if !errors.Is(trailingErr, io.EOF) {
		if trailingErr == nil {
			return errors.New("请求体只能包含一个 JSON 对象")
		}
		return trailingErr
	}
	return nil
}
func validEmail(value string) bool {
	if len(value) == 0 || len(value) > 254 {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && strings.EqualFold(address.Address, strings.TrimSpace(value)) && strings.Contains(value, "@")
}

func validSlug(value string) bool {
	return regexp.MustCompile("^[a-z0-9][a-z0-9-]{2,39}$").MatchString(value)
}

func validPassword(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	hasLetter, hasNumber := false, false
	for _, item := range value {
		if item >= '0' && item <= '9' {
			hasNumber = true
		}
		if (item >= 'a' && item <= 'z') || (item >= 'A' && item <= 'Z') {
			hasLetter = true
		}
	}
	return hasLetter && hasNumber
}

func validRole(value string) bool {
	return value == model.RoleDeveloper || value == model.RoleReviewer || value == model.RoleOwner
}

func emailDomain(email string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(email)), "@")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func normalizeDomains(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(value, "@")))
		if value == "" || !strings.Contains(value, ".") || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
