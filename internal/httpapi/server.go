package httpapi

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/agent"
	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/modelprobe"
	"github.com/kyfd/changeguard/internal/modelsecret"
	"github.com/kyfd/changeguard/internal/observability"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

//go:embed web/*
var webAssets embed.FS

type Server struct {
	service      *service.Service
	auth         *auth.Manager
	logger       *log.Logger
	metrics      *observability.Metrics
	collectors   []MetricsCollector
	integrations integration.Config
	handler      http.Handler
	// store 与 analyzer 由 New 从 collectors 里识别出来。它们同时也是
	// MetricsCollector，但这里需要具体类型：模型接入配置写在 store 上，
	// 而 Agent 运行时的按企业解析器需要读回同一份数据。
	store    *store.Store
	analyzer *agent.Runtime
	// secrets 封装企业模型 Key。未配置主密钥时为 nil，接口据此失败关闭。
	secrets *modelsecret.Box
	probe   *modelprobe.Client
	// allowPrivateUpstream 是否放行内网模型网关。默认 false，由环境显式打开。
	allowPrivateUpstream bool
}

type MetricsCollector interface {
	WritePrometheus(io.Writer)
}

func New(svc *service.Service, authManager *auth.Manager, logger *log.Logger, collectors ...MetricsCollector) *Server {
	server := &Server{
		service: svc, auth: authManager, logger: logger,
		metrics: observability.New(), collectors: collectors, integrations: integration.FromEnvironment(),
	}
	for _, collector := range collectors {
		switch typed := collector.(type) {
		case *store.Store:
			server.store = typed
		case *agent.Runtime:
			server.analyzer = typed
		}
	}
	// 主密钥缺失时 secrets 保持 nil：所有模型接入接口都会明确报"未配置主密钥"，
	// 而不是退化成明文存储。
	if box, err := modelsecret.New(strings.TrimSpace(os.Getenv("CHANGEGUARD_SECRETS_MASTER_KEY"))); err == nil {
		server.secrets = box
	} else if !errors.Is(err, modelsecret.ErrNotConfigured) {
		logger.Printf("model secret box unavailable: %v", err)
	}
	server.allowPrivateUpstream = modelprobe.AllowPrivateUpstream()
	server.probe = modelprobe.New(server.allowPrivateUpstream)
	// 把企业自配模型接进 Agent 运行时。缺主密钥时不注入 resolver，
	// 让 Runtime 保持原有 env 行为，而不是注入一个必然失败的解析器。
	if configured := server.wireModelConfigResolver(); configured > 0 {
		logger.Printf("enterprise model config active for %d organization(s)", configured)
	} else if !server.modelSecretConfigured() {
		logger.Printf("enterprise model config unavailable: set CHANGEGUARD_SECRETS_MASTER_KEY to enable per-organization model keys")
	}
	server.handler = securityHeaders(server.routes())
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/auth/") ||
			strings.HasPrefix(r.URL.Path, "/api/gate/") ||
			strings.HasPrefix(r.URL.Path, "/api/integrations/") ||
			strings.HasPrefix(r.URL.Path, "/auth/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/health/live", s.handleLive)
	mux.HandleFunc("/health/ready", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/api/auth/status", s.auth.HandleStatus)
	mux.HandleFunc("/api/auth/me", s.auth.HandleMe)
	mux.HandleFunc("/api/auth/session", s.auth.HandleSession)
	mux.HandleFunc("/api/auth/register", s.auth.HandleRegisterEnterprise)
	mux.HandleFunc("/api/auth/login", s.auth.HandleLocalLogin)
	mux.HandleFunc("/api/auth/invitations/accept", s.auth.HandleAcceptInvite)
	mux.HandleFunc("/auth/login", s.auth.HandleLogin)
	mux.HandleFunc("/auth/callback", s.auth.HandleCallback)
	mux.HandleFunc("/auth/logout", s.auth.HandleLogout)
	mux.HandleFunc("/api/enterprise", s.auth.HandleOrganization)
	mux.HandleFunc("/api/enterprise/members", s.auth.HandleMembers)
	mux.HandleFunc("/api/enterprise/members/", s.auth.HandleMember)
	mux.HandleFunc("/api/enterprise/invites", s.auth.HandleInvites)
	mux.HandleFunc("/api/enterprise/invites/", s.auth.HandleInvite)
	// 企业自配模型接入。浏览器只经本服务访问上游；Key 加密落盘、永不回显。
	mux.HandleFunc("/api/enterprise/llm", s.handleEnterpriseLLM)
	mux.HandleFunc("/api/enterprise/llm/test", s.handleEnterpriseLLMTest)
	mux.HandleFunc("/api/enterprise/llm/models", s.handleEnterpriseLLMModels)
	mux.HandleFunc("/api/enterprise/llm/presets", s.handleEnterpriseLLMPresets)
	mux.HandleFunc("/api/config/status", s.handleConfigStatus)
	mux.HandleFunc("/api/dashboard", s.handleDashboard)
	mux.HandleFunc("/api/governance/outcomes", s.handleGovernanceOutcomes)
	mux.HandleFunc("/api/governance/trends", s.handleGovernanceTrends)
	mux.HandleFunc("/api/apps", s.handleApps)
	mux.HandleFunc("/api/apps/", s.handleApp)
	mux.HandleFunc("/api/users", s.handleUsers)
	mux.HandleFunc("/api/policies", s.handlePolicies)
	mux.HandleFunc("/api/policies/", s.handlePolicy)
	mux.HandleFunc("/api/enterprise/llm/usage", s.handleEnterpriseLLMUsage)
	mux.HandleFunc("/api/changes", s.handleChanges)
	mux.HandleFunc("/api/changes/", s.handleChange)
	mux.HandleFunc("/api/passports", s.handlePassports)
	mux.HandleFunc("/api/conflicts", s.handleConflicts)
	mux.HandleFunc("/api/gate/verify", s.handleGateVerify)
	mux.HandleFunc("/api/gate/consume", s.handleGateConsume)
	mux.HandleFunc("/api/audits", s.handleAudits)
	mux.HandleFunc("/api/audits/export", s.handleAuditsExport)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/operations/outbox", s.handleOutbox)
	mux.HandleFunc("/api/operations/outbox/", s.handleOutboxEvent)
	mux.HandleFunc("/api/integrations/status", s.handleIntegrationStatus)
	mux.HandleFunc("/api/integrations/events", s.handleIntegrationEvents)
	mux.HandleFunc("/api/integrations/gitlab/webhook", s.handleGitLabWebhook)
	mux.HandleFunc("/api/integrations/jenkins/events", s.handleJenkinsEvent)
	mux.HandleFunc("/api/integrations/operations/events", s.handleOperationsEvents)
	mux.HandleFunc("/api/integrations/operations/webhook", s.handleOperationsWebhook)
	mux.HandleFunc("/api/upgrade/status", s.handleUpgradeStatus)
	mux.HandleFunc("/api/upgrade/upload", s.handleUpgradeUpload)
	mux.HandleFunc("/api/upgrade/apply", s.handleUpgradeApply)
	mux.HandleFunc("/api/upgrade/abort", s.handleUpgradeAbort)
	// 变更准备 Agent：浏览器只与本服务通信，身份由服务端解析后注入下游。
	mux.HandleFunc("/api/agent/", s.handleAgentProxy)
	// 工作台页面与资源。必须显式注册：根静态处理器会把目录路径回退成控制台首页。
	mux.HandleFunc("/agent/", s.handleAgentWorkbench)
	staticFS, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "接口不存在")
			return
		}
		if r.URL.Path != "/" {
			if _, err := fs.Stat(staticFS, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
				r.URL.Path = "/"
			}
		}
		if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
	authenticated := s.auth.Middleware(mux)

	// 内部只读工具接口单独成一棵树：它由共享密钥做服务认证，**刻意不复用会话中间件**。
	// Agent 后端没有、也不应该持有治理会话（见 agenttools.go 的说明）；
	// 浏览器拿不到共享密钥，因此这个入口对浏览器不可达。
	root := http.NewServeMux()
	root.Handle(agentToolsPrefix, http.HandlerFunc(s.handleAgentTools))
	root.Handle("/", authenticated)
	return s.metrics.Middleware(withRecover(root, s.logger), s.logger)
}

func optionalTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func envValue(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func boolMetric(value bool) int {
	if value {
		return 1
	}
	return 0
}

func requestDigest(operation, resource string, body any) string {
	encoded, _ := json.Marshal(struct {
		Operation string `json:"operation"`
		Resource  string `json:"resource"`
		Body      any    `json:"body,omitempty"`
	}{operation, resource, body})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

func validatedIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", true
	}
	if !idempotencyKeyPattern.MatchString(key) {
		writeCodedError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key 必须为 8 到 128 个 ASCII 字符，仅允许字母、数字、点、下划线、冒号和连字符")
		return "", false
	}
	return key, true
}

func actorID(r *http.Request) string {
	if value, ok := auth.ActorID(r.Context()); ok {
		return value
	}
	return ""
}

func readComment(r *http.Request) string {
	var body struct{ Comment string }
	_ = decodeJSON(r, &body)
	return strings.TrimSpace(body.Comment)
}

const maxJSONBodyBytes int64 = 2 << 20

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	limited := &io.LimitedReader{R: r.Body, N: maxJSONBodyBytes + 1}
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

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrIdempotencyConflict):
		writeCodedError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", err.Error())
	case errors.Is(err, service.ErrIdempotencyInProgress):
		writeCodedError(w, http.StatusConflict, "IDEMPOTENCY_REQUEST_IN_PROGRESS", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "记录不存在")
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrInvalidState), errors.Is(err, store.ErrConcurrentWrite), errors.Is(err, service.ErrRuleSetChanged), errors.Is(err, service.ErrPassportRevoked), errors.Is(err, service.ErrPassportReplay), errors.Is(err, store.ErrPassportInactive), errors.Is(err, service.ErrUntrustedAgentTask):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrPassportExpired), errors.Is(err, store.ErrPassportExpired):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, service.ErrPassportUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, service.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "服务处理失败")
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code, "message": message})
}

func writeError(w http.ResponseWriter, status int, message string) {
	code := map[int]string{
		http.StatusBadRequest: "BAD_REQUEST", http.StatusUnauthorized: "UNAUTHORIZED",
		http.StatusForbidden: "FORBIDDEN", http.StatusNotFound: "NOT_FOUND",
		http.StatusConflict: "CONFLICT", http.StatusGone: "GONE",
		http.StatusMethodNotAllowed: "METHOD_NOT_ALLOWED", http.StatusServiceUnavailable: "SERVICE_UNAVAILABLE",
		http.StatusInternalServerError: "INTERNAL_ERROR",
	}[status]
	if code == "" {
		code = "REQUEST_FAILED"
	}
	writeJSON(w, status, map[string]any{"error": message, "code": code, "message": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "请求方法不支持")
}

func withRecover(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Printf("panic: %v", recovered)
				writeError(w, http.StatusInternalServerError, "服务器发生内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func Shutdown(ctx context.Context, server *http.Server) error {
	return server.Shutdown(ctx)
}
