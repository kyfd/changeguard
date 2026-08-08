package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

const sessionCookieName = "dbguard_session"
const stateCookieName = "dbguard_oidc_state"

type Config struct {
	Mode                 string
	Issuer               string
	ClientID             string
	ClientSecret         string
	RedirectURL          string
	PublicURL            string
	Scopes               []string
	RoleClaim            string
	OwnerValues          []string
	ReviewerValues       []string
	AllowedValues        []string
	AllowedDomains       []string
	TokenAuthMethod      string
	SessionTTL           time.Duration
	HTTPTimeout          time.Duration
	SecureCookie         bool
	RequireVerifiedEmail bool
}

type Status struct {
	Mode         string `json:"mode"`
	Enabled      bool   `json:"enabled"`
	OIDCEnabled  bool   `json:"oidc_enabled"`
	LocalEnabled bool   `json:"local_enabled"`
	Provider     string `json:"provider"`
	SessionTTL   string `json:"session_ttl"`
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

type loginFlow struct {
	Nonce        string
	CodeVerifier string
	Next         string
	ExpiresAt    time.Time
}

type session struct {
	UserID    string    `json:"user_id"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

type actorContextKey struct{}

type Manager struct {
	config Config
	store  *store.Store
	logger *log.Logger
	client *http.Client

	mu              sync.Mutex
	sessionStore    sessionRepository
	discovery       discoveryDocument
	discoveryExpiry time.Time
	keys            jwkSet
	keysExpiry      time.Time
}

func FromEnvironment() Config {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_AUTH_MODE")))
	switch mode {
	case "local", "oidc", "hybrid":
	case "":
		mode = "local"
	default:
		// Authentication configuration must never fail open. Unknown values
		// fall back to local session authentication instead of trusting headers.
		mode = "local"
	}
	redirectURL := envOr("DBGUARD_OIDC_REDIRECT_URL", "http://localhost:8080/auth/callback")
	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DBGUARD_PUBLIC_URL")), "/")
	secureCookie := strings.HasPrefix(strings.ToLower(redirectURL), "https://") ||
		strings.HasPrefix(strings.ToLower(publicURL), "https://")
	if value := strings.TrimSpace(os.Getenv("DBGUARD_AUTH_SECURE_COOKIE")); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			secureCookie = parsed
		}
	}
	return Config{
		Mode:                 mode,
		Issuer:               strings.TrimRight(strings.TrimSpace(os.Getenv("DBGUARD_OIDC_ISSUER")), "/"),
		ClientID:             strings.TrimSpace(os.Getenv("DBGUARD_OIDC_CLIENT_ID")),
		ClientSecret:         strings.TrimSpace(os.Getenv("DBGUARD_OIDC_CLIENT_SECRET")),
		RedirectURL:          redirectURL,
		PublicURL:            publicURL,
		Scopes:               splitValues(envOr("DBGUARD_OIDC_SCOPES", "openid profile email groups")),
		RoleClaim:            envOr("DBGUARD_OIDC_ROLE_CLAIM", "groups"),
		OwnerValues:          splitValues(os.Getenv("DBGUARD_OIDC_OWNER_VALUES")),
		ReviewerValues:       splitValues(os.Getenv("DBGUARD_OIDC_REVIEWER_VALUES")),
		AllowedValues:        splitValues(os.Getenv("DBGUARD_OIDC_ALLOWED_VALUES")),
		AllowedDomains:       splitValues(os.Getenv("DBGUARD_OIDC_ALLOWED_DOMAINS")),
		TokenAuthMethod:      envOr("DBGUARD_OIDC_TOKEN_AUTH_METHOD", "client_secret_post"),
		SessionTTL:           durationOr("DBGUARD_AUTH_SESSION_TTL", 12*time.Hour),
		HTTPTimeout:          durationOr("DBGUARD_OIDC_HTTP_TIMEOUT", 8*time.Second),
		SecureCookie:         secureCookie,
		RequireVerifiedEmail: boolOr("DBGUARD_OIDC_REQUIRE_VERIFIED_EMAIL", true),
	}
}

func New(config Config, data *store.Store, logger *log.Logger) *Manager {
	if config.SessionTTL <= 0 {
		config.SessionTTL = 12 * time.Hour
	}
	if config.HTTPTimeout <= 0 {
		config.HTTPTimeout = 8 * time.Second
	}
	if len(config.Scopes) == 0 {
		config.Scopes = []string{"openid", "profile", "email"}
	}
	if strings.TrimSpace(config.RoleClaim) == "" {
		config.RoleClaim = "groups"
	}
	return &Manager{
		config: config, store: data, logger: logger,
		client:       &http.Client{Timeout: config.HTTPTimeout},
		sessionStore: newSessionRepository(logger),
	}
}

func (m *Manager) Enabled() bool {
	return m.SessionAuthEnabled()
}

func (m *Manager) Status() Status {
	provider := ""
	if parsed, err := url.Parse(m.config.Issuer); err == nil {
		provider = parsed.Host
	}
	return Status{Mode: m.config.Mode, Enabled: m.Enabled(), OIDCEnabled: m.OIDCEnabled(), LocalEnabled: m.config.Mode != "oidc", Provider: provider, SessionTTL: m.config.SessionTTL.String()}
}

func ActorID(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(actorContextKey{}).(string)
	return value, ok && value != ""
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.cleanup()
		if m.publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		value, ok, sessionErr := m.sessionData(r)
		if sessionErr != nil {
			m.logger.Printf("session lookup failed: %v", sessionErr)
			writeAuthError(w, http.StatusServiceUnavailable, "会话服务暂时不可用，请稍后重试")
			return
		}
		if ok {
			if !csrfValid(r, value.CSRFToken) {
				writeAuthError(w, http.StatusForbidden, "请求安全令牌无效，请刷新页面后重试")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, value.UserID)))
			return
		}
		if !m.Enabled() {
			actorID := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
			if actorID == "" {
				actorID = "usr_developer"
			}
			if user, err := m.store.User(actorID); err != nil || !user.Active {
				writeAuthError(w, http.StatusUnauthorized, "当前演示成员不存在")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actorID)))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeAuthError(w, http.StatusUnauthorized, "登录状态已失效，请重新登录")
			return
		}
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	})
}
func (m *Manager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	writeAuthJSON(w, http.StatusOK, m.Status())
}

func (m *Manager) HandleMe(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "登录用户不存在")
		return
	}
	writeAuthJSON(w, http.StatusOK, user)
}

func (m *Manager) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	if !m.OIDCEnabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	discovery, err := m.discover(r.Context(), false)
	if err != nil {
		m.redirectError(w, r, "身份平台暂时不可用")
		return
	}
	stateValue, err := randomToken(32)
	if err != nil {
		m.redirectError(w, r, "无法创建登录请求")
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		m.redirectError(w, r, "无法创建登录请求")
		return
	}
	verifier, err := randomToken(48)
	if err != nil {
		m.redirectError(w, r, "无法创建登录请求")
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	flow := loginFlow{Nonce: nonce, CodeVerifier: verifier, Next: next, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if err := m.sessionStore.PutLoginFlow(r.Context(), tokenKey(stateValue), flow, 10*time.Minute); err != nil {
		m.redirectError(w, r, "无法保存登录请求，请稍后重试")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: stateValue, Path: "/auth/callback",
		MaxAge: 600, HttpOnly: true, Secure: m.config.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {m.config.ClientID},
		"redirect_uri":          {m.config.RedirectURL},
		"scope":                 {strings.Join(m.config.Scopes, " ")},
		"state":                 {stateValue},
		"nonce":                 {nonce},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, discovery.AuthorizationEndpoint+"?"+query.Encode(), http.StatusFound)
}

func (m *Manager) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAuthError(w, http.StatusMethodNotAllowed, "请求方法不支持")
		return
	}
	if errorCode := strings.TrimSpace(r.URL.Query().Get("error")); errorCode != "" {
		m.redirectError(w, r, "身份平台拒绝了登录请求")
		return
	}
	stateValue := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateValue == "" || code == "" || stateCookie.Value != stateValue {
		m.redirectError(w, r, "登录校验失败，请重新发起登录")
		return
	}
	flow, flowErr := m.sessionStore.TakeLoginFlow(r.Context(), tokenKey(stateValue))
	m.clearStateCookie(w)
	if flowErr != nil || time.Now().After(flow.ExpiresAt) {
		m.redirectError(w, r, "登录请求已过期")
		return
	}
	discovery, err := m.discover(r.Context(), false)
	if err != nil {
		m.redirectError(w, r, "无法读取身份平台配置")
		return
	}
	token, err := m.exchangeCode(r.Context(), discovery, code, flow.CodeVerifier)
	if err != nil {
		m.redirectError(w, r, "身份平台令牌交换失败")
		return
	}
	claims, err := m.verifyIDToken(r.Context(), discovery, token.IDToken, flow.Nonce)
	if err != nil {
		m.logger.Printf("OIDC ID Token 校验失败: %v", err)
		m.redirectError(w, r, "身份令牌校验失败")
		return
	}
	identity, err := m.identityFromClaims(claims)
	if err != nil {
		m.redirectError(w, r, err.Error())
		return
	}
	user, err := m.store.UpsertSSOUser(identity, model.AuditEvent{
		ID: store.NewID("audit_"), ActorID: identity.ID, ActorName: identity.Name,
		Action: "SSO_LOGIN", Detail: "通过企业身份平台登录", CreatedAt: time.Now(),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			m.redirectError(w, r, "当前企业邮箱尚未收到邀请，也不符合已启用的企业域名加入策略")
		case errors.Is(err, store.ErrConflict):
			m.redirectError(w, r, "当前企业成员已停用或身份绑定发生冲突")
		default:
			m.redirectError(w, r, "无法同步企业成员信息")
		}
		return
	}
	if _, err := m.createSession(w, user.ID); err != nil {
		m.redirectError(w, r, "无法创建登录会话")
		return
	}
	http.Redirect(w, r, flow.Next, http.StatusFound)
}

func (m *Manager) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAuthError(w, http.StatusMethodNotAllowed, "退出登录必须使用 POST 请求")
		return
	}
	value, ok, sessionErr := m.sessionData(r)
	if sessionErr != nil {
		writeAuthError(w, http.StatusServiceUnavailable, "会话服务暂时不可用，请稍后重试")
		return
	}
	if ok {
		provided := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
		if provided == "" {
			if err := r.ParseForm(); err == nil {
				provided = strings.TrimSpace(r.FormValue("csrf_token"))
			}
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(value.CSRFToken)) != 1 {
			writeAuthError(w, http.StatusForbidden, "退出登录安全令牌无效")
			return
		}
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = m.sessionStore.Delete(r.Context(), tokenKey(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: m.config.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	if m.OIDCEnabled() {
		if discovery, err := m.discover(r.Context(), false); err == nil && discovery.EndSessionEndpoint != "" {
			redirectTarget := m.config.PublicURL
			if redirectTarget == "" {
				redirectTarget = "/"
			}
			query := url.Values{"client_id": {m.config.ClientID}, "post_logout_redirect_uri": {redirectTarget}}
			http.Redirect(w, r, discovery.EndSessionEndpoint+"?"+query.Encode(), http.StatusFound)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusFound)
}
func (m *Manager) discover(ctx context.Context, force bool) (discoveryDocument, error) {
	m.mu.Lock()
	if !force && m.discovery.AuthorizationEndpoint != "" && time.Now().Before(m.discoveryExpiry) {
		value := m.discovery
		m.mu.Unlock()
		return value, nil
	}
	m.mu.Unlock()
	var document discoveryDocument
	if err := m.getJSON(ctx, m.config.Issuer+"/.well-known/openid-configuration", &document); err != nil {
		return discoveryDocument{}, err
	}
	if document.Issuer != m.config.Issuer || document.AuthorizationEndpoint == "" || document.TokenEndpoint == "" || document.JWKSURI == "" {
		return discoveryDocument{}, errors.New("OIDC discovery document is incomplete or issuer does not match")
	}
	m.mu.Lock()
	m.discovery = document
	m.discoveryExpiry = time.Now().Add(6 * time.Hour)
	m.mu.Unlock()
	return document, nil
}

func (m *Manager) exchangeCode(ctx context.Context, discovery discoveryDocument, code, verifier string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {m.config.RedirectURL},
		"client_id":     {m.config.ClientID},
		"code_verifier": {verifier},
	}
	if m.config.ClientSecret != "" && m.config.TokenAuthMethod != "client_secret_basic" {
		form.Set("client_secret", m.config.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if m.config.ClientSecret != "" && m.config.TokenAuthMethod == "client_secret_basic" {
		request.SetBasicAuth(m.config.ClientID, m.config.ClientSecret)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return tokenResponse{}, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, err
	}
	var token tokenResponse
	if err := json.Unmarshal(content, &token); err != nil {
		return tokenResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || token.Error != "" || token.IDToken == "" {
		return tokenResponse{}, fmt.Errorf("token endpoint rejected request: %s %s", token.Error, token.Description)
	}
	return token, nil
}

func (m *Manager) verifyIDToken(ctx context.Context, discovery discoveryDocument, rawToken, expectedNonce string) (map[string]any, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("ID Token format is invalid")
	}
	headerContent, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	claimsContent, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	var header map[string]any
	var claims map[string]any
	if err := json.Unmarshal(headerContent, &header); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(claimsContent, &claims); err != nil {
		return nil, err
	}
	algorithm, _ := header["alg"].(string)
	keyID, _ := header["kid"].(string)
	if keyID == "" || (algorithm != "RS256" && algorithm != "RS384" && algorithm != "RS512") {
		return nil, errors.New("ID Token signature algorithm is not allowed")
	}
	publicKey, err := m.rsaKey(ctx, discovery.JWKSURI, keyID, false)
	if err != nil {
		publicKey, err = m.rsaKey(ctx, discovery.JWKSURI, keyID, true)
		if err != nil {
			return nil, err
		}
	}
	hashAlgorithm := crypto.SHA256
	switch algorithm {
	case "RS384":
		hashAlgorithm = crypto.SHA384
	case "RS512":
		hashAlgorithm = crypto.SHA512
	}
	var digest []byte
	signingInput := []byte(parts[0] + "." + parts[1])
	switch hashAlgorithm {
	case crypto.SHA256:
		sum := sha256.Sum256(signingInput)
		digest = sum[:]
	case crypto.SHA384:
		sum := sha512.Sum384(signingInput)
		digest = sum[:]
	case crypto.SHA512:
		sum := sha512.Sum512(signingInput)
		digest = sum[:]
	}
	if err := rsa.VerifyPKCS1v15(publicKey, hashAlgorithm, digest, signature); err != nil {
		return nil, errors.New("ID Token signature verification failed")
	}
	if err := m.validateClaims(claims, expectedNonce); err != nil {
		return nil, err
	}
	return claims, nil
}

func (m *Manager) rsaKey(ctx context.Context, jwksURL, keyID string, force bool) (*rsa.PublicKey, error) {
	m.mu.Lock()
	keys := m.keys
	valid := !force && len(keys.Keys) > 0 && time.Now().Before(m.keysExpiry)
	m.mu.Unlock()
	if !valid {
		if err := m.getJSON(ctx, jwksURL, &keys); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.keys = keys
		m.keysExpiry = time.Now().Add(time.Hour)
		m.mu.Unlock()
	}
	for _, key := range keys.Keys {
		if key.Kid != keyID || key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") {
			continue
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return nil, err
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			return nil, err
		}
		exponent := 0
		for _, value := range exponentBytes {
			exponent = exponent<<8 + int(value)
		}
		if exponent < 3 {
			return nil, errors.New("JWK exponent is invalid")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(modulusBytes), E: exponent}, nil
	}
	return nil, errors.New("matching JWK was not found")
}

func (m *Manager) validateClaims(claims map[string]any, expectedNonce string) error {
	issuer, _ := claims["iss"].(string)
	if issuer != m.config.Issuer {
		return errors.New("issuer claim does not match")
	}
	if !audienceContains(claims["aud"], m.config.ClientID) {
		return errors.New("audience claim does not include client ID")
	}
	if audienceLength(claims["aud"]) > 1 {
		authorizedParty, _ := claims["azp"].(string)
		if authorizedParty != m.config.ClientID {
			return errors.New("authorized party claim does not match")
		}
	}
	now := time.Now()
	expiresAt, ok := numberClaim(claims["exp"])
	if !ok || now.After(time.Unix(expiresAt, 0).Add(2*time.Minute)) {
		return errors.New("ID Token has expired")
	}
	if notBefore, ok := numberClaim(claims["nbf"]); ok && now.Add(2*time.Minute).Before(time.Unix(notBefore, 0)) {
		return errors.New("ID Token is not valid yet")
	}
	nonce, _ := claims["nonce"].(string)
	if expectedNonce == "" || nonce != expectedNonce {
		return errors.New("nonce claim does not match")
	}
	if m.config.RequireVerifiedEmail {
		verified, exists := claims["email_verified"]
		value, valid := verified.(bool)
		if !exists || !valid || !value {
			return errors.New("身份平台未确认企业邮箱已验证")
		}
	} else if verified, exists := claims["email_verified"]; exists {
		if value, ok := verified.(bool); ok && !value {
			return errors.New("企业邮箱尚未验证")
		}
	}
	return nil
}

func (m *Manager) identityFromClaims(claims map[string]any) (model.User, error) {
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return model.User{}, errors.New("身份平台没有返回用户标识")
	}
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	if !validEmail(email) {
		return model.User{}, errors.New("身份平台没有返回有效的企业邮箱")
	}
	if len(m.config.AllowedDomains) > 0 {
		parts := strings.Split(strings.ToLower(email), "@")
		if len(parts) != 2 || !containsFold(m.config.AllowedDomains, parts[1]) {
			return model.User{}, errors.New("当前企业邮箱域名未被授权")
		}
	}
	roleValues := claimValues(claims[m.config.RoleClaim])
	if len(m.config.AllowedValues) > 0 && !intersectsFold(roleValues, m.config.AllowedValues) {
		return model.User{}, errors.New("当前账号不属于允许登录的组织或用户组")
	}
	name := firstStringClaim(claims, "name", "preferred_username", "email")
	if name == "" {
		name = "企业成员"
	}
	role := "后端开发"
	if intersectsFold(roleValues, m.config.ReviewerValues) {
		role = "数据库审核人"
	}
	if intersectsFold(roleValues, m.config.OwnerValues) {
		role = "技术负责人"
	}
	now := time.Now()
	return model.User{
		Name: name, Email: email, Role: role, IdentityProvider: m.config.Issuer,
		Subject: subject, LastLoginAt: &now,
	}, nil
}

func (m *Manager) sessionData(r *http.Request) (session, bool, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return session{}, false, nil
	}
	key := tokenKey(cookie.Value)
	value, err := m.sessionStore.Get(r.Context(), key)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			return session{}, false, nil
		}
		return session{}, false, fmt.Errorf("read session repository: %w", err)
	}
	if time.Now().After(value.ExpiresAt) {
		_ = m.sessionStore.Delete(r.Context(), key)
		return session{}, false, nil
	}
	user, err := m.store.User(value.UserID)
	if err != nil || !user.Active {
		_ = m.sessionStore.Delete(r.Context(), key)
		return session{}, false, nil
	}
	return value, true, nil
}

func csrfValid(r *http.Request, expected string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	actual := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	if expected == "" || actual == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
func (m *Manager) cleanup() {
	m.sessionStore.Cleanup(time.Now())
}

func (m *Manager) publicPath(path string) bool {
	if path == "/api/health" || path == "/health/live" || path == "/health/ready" ||
		path == "/metrics" || path == "/api/auth/status" || path == "/api/auth/register" ||
		path == "/api/auth/login" || path == "/api/auth/invitations/accept" ||
		path == "/api/integrations/gitlab/webhook" ||
		path == "/api/integrations/jenkins/events" ||
		path == "/api/integrations/operations/webhook" ||
		strings.HasPrefix(path, "/api/gate/") || strings.HasPrefix(path, "/auth/") {
		return true
	}
	return !strings.HasPrefix(path, "/api/")
}

func (m *Manager) getJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("identity endpoint returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(target)
}

func (m *Manager) redirectError(w http.ResponseWriter, r *http.Request, message string) {
	m.logger.Printf("SSO 登录失败: %s", message)
	http.Redirect(w, r, "/?auth_error="+url.QueryEscape(message), http.StatusFound)
}

func (m *Manager) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/auth/callback", MaxAge: -1,
		HttpOnly: true, Secure: m.config.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func randomToken(size int) (string, error) {
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(content), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func tokenKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func safeNext(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return "/"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return value
}

func audienceContains(value any, expected string) bool {
	return containsFold(claimValues(value), expected)
}

func audienceLength(value any) int {
	return len(claimValues(value))
}

func claimValues(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return append([]string(nil), typed...)
	default:
		return nil
	}
}

func numberClaim(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case json.Number:
		number, err := typed.Int64()
		return number, err == nil
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func firstStringClaim(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := claims[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func containsFold(items []string, expected string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(expected)) {
			return true
		}
	}
	return false
}

func intersectsFold(left, right []string) bool {
	for _, item := range left {
		if containsFold(right, item) {
			return true
		}
	}
	return false
}

func splitValues(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '，' || r == ' '
	})
	result := make([]string, 0, len(fields))
	for _, item := range fields {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func durationOr(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func boolOr(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeAuthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	writeAuthJSON(w, status, map[string]any{"error": message})
}
