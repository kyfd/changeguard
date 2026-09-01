package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func newEnterpriseTestManager() (*Manager, *store.Store) {
	data := store.NewMemory()
	manager := New(Config{
		Mode:         "local",
		PublicURL:    "https://dbguard.example.com",
		SessionTTL:   time.Hour,
		HTTPTimeout:  time.Second,
		SecureCookie: true,
	}, data, log.New(io.Discard, "", 0))
	return manager, data
}

func performEnterpriseRequest(handler http.Handler, method, target string, body any, cookie *http.Cookie, csrfToken string) *httptest.ResponseRecorder {
	var content io.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		content = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, target, content)
	request.Header.Set("Content-Type", "application/json")
	if csrfToken != "" {
		request.Header.Set("X-CSRF-Token", csrfToken)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sessionCookie(response *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	return nil
}

func TestEnterpriseRegistrationInvitationAndRoleAssignment(t *testing.T) {
	manager, data := newEnterpriseTestManager()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/register", manager.HandleRegisterEnterprise)
	mux.HandleFunc("/api/auth/invitations/accept", manager.HandleAcceptInvite)
	mux.HandleFunc("/api/enterprise/invites", manager.HandleInvites)
	mux.HandleFunc("/api/enterprise/members/", manager.HandleMember)
	handler := manager.Middleware(mux)

	register := performEnterpriseRequest(handler, http.MethodPost, "/api/auth/register", model.RegisterEnterpriseInput{
		OrganizationName: "星环科技",
		OrganizationSlug: "stellar-tech",
		Name:             "刘丰熙",
		Email:            "owner@stellar.example",
		Password:         "owner123",
	}, nil, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", register.Code, register.Body.String())
	}
	cookie := sessionCookie(register)
	if cookie == nil {
		t.Fatal("registration did not create a session cookie")
	}
	owner, err := data.UserByEmail("owner@stellar.example")
	if err != nil {
		t.Fatalf("owner not persisted: %v", err)
	}
	if !owner.EnterpriseAdmin || owner.Role != model.RoleOwner || !owner.Active {
		t.Fatalf("unexpected owner: %+v", owner)
	}

	csrfRejected := performEnterpriseRequest(handler, http.MethodPost, "/api/enterprise/invites", model.CreateInviteInput{
		Email: "csrf-check@stellar.example", Role: model.RoleDeveloper, ExpiresIn: 24,
	}, cookie, "")
	if csrfRejected.Code != http.StatusForbidden {
		t.Fatalf("state-changing request without CSRF token must be rejected, status=%d body=%s", csrfRejected.Code, csrfRejected.Body.String())
	}

	inviteResponse := performEnterpriseRequest(handler, http.MethodPost, "/api/enterprise/invites", model.CreateInviteInput{
		Email:     "reviewer@stellar.example",
		Role:      model.RoleReviewer,
		ExpiresIn: 72,
	}, cookie, testCSRFToken(manager, cookie))
	if inviteResponse.Code != http.StatusCreated {
		t.Fatalf("invite status = %d, body = %s", inviteResponse.Code, inviteResponse.Body.String())
	}
	var created model.InviteCreated
	if err := json.Unmarshal(inviteResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	if created.PlainToken == "" || created.InviteURL == "" {
		t.Fatalf("invite token must be returned once: %+v", created)
	}

	acceptResponse := performEnterpriseRequest(handler, http.MethodPost, "/api/auth/invitations/accept", model.AcceptInviteInput{
		Token:    created.PlainToken,
		Name:     "王审核",
		Email:    "reviewer@stellar.example",
		Password: "reviewer123",
	}, nil, "")
	if acceptResponse.Code != http.StatusCreated {
		t.Fatalf("accept status = %d, body = %s", acceptResponse.Code, acceptResponse.Body.String())
	}
	reviewer, err := data.UserByEmail("reviewer@stellar.example")
	if err != nil {
		t.Fatalf("reviewer not persisted: %v", err)
	}
	if reviewer.OrganizationID != owner.OrganizationID || reviewer.Role != model.RoleReviewer {
		t.Fatalf("invited member joined wrong tenant or role: %+v", reviewer)
	}

	updateResponse := performEnterpriseRequest(handler, http.MethodPut, "/api/enterprise/members/"+reviewer.ID, model.UpdateMemberInput{
		Role:            model.RoleOwner,
		Active:          true,
		EnterpriseAdmin: true,
	}, cookie, testCSRFToken(manager, cookie))
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("member update status = %d, body = %s", updateResponse.Code, updateResponse.Body.String())
	}
	updated, _ := data.User(reviewer.ID)
	if !updated.EnterpriseAdmin || updated.Role != model.RoleOwner {
		t.Fatalf("member role assignment not persisted: %+v", updated)
	}
}

func TestPasswordHashAndSSOLockoutGuard(t *testing.T) {
	salt, hash, err := hashPassword("safePass123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !verifyPassword("safePass123", salt, hash) {
		t.Fatal("correct password was rejected")
	}
	if verifyPassword("wrongPass123", salt, hash) {
		t.Fatal("wrong password was accepted")
	}

	manager, _ := newEnterpriseTestManager()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/register", manager.HandleRegisterEnterprise)
	mux.HandleFunc("/api/enterprise", manager.HandleOrganization)
	handler := manager.Middleware(mux)
	register := performEnterpriseRequest(handler, http.MethodPost, "/api/auth/register", model.RegisterEnterpriseInput{
		OrganizationName: "安全企业",
		OrganizationSlug: "secure-company",
		Name:             "企业管理员",
		Email:            "admin@secure.example",
		Password:         "admin1234",
	}, nil, "")
	cookie := sessionCookie(register)
	response := performEnterpriseRequest(handler, http.MethodPut, "/api/enterprise", model.UpdateOrganizationInput{
		Name:         "安全企业",
		EmailDomains: []string{"secure.example"},
		SSOEnforced:  true,
	}, cookie, testCSRFToken(manager, cookie))
	if response.Code != http.StatusConflict {
		t.Fatalf("SSO must not be enforced before OIDC and an SSO admin are ready: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSafeNextRejectsExternalRedirect(t *testing.T) {
	cases := map[string]string{
		"":                        "/",
		"https://evil.example":    "/",
		"//evil.example/path":     "/",
		"/dashboard?view=pending": "/dashboard?view=pending",
	}
	for input, expected := range cases {
		if actual := safeNext(input); actual != expected {
			t.Fatalf("safeNext(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func testCSRFToken(manager *Manager, cookie *http.Cookie) string {
	value, err := manager.sessionStore.Get(context.Background(), tokenKey(cookie.Value))
	if err != nil {
		return ""
	}
	return value.CSRFToken
}

func TestDefaultAuthModeRequiresLogin(t *testing.T) {
	t.Setenv("DBGUARD_AUTH_MODE", "")
	config := FromEnvironment()
	if config.Mode != "local" {
		t.Fatalf("default auth mode = %q, want local", config.Mode)
	}
}

func TestAuthenticationRejectsTrailingJSON(t *testing.T) {
	manager, _ := newEnterpriseTestManager()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(
		`{"organization_name":"测试企业","organization_slug":"test-company","name":"管理员","email":"owner@test.example","password":"owner123"} {}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.HandleRegisterEnterprise(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLogoutDeletesSessionAndReturnsToLoginPage(t *testing.T) {
	manager, _ := newEnterpriseTestManager()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/register", manager.HandleRegisterEnterprise)
	mux.HandleFunc("/api/auth/session", manager.HandleSession)
	mux.HandleFunc("/auth/logout", manager.HandleLogout)
	handler := manager.Middleware(mux)

	register := performEnterpriseRequest(handler, http.MethodPost, "/api/auth/register", model.RegisterEnterpriseInput{
		OrganizationName: "退出测试企业", OrganizationSlug: "logout-company", Name: "管理员",
		Email: "owner@logout.example", Password: "owner123",
	}, nil, "")
	cookie := sessionCookie(register)
	if register.Code != http.StatusCreated || cookie == nil {
		t.Fatalf("registration failed: status=%d body=%s", register.Code, register.Body.String())
	}
	var session model.AuthSession
	if err := json.Unmarshal(register.Body.Bytes(), &session); err != nil || session.CSRFToken == "" {
		t.Fatalf("registration did not return csrf token: err=%v body=%s", err, register.Body.String())
	}
	getLogout := performEnterpriseRequest(handler, http.MethodGet, "/auth/logout", nil, cookie, "")
	if getLogout.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout must be rejected, got %d", getLogout.Code)
	}
	missingToken := performEnterpriseRequest(handler, http.MethodPost, "/auth/logout", nil, cookie, "")
	if missingToken.Code != http.StatusForbidden {
		t.Fatalf("logout without csrf token must be rejected, got %d", missingToken.Code)
	}
	logout := performEnterpriseRequest(handler, http.MethodPost, "/auth/logout", nil, cookie, session.CSRFToken)
	if logout.Code != http.StatusFound || logout.Header().Get("Location") != "/" {
		t.Fatalf("unexpected logout response: status=%d location=%q", logout.Code, logout.Header().Get("Location"))
	}
	oldSession := performEnterpriseRequest(handler, http.MethodGet, "/api/auth/session", nil, cookie, "")
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("old session must be invalid after logout: status=%d body=%s", oldSession.Code, oldSession.Body.String())
	}
}

func TestSecureCookieDefaultsFromPublicHTTPSURL(t *testing.T) {
	t.Setenv("DBGUARD_PUBLIC_URL", "https://dbguard.example.com")
	t.Setenv("DBGUARD_OIDC_REDIRECT_URL", "")
	t.Setenv("DBGUARD_AUTH_SECURE_COOKIE", "")
	if config := FromEnvironment(); !config.SecureCookie {
		t.Fatal("HTTPS public URL must enable secure session cookies by default")
	}
	t.Setenv("DBGUARD_AUTH_SECURE_COOKIE", "not-a-boolean")
	if config := FromEnvironment(); !config.SecureCookie {
		t.Fatal("invalid cookie override must not silently disable HTTPS security")
	}
}

func TestDemoCredentialsRequireExplicitFlag(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_ACCOUNTS", "")
	if _, err := store.NewMemory().Credential("usr_owner"); err == nil {
		t.Fatal("demo credential must not be created unless explicitly enabled")
	}

	t.Setenv("DBGUARD_ENABLE_DEMO_ACCOUNTS", "true")
	data := store.NewMemory()
	for _, userID := range []string{"usr_developer", "usr_reviewer", "usr_owner"} {
		credential, err := data.Credential(userID)
		if err != nil {
			t.Fatalf("credential %s missing: %v", userID, err)
		}
		if !verifyPassword("Demo1234", credential.PasswordSalt, credential.PasswordHash) {
			t.Fatalf("credential %s does not accept demo password", userID)
		}
		if verifyPassword("wrong-password", credential.PasswordSalt, credential.PasswordHash) {
			t.Fatalf("credential %s accepts an invalid password", userID)
		}
	}
}

func TestSSOBindingCannotBeOverwrittenByEmailMatch(t *testing.T) {
	data := store.NewMemory()
	var candidate model.User
	for _, user := range data.Users() {
		if user.Email != "" {
			candidate = user
			break
		}
	}
	if candidate.ID == "" {
		t.Fatal("seed data must include a user with email")
	}
	first := model.User{
		Name: candidate.Name, Email: candidate.Email,
		IdentityProvider: "https://identity.example.com", Subject: "subject-one",
	}
	if _, err := data.UpsertSSOUser(first, model.AuditEvent{
		ID: store.NewID("audit_"), Action: "SSO_LOGIN", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("initial SSO binding failed: %v", err)
	}
	second := first
	second.Subject = "subject-two"
	if _, err := data.UpsertSSOUser(second, model.AuditEvent{
		ID: store.NewID("audit_"), Action: "SSO_LOGIN", CreatedAt: time.Now(),
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("existing email binding must not be overwritten by another subject, got %v", err)
	}
}
