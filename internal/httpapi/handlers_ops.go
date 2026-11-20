package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/buildinfo"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/report"
)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	expected := strings.TrimSpace(os.Getenv("CHANGEGUARD_METRICS_TOKEN"))
	if expected == "" {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"dbguard-metrics\"")
		writeError(w, http.StatusUnauthorized, "监控凭据无效")
		return
	}
	s.metrics.Handler(w, r)
	build := buildinfo.Current()
	_, _ = fmt.Fprintln(w, "# HELP dbguard_build_info Build identity for release provenance.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_build_info gauge")
	_, _ = fmt.Fprintf(w, "dbguard_build_info{version=%q,commit=%q,source_sha256=%q,built_at=%q,go_version=%q} 1\n",
		build.Version, build.Commit, build.SourceSHA256, build.BuiltAt, build.GoVersion)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_build_provenance_verified Whether all release provenance fields passed validation.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_build_provenance_verified gauge")
	_, _ = fmt.Fprintf(w, "dbguard_build_provenance_verified %d\n", map[bool]int{false: 0, true: 1}[build.ProvenanceVerified])
	outcomes := s.service.GlobalGovernanceOutcomes(30, time.Now())
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_changes Changes observed in the rolling 30-day governance window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_changes gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_changes{scope=%q} %d\n", "total", outcomes.TotalChanges)
	_, _ = fmt.Fprintf(w, "dbguard_governance_changes{scope=%q} %d\n", "production", outcomes.ProductionChanges)
	_, _ = fmt.Fprintf(w, "dbguard_governance_changes{scope=%q} %d\n", "completed", outcomes.CompletedChanges)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_open_blocking_findings Blocking findings not yet independently verified.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_open_blocking_findings gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_open_blocking_findings %d\n", outcomes.OpenBlockingFindings)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_control_coverage_percent Governance control coverage in the rolling 30-day window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_control_coverage_percent gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_control_coverage_percent{control=%q} %.2f\n", "check_run", outcomes.ControlCoverage.CheckRunPercent)
	_, _ = fmt.Fprintf(w, "dbguard_governance_control_coverage_percent{control=%q} %.2f\n", "rollback_plan", outcomes.ControlCoverage.RollbackPlanPercent)
	_, _ = fmt.Fprintf(w, "dbguard_governance_control_coverage_percent{control=%q} %.2f\n", "success_metrics", outcomes.ControlCoverage.SuccessMetricsPercent)
	_, _ = fmt.Fprintf(w, "dbguard_governance_control_coverage_percent{control=%q} %.2f\n", "progressive_delivery", outcomes.ControlCoverage.ProgressiveDeliveryPercent)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_deployment_outcomes Linked terminal deployment outcomes in the rolling 30-day window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_deployment_outcomes gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_deployment_outcomes{outcome=%q} %d\n", "success", outcomes.Flow.SuccessfulDeployments)
	_, _ = fmt.Fprintf(w, "dbguard_governance_deployment_outcomes{outcome=%q} %d\n", "failed", outcomes.Flow.FailedDeployments)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_deployment_failure_rate_percent Failed terminal deployments divided by linked terminal deployment outcomes.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_deployment_failure_rate_percent gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_deployment_failure_rate_percent %.2f\n", outcomes.Flow.DeploymentFailureRate)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_rollback_outcomes Linked terminal rollback executions in the rolling 30-day window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_rollback_outcomes gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_rollback_outcomes{outcome=%q} %d\n", "success", outcomes.Operations.SuccessfulRollbacks)
	_, _ = fmt.Fprintf(w, "dbguard_governance_rollback_outcomes{outcome=%q} %d\n", "failed", outcomes.Operations.FailedRollbacks)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_linked_incidents Change-linked incidents by current state in the rolling 30-day window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_linked_incidents gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_linked_incidents{state=%q} %d\n", "open", outcomes.Operations.OpenIncidents)
	_, _ = fmt.Fprintf(w, "dbguard_governance_linked_incidents{state=%q} %d\n", "resolved", outcomes.Operations.ResolvedIncidents)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_incident_resolution_minutes Average resolution time for incidents with linked open and resolved evidence.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_incident_resolution_minutes gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_incident_resolution_minutes %.2f\n", outcomes.Operations.AverageIncidentResolutionMinutes)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_incident_resolution_samples Incident episodes backing the resolution-time metric.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_incident_resolution_samples gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_incident_resolution_samples %d\n", outcomes.Operations.IncidentResolutionSampleCount)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_post_release_remediation_rate_percent Successful releases followed by a linked incident or rollback execution.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_post_release_remediation_rate_percent gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_post_release_remediation_rate_percent %.2f\n", outcomes.Operations.PostReleaseRemediationRate)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_post_release_samples Successful release samples backing the remediation-rate metric.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_post_release_samples gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_post_release_samples %d\n", outcomes.Operations.PostReleaseSampleCount)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_business_sli_outcomes Pre/post business SLI comparisons in the rolling 30-day window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_business_sli_outcomes gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_sli_outcomes{outcome=%q} %d\n", "improved", outcomes.Business.ImprovedSLIs)
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_sli_outcomes{outcome=%q} %d\n", "stable", outcomes.Business.StableSLIs)
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_sli_outcomes{outcome=%q} %d\n", "degraded", outcomes.Business.DegradedSLIs)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_business_sli_samples Release-bracketed business SLI comparison samples.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_business_sli_samples gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_sli_samples %d\n", outcomes.Business.SLISampleCount)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_business_objective_samples Business SLI samples with an explicit numeric objective.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_business_objective_samples gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_objective_samples %d\n", outcomes.Business.ObjectiveSampleCount)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_business_objective_attainment_percent Business SLI comparisons meeting their explicit objective.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_business_objective_attainment_percent gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_business_objective_attainment_percent %.2f\n", outcomes.Business.ObjectiveAttainmentRatePercent)
	_, _ = fmt.Fprintln(w, "# HELP dbguard_governance_outcome_signal_observable Whether each external outcome signal has evidence in the rolling window.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_governance_outcome_signal_observable gauge")
	_, _ = fmt.Fprintf(w, "dbguard_governance_outcome_signal_observable{signal=%q} %d\n", "release", boolMetric(outcomes.OutcomeDataQuality.ReleaseOutcomeObservable))
	_, _ = fmt.Fprintf(w, "dbguard_governance_outcome_signal_observable{signal=%q} %d\n", "rollback", boolMetric(outcomes.OutcomeDataQuality.RollbackOutcomeObservable))
	_, _ = fmt.Fprintf(w, "dbguard_governance_outcome_signal_observable{signal=%q} %d\n", "incident", boolMetric(outcomes.OutcomeDataQuality.IncidentLinkageObservable))
	_, _ = fmt.Fprintf(w, "dbguard_governance_outcome_signal_observable{signal=%q} %d\n", "business_sli", boolMetric(outcomes.OutcomeDataQuality.BusinessSLIObservable))
	summary := s.service.GlobalOperations()
	_, _ = fmt.Fprintln(w, "# HELP dbguard_outbox_events Current Outbox events by status.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_outbox_events gauge")
	_, _ = fmt.Fprintf(w, "dbguard_outbox_events{status=%q} %d\n", "pending", summary.PendingEvents)
	_, _ = fmt.Fprintf(w, "dbguard_outbox_events{status=%q} %d\n", "processing", summary.ProcessingEvents)
	_, _ = fmt.Fprintf(w, "dbguard_outbox_events{status=%q} %d\n", "dead", summary.DeadEvents)
	oldestPendingSeconds := 0.0
	if summary.OldestPendingAt != nil {
		oldestPendingSeconds = time.Since(*summary.OldestPendingAt).Seconds()
		if oldestPendingSeconds < 0 {
			oldestPendingSeconds = 0
		}
	}
	_, _ = fmt.Fprintln(w, "# HELP dbguard_outbox_oldest_pending_age_seconds Age of the oldest pending Outbox event.")
	_, _ = fmt.Fprintln(w, "# TYPE dbguard_outbox_oldest_pending_age_seconds gauge")
	_, _ = fmt.Fprintf(w, "dbguard_outbox_oldest_pending_age_seconds %.0f\n", oldestPendingSeconds)
	for _, collector := range s.collectors {
		if collector != nil {
			collector.WritePrometheus(w)
		}
	}
}

func (s *Server) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	configured := strings.TrimSpace(os.Getenv("CHANGEGUARD_LLM_BASE_URL")) != "" &&
		strings.TrimSpace(os.Getenv("CHANGEGUARD_LLM_API_KEY")) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"llm_configured":                    configured,
		"llm_secret_ready":                  s.modelSecretConfigured(),
		"llm_provider":                      "OpenAI-compatible",
		"llm_model":                         envValue("CHANGEGUARD_LLM_MODEL", "deepseek-chat"),
		"daily_analysis_limit":              envValue("CHANGEGUARD_LLM_DAILY_ANALYSIS_LIMIT", "20"),
		"daily_organization_analysis_limit": envValue("CHANGEGUARD_LLM_DAILY_ORG_LIMIT", "100"),
		"daily_global_analysis_limit":       envValue("CHANGEGUARD_LLM_DAILY_GLOBAL_LIMIT", "200"),
		"analysis_limit_mode":               envValue("CHANGEGUARD_LLM_LIMIT_MODE", map[bool]string{true: "redis", false: "memory"}[strings.EqualFold(envValue("CHANGEGUARD_SESSION_MODE", "memory"), "redis")]),
		"max_output_tokens":                 envValue("CHANGEGUARD_LLM_MAX_TOKENS", "700"),
		"max_model_concurrency":             envValue("CHANGEGUARD_LLM_MAX_CONCURRENCY", "4"),
		"model_http_timeout":                envValue("CHANGEGUARD_LLM_HTTP_TIMEOUT", "15s"),
		"model_max_retries":                 envValue("CHANGEGUARD_LLM_MAX_RETRIES", "1"),
		"model_circuit_failures":            envValue("CHANGEGUARD_LLM_CIRCUIT_FAILURES", "3"),
		"model_circuit_cooldown":            envValue("CHANGEGUARD_LLM_CIRCUIT_COOLDOWN", "1m"),
		"experiment_mode":                   envValue("CHANGEGUARD_EXPERIMENT_MODE", "demo_only"),
		"passport_signing_configured":       s.service.PassportConfigured(),
		"gitlab_integration_configured":     s.integrations.GitLabConfigured(),
		"jenkins_integration_configured":    s.integrations.JenkinsConfigured(),
		"operations_integration_configured": s.integrations.OperationsConfigured(),
		"store_mode":                        s.service.StoreMode(),
		"session_mode":                      s.auth.SessionMode(),
		"prepare_agent_enabled":             AgentEnabled(),
		"agent_tools_enabled":               AgentToolsEnabled(),
		"capabilities": map[string]bool{
			"enterprise_llm_api":      s.analyzer != nil,
			"enterprise_outbound_api": false,
			"agent_runtime_api":       false,
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.handleReady(w, r)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "dbguard", "build": buildinfo.Current(), "time": time.Now()})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"store": "ok", "session": "ok"}
	ready := true
	if err := s.service.Health(ctx); err != nil {
		s.logger.Printf("readiness store check failed: %v", err)
		checks["store"] = "unavailable"
		ready = false
	}
	if err := s.auth.Health(ctx); err != nil {
		s.logger.Printf("readiness session check failed: %v", err)
		checks["session"] = "unavailable"
		ready = false
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	s.metrics.SetReadiness(ready)
	writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ok", false: "degraded"}[ready], "checks": checks, "build": buildinfo.Current(), "time": time.Now()})
}

func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	summary, events, err := s.service.OperationsFor(actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	summary.SessionMode = s.auth.SessionMode()
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "events": events})
}

func (s *Server) handleOutboxEvent(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/operations/outbox/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "retry" || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "未知 Outbox 操作")
		return
	}
	if err := s.service.RetryOutbox(parts[0], actorID(r)); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued"})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	value, err := s.service.DashboardFor(actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// handleGovernanceTrends 最近 N 个自然月（UTC+8）的治理趋势聚合。
func (s *Server) handleGovernanceTrends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	months := 6
	if raw := strings.TrimSpace(r.URL.Query().Get("months")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 24 {
			writeError(w, http.StatusBadRequest, "months 必须是 1 到 24 的整数")
			return
		}
		months = parsed
	}
	trends, err := s.service.GovernanceTrendsFor(actorID(r), months)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trends)
}

func (s *Server) handleGovernanceOutcomes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	windowDays := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("window_days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 365 {
			writeError(w, http.StatusBadRequest, "window_days 必须是 1 到 365 的整数")
			return
		}
		windowDays = parsed
	}
	value, err := s.service.GovernanceOutcomesFor(actorID(r), windowDays, time.Now())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		value, err := s.service.ApplicationsFor(actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
	case http.MethodPost:
		var input model.SaveApplicationInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "应用参数格式不正确")
			return
		}
		application, err := s.service.CreateApplication(input, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, application)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/apps/"), "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "应用不存在")
		return
	}
	var input model.SaveApplicationInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "应用参数格式不正确")
		return
	}
	application, err := s.service.UpdateApplication(id, input, actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, application)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	value, err := s.service.UsersFor(actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		policies, err := s.service.PoliciesFor(actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, policies)
	case http.MethodPost:
		var input model.SaveRiskPolicyInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "规则参数格式不正确")
			return
		}
		policy, err := s.service.CreatePolicy(input, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, policy)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/policies/"), "/")
	if path == "" {
		writeError(w, http.StatusNotFound, "规则不存在")
		return
	}
	if path == "test" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var input model.TestRiskPolicyInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "试跑参数格式不正确")
			return
		}
		result, err := s.service.TestPolicies(input, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if path == "export" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		policies, err := s.service.PoliciesFor(actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename=dbguard-risk-policies.json")
		writeJSON(w, http.StatusOK, map[string]any{
			"exported_at": time.Now(),
			"policies":    policies,
		})
		return
	}
	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodPut {
			methodNotAllowed(w)
			return
		}
		var input model.SaveRiskPolicyInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "规则参数格式不正确")
			return
		}
		policy, err := s.service.UpdatePolicy(id, input, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, policy)
		return
	}
	if len(parts) == 2 && parts[1] == "toggle" && r.Method == http.MethodPost {
		policy, err := s.service.TogglePolicy(id, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, policy)
		return
	}
	writeError(w, http.StatusNotFound, "未知规则操作")
}

func (s *Server) handleAudits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	audits, err := s.service.AuditsFor(actorID(r), limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, audits)
}

// handleAuditsExport 输出审计月报（打印即 PDF 的自包含 HTML）。
// GET 请求仅需会话 Cookie，新标签页打开即可通过认证；可见范围与 /api/audits 一致。
func (s *Server) handleAuditsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if query := strings.TrimSpace(r.URL.Query().Get("month")); query != "" {
		parsed, err := report.ParseMonth(query)
		if err != nil {
			writeError(w, http.StatusBadRequest, "month 必须是 YYYY-MM 格式")
			return
		}
		month = parsed
	}
	document, err := s.service.AuditMonthlyReport(actorID(r), month)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="changeguard-audit-%s.html"`, month.Format("2006-01")))
	_, _ = w.Write(document)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前服务器不支持事件流")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	actor, err := s.service.ActorFor(actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	events, cancel := s.service.Subscribe()
	defer cancel()
	if !writeEventStream(w, flusher, ": connected\n\n") {
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			if event.OrganizationID != actor.OrganizationID {
				continue
			}
			if _, accessErr := s.service.ChangeFor(event.ChangeID, actor.ID); accessErr != nil {
				continue
			}
			content, _ := json.Marshal(event)
			if !writeEventStream(w, flusher, "event: change\ndata: "+string(content)+"\n\n") {
				return
			}
		case <-heartbeat.C:
			if !writeEventStream(w, flusher, ": heartbeat\n\n") {
				return
			}
		}
	}
}

func writeEventStream(w http.ResponseWriter, flusher http.Flusher, payload string) bool {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(w, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (s *Server) handleEnterpriseLLMUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.analyzer == nil {
		writeError(w, http.StatusServiceUnavailable, "模型用量统计未启用")
		return
	}
	user, organization, ok := s.currentEnterprise(w, r)
	if !ok {
		return
	}
	usage, err := s.analyzer.UsageSnapshotContext(r.Context(), organization.ID, user.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "模型用量统计暂不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"day":               usage.Day,
		"user_used":         usage.UserUsed,
		"user_limit":        usage.UserLimit,
		"org_used":          usage.OrgUsed,
		"org_limit":         usage.OrgLimit,
		"global_observable": false,
		"source":            "recorded_runtime_counters",
	})
}
