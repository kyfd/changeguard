package agentgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/agent"
)

var protectedPath = regexp.MustCompile(`^/api/changes/([A-Za-z0-9_-]{1,128})/(agent-ask|submit-check)$`)

type Gateway struct {
	cfg     Config
	client  *http.Client
	audit   *AuditLog
	metrics *gatewayMetrics
	limiter *tokenLimiter
	started time.Time
	logger  *log.Logger
}

func New(cfg Config, logger *log.Logger) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	audit, err := OpenAuditLog(cfg.AuditFile, cfg.AuditKey)
	if err != nil {
		return nil, err
	}
	metrics, err := openGatewayMetrics(cfg.MetricsFile, cfg.AuditKey, cfg.SLOWindow)
	if err != nil {
		_ = audit.Close()
		return nil, fmt.Errorf("initialize metrics state: %w", err)
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Gateway{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.UpstreamTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		audit:   audit,
		metrics: metrics,
		limiter: newTokenLimiter(cfg.RatePerMinute, cfg.RateBurst),
		started: time.Now().UTC(),
		logger:  logger,
	}, nil
}

func (g *Gateway) Close() error {
	return errors.Join(g.metrics.Close(), g.audit.Close())
}

func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serveHTTP)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/health/live":
		g.handleLive(w, r)
	case "/health/ready":
		g.handleReady(w, r)
	case "/health/slo":
		g.handleSLO(w, r)
	case "/metrics":
		g.handleMetrics(w, r)
	case "/api/agent-runtime/summary":
		g.handleSummary(w, r)
	case "/api/agent-runtime/events":
		g.handleEvents(w, r)
	default:
		g.handleProtectedProxy(w, r)
	}
}

func (g *Gateway) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version})
}

func (g *Gateway) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	state := g.audit.State()
	metricsState := g.metrics.State()
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.ReadyTimeout)
	defer cancel()
	upstreamReady := g.upstreamReady(ctx)
	status := http.StatusOK
	if !state.Verified || !metricsState.Verified || !upstreamReady {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":           map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK],
		"audit_verified":   state.Verified,
		"metrics_verified": metricsState.Verified,
		"upstream_ready":   upstreamReady,
	})
}

func (g *Gateway) handleSLO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.ReadyTimeout)
	defer cancel()
	metrics := g.metrics.snapshot()
	metricsState := g.metrics.State()
	audit := g.audit.State()
	upstreamReady := g.upstreamReady(ctx)
	slo := calculateSLO(metrics, metricsState, audit, upstreamReady, g.cfg.SLOP95Target, g.cfg.SLOAvailability)
	status := http.StatusOK
	if slo.Status == "degraded" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, slo)
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !g.metricsAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	snapshot := g.metrics.snapshot()
	metricsState := g.metrics.State()
	audit := g.audit.State()
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.ReadyTimeout)
	defer cancel()
	upstreamReady := g.upstreamReady(ctx)
	slo := calculateSLO(snapshot, metricsState, audit, upstreamReady, g.cfg.SLOP95Target, g.cfg.SLOAvailability)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP changeguard_agent_gateway_requests_total Protected Agent operations.\n")
	_, _ = fmt.Fprintf(w, "# TYPE changeguard_agent_gateway_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_requests_total{outcome=\"success\"} %d\n", snapshot.Successful)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_requests_total{outcome=\"failed\"} %d\n", snapshot.Failed)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_requests_total{outcome=\"rejected\"} %d\n", snapshot.Rejected)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_injection_suspected_total %d\n", snapshot.InjectionSuspectedTotal)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_upstream_errors_total %d\n", snapshot.UpstreamErrors)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_request_duration_milliseconds_p95 %d\n", snapshot.P95DurationMS)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_slo_availability_percent %.2f\n", slo.AvailabilityPercent)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_slo_availability_target_percent %.2f\n", slo.AvailabilityTargetPercent)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_slo_p95_duration_milliseconds %d\n", slo.P95DurationMS)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_slo_p95_target_milliseconds %d\n", slo.P95TargetMS)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_slo_eligible_requests %d\n", slo.EligibleRequests)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_metrics_state_verified %d\n", boolNumber(metricsState.Verified))
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_metrics_window_start_unixtime %d\n", metricsState.WindowStartedAt.Unix())
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_metrics_window_end_unixtime %d\n", metricsState.WindowEndsAt.Unix())
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_metrics_last_persisted_unixtime %d\n", metricsState.LastPersistedAt.Unix())
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_upstream_ready %d\n", boolNumber(upstreamReady))
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_audit_events_total %d\n", audit.Events)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_audit_file_bytes %d\n", audit.FileBytes)
	_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_audit_chain_verified %d\n", boolNumber(audit.Verified))
	operations := make([]string, 0, len(snapshot.Operations))
	for operation := range snapshot.Operations {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	for _, operation := range operations {
		metric := snapshot.Operations[operation]
		_, _ = fmt.Fprintf(w, "changeguard_agent_gateway_operation_total{operation=%q} %d\n", operation, metric.Total)
	}
}

func (g *Gateway) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !g.authorizeRuntimeOperator(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.ReadyTimeout)
	defer cancel()
	metrics := g.metrics.snapshot()
	metricsState := g.metrics.State()
	audit := g.audit.State()
	upstreamReady := g.upstreamReady(ctx)
	slo := calculateSLO(metrics, metricsState, audit, upstreamReady, g.cfg.SLOP95Target, g.cfg.SLOAvailability)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "protected",
		"version":         Version,
		"started_at":      g.started,
		"uptime_seconds":  int64(time.Since(g.started).Seconds()),
		"upstream_ready":  upstreamReady,
		"metrics":         metrics,
		"metrics_state":   metricsState,
		"slo":             slo,
		"audit_chain":     audit,
		"rate_per_minute": g.cfg.RatePerMinute,
		"rate_burst":      g.cfg.RateBurst,
		"max_body_bytes":  g.cfg.MaxBodyBytes,
		"protected_paths": []string{"agent-ask", "submit-check"},
	})
}

func (g *Gateway) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !g.authorizeRuntimeOperator(w, r) {
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}
	includeStarted := false
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include_started"))) {
	case "1", "true", "yes":
		includeStarted = true
	}
	page, err := g.audit.Recent(limit, includeStarted)
	if err != nil {
		g.logger.Printf("read recent audit events failed: %v", err)
		http.Error(w, "Agent audit log unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (g *Gateway) authorizeRuntimeOperator(w http.ResponseWriter, r *http.Request) bool {
	actor, status, err := g.authenticate(r.Context(), r)
	if err != nil {
		g.logger.Printf("runtime authentication failed: %v", err)
		http.Error(w, "authentication unavailable", http.StatusBadGateway)
		return false
	}
	if status != http.StatusOK {
		http.Error(w, "unauthorized", status)
		return false
	}
	if !actor.EnterpriseAdmin && actor.Role != "技术负责人" && actor.Role != "企业管理员" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (g *Gateway) handleProtectedProxy(w http.ResponseWriter, r *http.Request) {
	match := protectedPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Agent-Gateway", "protected")
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	started := time.Now()
	changeID, operation := match[1], match[2]
	principal := principalHash(r)
	if !g.limiter.allow(principal, started) {
		g.recordMetrics(operation, "rejected", time.Since(started), false, false)
		g.appendAudit(AuditRecord{Operation: operation, ChangeID: changeID, PrincipalHash: principal, HTTPStatus: http.StatusTooManyRequests, Outcome: "rate_limited"})
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Agent request rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	body, err := readLimitedBody(r.Body, g.cfg.MaxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		g.recordMetrics(operation, "rejected", time.Since(started), false, false)
		g.appendAudit(AuditRecord{Operation: operation, ChangeID: changeID, PrincipalHash: principal, HTTPStatus: status, Outcome: "invalid_request"})
		http.Error(w, err.Error(), status)
		return
	}
	requestDigest := sha256.Sum256(body)
	injection := false
	if operation == "agent-ask" {
		var payload struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Question) == "" {
			g.recordMetrics(operation, "rejected", time.Since(started), false, false)
			g.appendAudit(AuditRecord{Operation: operation, ChangeID: changeID, PrincipalHash: principal, RequestSHA256: hex.EncodeToString(requestDigest[:]), HTTPStatus: http.StatusBadRequest, Outcome: "invalid_question"})
			http.Error(w, "question is required", http.StatusBadRequest)
			return
		}
		injection, _ = agent.DetectTextInjection(payload.Question)
	}
	preflight := AuditRecord{
		Operation:          operation + "_started",
		ChangeID:           changeID,
		PrincipalHash:      principal,
		RequestSHA256:      hex.EncodeToString(requestDigest[:]),
		Outcome:            "started",
		InjectionSuspected: injection,
	}
	if _, err := g.audit.Append(preflight); err != nil {
		g.logger.Printf("audit preflight failed: %v", err)
		g.recordMetrics(operation, "failed", time.Since(started), injection, false)
		http.Error(w, "Agent audit log unavailable", http.StatusServiceUnavailable)
		return
	}
	response, responseBody, upstreamErr := g.forward(r.Context(), r, body)
	if upstreamErr != nil {
		duration := time.Since(started)
		g.recordMetrics(operation, "failed", duration, injection, true)
		g.appendAudit(AuditRecord{
			Operation: operation, ChangeID: changeID, PrincipalHash: principal,
			RequestSHA256: hex.EncodeToString(requestDigest[:]), HTTPStatus: http.StatusBadGateway,
			Outcome: "upstream_error", DurationMS: duration.Milliseconds(), InjectionSuspected: injection,
		})
		g.logger.Printf("upstream %s failed: %v", operation, upstreamErr)
		http.Error(w, "Agent upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	meta := extractAgentMetadata(responseBody)
	if injection {
		meta.InjectionSuspected = true
	}
	outcome := "success"
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		outcome = "rejected"
	} else if response.StatusCode >= 500 {
		outcome = "failed"
	}
	duration := time.Since(started)
	g.recordMetrics(operation, outcome, duration, meta.InjectionSuspected, response.StatusCode >= 500)
	if _, err := g.audit.Append(AuditRecord{
		Operation: operation, ChangeID: changeID, PrincipalHash: principal,
		RequestSHA256: hex.EncodeToString(requestDigest[:]), HTTPStatus: response.StatusCode,
		Outcome: outcome, DurationMS: duration.Milliseconds(), TraceID: meta.TraceID,
		Provider: meta.Provider, Model: meta.Model, Risk: meta.Risk, ToolCalls: meta.ToolCalls,
		InjectionSuspected: meta.InjectionSuspected,
	}); err != nil {
		g.logger.Printf("audit completion failed: %v", err)
		w.Header().Set("X-Agent-Audit-Status", "incomplete")
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("X-Agent-Gateway", "protected")
	if meta.TraceID != "" {
		w.Header().Set("X-Agent-Trace-ID", meta.TraceID)
	}
	if meta.InjectionSuspected {
		w.Header().Set("X-Agent-Input-Risk", "suspected")
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

func (g *Gateway) forward(ctx context.Context, source *http.Request, body []byte) (*http.Response, []byte, error) {
	target := *g.cfg.UpstreamURL
	target.Path = source.URL.Path
	target.RawQuery = source.URL.RawQuery
	request, err := http.NewRequestWithContext(ctx, source.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	copyRequestHeaders(request.Header, source.Header)
	request.Header.Set("X-Agent-Gateway", Version)
	response, err := g.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	content, err := readLimitedBody(response.Body, g.cfg.MaxResponse)
	if err != nil {
		_ = response.Body.Close()
		return nil, nil, fmt.Errorf("read upstream response: %w", err)
	}
	response.Body = io.NopCloser(bytes.NewReader(content))
	return response, content, nil
}

type actorIdentity struct {
	ID              string `json:"id"`
	Role            string `json:"role"`
	EnterpriseAdmin bool   `json:"enterprise_admin"`
}

func (g *Gateway) authenticate(ctx context.Context, source *http.Request) (actorIdentity, int, error) {
	target := *g.cfg.UpstreamURL
	target.Path = "/api/auth/me"
	target.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return actorIdentity{}, 0, err
	}
	for _, name := range []string{"Cookie", "Authorization", "User-Agent", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Proto"} {
		if value := source.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
	response, err := g.client.Do(request)
	if err != nil {
		return actorIdentity{}, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			status = http.StatusBadGateway
		}
		return actorIdentity{}, status, nil
	}
	body, err := readLimitedBody(response.Body, 64<<10)
	if err != nil {
		return actorIdentity{}, 0, err
	}
	var actor actorIdentity
	if err := json.Unmarshal(body, &actor); err != nil {
		return actorIdentity{}, 0, err
	}
	if actor.ID == "" {
		return actorIdentity{}, 0, errors.New("upstream identity did not include an id")
	}
	return actor, http.StatusOK, nil
}

func (g *Gateway) upstreamReady(ctx context.Context) bool {
	target := *g.cfg.UpstreamURL
	target.Path = "/health/ready"
	target.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	response, err := g.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (g *Gateway) metricsAuthorized(r *http.Request) bool {
	if g.cfg.MetricsToken != "" {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		return subtle.ConstantTimeCompare([]byte(provided), []byte(g.cfg.MetricsToken)) == 1
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (g *Gateway) appendAudit(record AuditRecord) {
	if _, err := g.audit.Append(record); err != nil {
		g.logger.Printf("append audit failed: %v", err)
	}
}

func (g *Gateway) recordMetrics(operation, outcome string, duration time.Duration, injection, upstreamError bool) {
	if err := g.metrics.record(operation, outcome, duration, injection, upstreamError); err != nil {
		g.logger.Printf("persist Agent metrics failed: %v", err)
	}
}

type agentMetadata struct {
	TraceID            string
	Provider           string
	Model              string
	Risk               string
	ToolCalls          int
	InjectionSuspected bool
}

func extractAgentMetadata(body []byte) agentMetadata {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return agentMetadata{}
	}
	candidates := make([]map[string]any, 0, 8)
	// agent-ask returns the updated change as the root object. Prefer the
	// newest Q&A entry so its trace and tool metadata cannot be shadowed by
	// an older analysis attached to the same change.
	appendLatestAgentQA := func(container map[string]any) {
		items, ok := container["agent_qa"].([]any)
		if !ok || len(items) == 0 {
			return
		}
		if latest, ok := items[len(items)-1].(map[string]any); ok {
			candidates = append(candidates, latest)
		}
	}
	appendLatestAgentQA(root)
	for _, key := range []string{"entry", "analysis", "change"} {
		if value, ok := root[key].(map[string]any); ok {
			appendLatestAgentQA(value)
			candidates = append(candidates, value)
			if nested, ok := value["analysis"].(map[string]any); ok {
				candidates = append(candidates, nested)
			}
		}
	}
	candidates = append(candidates, root)
	result := agentMetadata{}
	for _, candidate := range candidates {
		if result.TraceID == "" {
			result.TraceID = textValue(candidate["trace_id"])
		}
		if result.Provider == "" {
			result.Provider = textValue(candidate["provider"])
		}
		if result.Model == "" {
			result.Model = textValue(candidate["model"])
		}
		if result.Risk == "" {
			result.Risk = textValue(candidate["risk"])
		}
		if result.ToolCalls == 0 {
			result.ToolCalls = intValue(candidate["tool_calls"])
		}
		if boolValue(candidate["injection_suspected"]) {
			result.InjectionSuspected = true
		}
	}
	return result
}

var errBodyTooLarge = errors.New("request body exceeds configured limit")

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errBodyTooLarge
	}
	return content, nil
}

func principalHash(r *http.Request) string {
	identity := r.Header.Get("Cookie")
	if identity == "" {
		identity = r.Header.Get("Authorization")
	}
	if identity == "" {
		identity = r.Header.Get("X-Real-IP")
	}
	if identity == "" {
		identity = r.RemoteAddr
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:8])
}

func copyRequestHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		destination.Del(key)
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func textValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func intValue(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(number)
		return parsed
	default:
		return 0
	}
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
