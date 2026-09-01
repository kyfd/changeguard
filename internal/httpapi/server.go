package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/buildinfo"
	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/observability"
	"github.com/kyfd/changeguard/internal/report"
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
}

type MetricsCollector interface {
	WritePrometheus(io.Writer)
}

func New(svc *service.Service, authManager *auth.Manager, logger *log.Logger, collectors ...MetricsCollector) *Server {
	server := &Server{
		service: svc, auth: authManager, logger: logger,
		metrics: observability.New(), collectors: collectors, integrations: integration.FromEnvironment(),
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
	mux.HandleFunc("/api/config/status", s.handleConfigStatus)
	mux.HandleFunc("/api/dashboard", s.handleDashboard)
	mux.HandleFunc("/api/governance/outcomes", s.handleGovernanceOutcomes)
	mux.HandleFunc("/api/apps", s.handleApps)
	mux.HandleFunc("/api/apps/", s.handleApp)
	mux.HandleFunc("/api/users", s.handleUsers)
	mux.HandleFunc("/api/policies", s.handlePolicies)
	mux.HandleFunc("/api/policies/", s.handlePolicy)
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
	return s.metrics.Middleware(withRecover(s.auth.Middleware(mux), s.logger), s.logger)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	expected := strings.TrimSpace(os.Getenv("DBGUARD_METRICS_TOKEN"))
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
	configured := strings.TrimSpace(os.Getenv("DBGUARD_LLM_BASE_URL")) != "" &&
		strings.TrimSpace(os.Getenv("DBGUARD_LLM_API_KEY")) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"llm_configured":                    configured,
		"llm_provider":                      "OpenAI-compatible",
		"llm_model":                         envValue("DBGUARD_LLM_MODEL", "deepseek-chat"),
		"daily_analysis_limit":              envValue("DBGUARD_LLM_DAILY_ANALYSIS_LIMIT", "20"),
		"daily_organization_analysis_limit": envValue("DBGUARD_LLM_DAILY_ORG_LIMIT", "100"),
		"daily_global_analysis_limit":       envValue("DBGUARD_LLM_DAILY_GLOBAL_LIMIT", "200"),
		"analysis_limit_mode":               envValue("DBGUARD_LLM_LIMIT_MODE", map[bool]string{true: "redis", false: "memory"}[strings.EqualFold(envValue("DBGUARD_SESSION_MODE", "memory"), "redis")]),
		"max_output_tokens":                 envValue("DBGUARD_LLM_MAX_TOKENS", "700"),
		"max_model_concurrency":             envValue("DBGUARD_LLM_MAX_CONCURRENCY", "4"),
		"model_http_timeout":                envValue("DBGUARD_LLM_HTTP_TIMEOUT", "15s"),
		"model_max_retries":                 envValue("DBGUARD_LLM_MAX_RETRIES", "1"),
		"model_circuit_failures":            envValue("DBGUARD_LLM_CIRCUIT_FAILURES", "3"),
		"model_circuit_cooldown":            envValue("DBGUARD_LLM_CIRCUIT_COOLDOWN", "1m"),
		"experiment_mode":                   envValue("DBGUARD_EXPERIMENT_MODE", "demo_only"),
		"passport_signing_configured":       s.service.PassportConfigured(),
		"gitlab_integration_configured":     s.integrations.GitLabConfigured(),
		"jenkins_integration_configured":    s.integrations.JenkinsConfigured(),
		"operations_integration_configured": s.integrations.OperationsConfigured(),
		"store_mode":                        s.service.StoreMode(),
		"session_mode":                      s.auth.SessionMode(),
	})
}

func (s *Server) handleChangeReport(w http.ResponseWriter, id, format, requestActorID string) {
	change, err := s.service.ChangeFor(id, requestActorID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if strings.EqualFold(format, "xlsx") {
		audits, auditErr := s.service.AuditsForChange(id, requestActorID)
		if auditErr != nil {
			writeServiceError(w, auditErr)
			return
		}
		content, buildErr := report.XLSX(change, audits)
		if buildErr != nil {
			writeError(w, http.StatusInternalServerError, "Excel 报告生成失败")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-evidence.xlsx", change.ID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}

	var document strings.Builder
	fmt.Fprintf(&document, "# ChangeGuard 研发变更证据报告\n\n")
	fmt.Fprintf(&document, "- 变更编号：%s\n- 标题：%s\n- 服务：%s\n- 环境：%s\n- 变更类型：%s\n", change.ID, change.Title, change.ApplicationName, change.Environment, change.ChangeType)
	fmt.Fprintf(&document, "- 状态：%s\n- 风险：%s\n- 提交人：%s\n- 计划时间：%s\n- 制品 SHA-256：%s\n- 规则版本：%s\n", change.Status, change.Risk, change.SubmitterName, change.PlannedAt.Format(time.RFC3339), change.ArtifactSHA256, change.RuleSetVersion)
	if change.RepositoryURL != "" {
		fmt.Fprintf(&document, "- 代码仓库：%s\n", change.RepositoryURL)
	}
	if change.Branch != "" {
		fmt.Fprintf(&document, "- 分支：%s\n", change.Branch)
	}
	if change.CommitSHA != "" {
		fmt.Fprintf(&document, "- Commit：%s\n", change.CommitSHA)
	}
	document.WriteString("\n## 业务背景与变更目标\n\n")
	if strings.TrimSpace(change.Description) == "" {
		document.WriteString("未填写业务说明。\n")
	} else {
		document.WriteString(change.Description + "\n")
	}

	document.WriteString("\n## 变更制品清单\n\n")
	if len(change.Artifacts) == 0 {
		document.WriteString("- 未上传代码、配置、Kubernetes 或 API 结构化制品。\n")
	}
	for _, artifact := range change.Artifacts {
		fmt.Fprintf(&document, "### %s · %s\n\n", artifactKindLabel(artifact.Kind), artifact.Name)
		if artifact.Source != "" {
			fmt.Fprintf(&document, "- 来源：%s\n", artifact.Source)
		}
		if artifact.Language != "" {
			fmt.Fprintf(&document, "- 格式：%s\n", artifact.Language)
		}
		document.WriteString("\n" + codeFence(markdownLanguage(artifact.Language), artifact.Content) + "\n")
	}
	if strings.TrimSpace(change.SQL) != "" {
		document.WriteString("### 数据库执行脚本\n\n" + codeFence("sql", change.SQL) + "\n")
	}

	document.WriteString("## 回滚与恢复方案\n\n")
	if strings.TrimSpace(change.RollbackPlan) != "" {
		document.WriteString(change.RollbackPlan + "\n\n")
	} else {
		document.WriteString("未填写文字回滚方案。\n\n")
	}
	if strings.TrimSpace(change.RollbackSQL) != "" {
		document.WriteString(codeFence("sql", change.RollbackSQL) + "\n")
	}

	metrics := strings.Join(change.ReleasePlan.SuccessMetrics, "、")
	if metrics == "" {
		metrics = "未配置"
	}
	strategy := change.ReleasePlan.Strategy
	if strategy == "" {
		strategy = "未配置"
	}
	fmt.Fprintf(&document, "## 发布计划\n\n- 策略：%s\n- 灰度比例：%d%%\n- 观察时长：%d 分钟\n- 自动回滚：%t\n- 成功指标：%s\n",
		strategy, change.ReleasePlan.CanaryPercent, change.ReleasePlan.ObservationMinutes, change.ReleasePlan.AutoRollback, metrics)

	document.WriteString("\n## 规则检查与风险证据\n\n")
	if len(change.Findings) == 0 {
		document.WriteString("- 未产生规则风险项。\n")
	}
	for _, finding := range change.Findings {
		fmt.Fprintf(&document, "- [%s/%s] %s（%s，规则 v%d，阻断=%t）：%s；建议：%s；证据：%s\n",
			finding.Severity, finding.Status, finding.Title, finding.Code, finding.RuleVersion, finding.Blocking,
			finding.Detail, finding.Suggestion, finding.ID)
	}
	if change.Experiment != nil {
		fmt.Fprintf(&document, "\n## 预发布验证\n\n- 模式：%s\n- 结果：%s\n- 策略：%s\n- 耗时：%dms\n- 最大锁等待：%dms\n- 检查通过：%d/%d\n- 回滚验证：%t\n",
			change.Experiment.Mode, change.Experiment.Status, change.Experiment.Strategy, change.Experiment.DurationMS,
			change.Experiment.LockWaitMS, change.Experiment.ChecksPassed, change.Experiment.ChecksTotal, change.Experiment.RollbackVerified)
		if change.Experiment.ExecutionError != "" {
			fmt.Fprintf(&document, "- 执行错误：%s\n", change.Experiment.ExecutionError)
		}
		for _, evidence := range change.Experiment.Evidence {
			fmt.Fprintf(&document, "- 证据 %s · %s：%s（来源：%s）\n", evidence.ID, evidence.Title, evidence.Value, evidence.Source)
		}
	}
	if change.Analysis != nil {
		advisoryRisk := change.Analysis.AdvisoryRisk
		if advisoryRisk == "" {
			advisoryRisk = change.Analysis.Risk
		}
		fmt.Fprintf(&document, "\n## 智能辅助分析（仅供参考，不参与放行）\n\n- 提供方：%s\n- 模型：%s\n- AI 建议风险：%s\n- 结论：%s\n- 引用证据：%s\n",
			change.Analysis.Provider, change.Analysis.Model, advisoryRisk, change.Analysis.Summary, strings.Join(change.Analysis.EvidenceIDs, "、"))
		for _, reason := range change.Analysis.Reasons {
			fmt.Fprintf(&document, "- 依据：%s\n", reason)
		}
		for _, suggestion := range change.Analysis.Suggestions {
			fmt.Fprintf(&document, "- 建议：%s\n", suggestion)
		}
	}

	audits, auditErr := s.service.AuditsForChange(id, requestActorID)
	if auditErr != nil {
		writeServiceError(w, auditErr)
		return
	}
	document.WriteString("\n## 协作讨论\n\n")
	if len(change.Comments) == 0 {
		document.WriteString("- 暂无协作评论。\n")
	}
	for _, comment := range change.Comments {
		fmt.Fprintf(&document, "- %s · %s（%s）：%s\n", comment.CreatedAt.Format(time.RFC3339), comment.AuthorName, comment.AuthorRole, comment.Content)
	}
	document.WriteString("\n## 审计轨迹\n\n")
	if len(audits) == 0 {
		document.WriteString("- 暂无审计事件。\n")
	}
	for _, event := range audits {
		fmt.Fprintf(&document, "- %s · %s · %s：%s\n", event.CreatedAt.Format(time.RFC3339), event.ActorName, event.Action, event.Detail)
	}
	fmt.Fprintf(&document, "\n## 审批结论\n\n- 审批人：%s\n- 意见：%s\n", change.ReviewerName, change.ReviewComment)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-report.md", change.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(document.String()))
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	from, err := optionalTime(r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from 必须是 RFC3339 时间")
		return
	}
	to, err := optionalTime(r.URL.Query().Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to 必须是 RFC3339 时间")
		return
	}
	radar, err := s.service.ConflictRadarFor(actorID(r), from, to)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, radar)
}

func (s *Server) handleIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	events, err := s.service.IntegrationEventsFor(actorID(r), 250)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	signals, err := s.service.OutcomeSignalsFor(actorID(r), 0)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var gitLabLast, jenkinsLast, operationsLast *time.Time
	for _, event := range events {
		received := event.ReceivedAt
		if event.Provider == "GITLAB" && gitLabLast == nil {
			gitLabLast = &received
		}
		if event.Provider == "JENKINS" && jenkinsLast == nil {
			jenkinsLast = &received
		}
	}
	for _, signal := range signals {
		if operationsLast == nil || signal.ReceivedAt.After(*operationsLast) {
			received := signal.ReceivedAt
			operationsLast = &received
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DBGUARD_PUBLIC_URL")), "/")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	gitLabAuth := "X-Gitlab-Token（兼容模式）"
	if s.integrations.GitLabSigningToken != "" {
		gitLabAuth = "HMAC-SHA256 Signing Token（推荐）"
	}
	status := model.IntegrationStatus{
		GitLab: model.IntegrationProviderStatus{
			Provider: "GITLAB", Configured: s.integrations.GitLabConfigured(),
			Authentication:  gitLabAuth,
			Endpoint:        baseURL + "/api/integrations/gitlab/webhook",
			SupportedEvents: []string{"Pipeline Hook"},
			LastReceivedAt:  gitLabLast,
		},
		Jenkins: model.IntegrationProviderStatus{
			Provider: "JENKINS", Configured: s.integrations.JenkinsConfigured(),
			Authentication:  "Authorization: Bearer Token",
			Endpoint:        baseURL + "/api/integrations/jenkins/events",
			SupportedEvents: []string{"RUNNING", "SUCCESS", "FAILURE", "ABORTED", "UNSTABLE"},
			LastReceivedAt:  jenkinsLast,
		},
		Operations: model.IntegrationProviderStatus{
			Provider: "OPERATIONS", Configured: s.integrations.OperationsConfigured(),
			Authentication:  "Authorization: Bearer Token",
			Endpoint:        baseURL + "/api/integrations/operations/webhook",
			SupportedEvents: []string{"INCIDENT", "ROLLBACK", "BUSINESS_SLI"},
			LastReceivedAt:  operationsLast,
		},
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleIntegrationEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 500 之间")
			return
		}
		limit = parsed
	}
	events, err := s.service.IntegrationEventsFor(actorID(r), limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GitLab Webhook 请求体无效")
		return
	}
	if err := integration.VerifyGitLab(s.integrations, r.Header, body, time.Now()); err != nil {
		writeIntegrationError(w, err)
		return
	}
	event, err := integration.ParseGitLab(s.integrations, r.Header, body)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	s.recordIntegrationEvent(w, event)
}

func (s *Server) handleJenkinsEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Jenkins 事件请求体无效")
		return
	}
	if err := integration.VerifyJenkins(s.integrations, r.Header); err != nil {
		writeIntegrationError(w, err)
		return
	}
	event, err := integration.ParseJenkins(s.integrations, r.Header, body)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	s.recordIntegrationEvent(w, event)
}

func (s *Server) handleOperationsEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 500 之间")
			return
		}
		limit = parsed
	}
	signals, err := s.service.OutcomeSignalsFor(actorID(r), limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": signals})
}

func (s *Server) handleOperationsWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "运维结果事件请求体无效")
		return
	}
	if err := integration.VerifyOperations(s.integrations, r.Header); err != nil {
		writeOperationsError(w, err)
		return
	}
	signal, err := integration.ParseOperations(s.integrations, body, time.Now())
	if err != nil {
		writeOperationsError(w, err)
		return
	}
	recorded, created, err := s.service.RecordOutcomeSignal(signal)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"accepted": true, "duplicate": !created,
		"event_id": recorded.ExternalID, "signal_id": recorded.ID,
		"change_id": recorded.ChangeID, "kind": recorded.Kind,
	})
}

func (s *Server) recordIntegrationEvent(w http.ResponseWriter, event model.IntegrationEvent) {
	recorded, created, err := s.service.RecordIntegrationEvent(event)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"accepted": true, "duplicate": !created,
		"event_id": recorded.ID, "change_id": recorded.ChangeID,
	})
}

func readIntegrationBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
}

func writeIntegrationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "流水线集成尚未配置")
	case errors.Is(err, integration.ErrUnauthorized), errors.Is(err, integration.ErrReplay):
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"changeguard-integration\"")
		writeError(w, http.StatusUnauthorized, "流水线凭据或签名无效")
	case errors.Is(err, integration.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "仅支持 GitLab Pipeline Hook")
	default:
		writeError(w, http.StatusBadRequest, "流水线事件格式无效")
	}
}

func writeOperationsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "运维结果集成尚未配置")
	case errors.Is(err, integration.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"changeguard-operations\"")
		writeError(w, http.StatusUnauthorized, "运维结果凭据无效")
	case errors.Is(err, integration.ErrReplay):
		writeError(w, http.StatusBadRequest, "运维结果事件时间超出允许范围")
	case errors.Is(err, integration.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "仅支持 INCIDENT、ROLLBACK 和 BUSINESS_SLI 事件")
	default:
		writeError(w, http.StatusBadRequest, "运维结果事件格式无效")
	}
}

func optionalTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func artifactKindLabel(kind model.ArtifactKind) string {
	switch kind {
	case model.ArtifactCode:
		return "代码变更"
	case model.ArtifactConfig:
		return "配置变更"
	case model.ArtifactKubernetes:
		return "Kubernetes 变更"
	case model.ArtifactAPI:
		return "API 契约变更"
	case model.ArtifactDatabase:
		return "数据库变更"
	default:
		return string(kind)
	}
}

func markdownLanguage(value string) string {
	var result strings.Builder
	for _, current := range strings.ToLower(strings.TrimSpace(value)) {
		if current >= 'a' && current <= 'z' || current >= '0' && current <= '9' || current == '+' || current == '#' || current == '-' || current == '_' {
			result.WriteRune(current)
		}
	}
	if result.Len() == 0 {
		return "text"
	}
	return result.String()
}

func codeFence(language, content string) string {
	fence := string([]byte{96, 96, 96})
	return fence + language + "\n" + content + "\n" + fence + "\n"
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

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		changes, err := s.service.ChangesFor(actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if r.URL.Query().Has("page") || r.URL.Query().Has("page_size") || r.URL.Query().Has("cursor") {
			pageSize := 50
			if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
				parsed, parseErr := strconv.Atoi(raw)
				if parseErr != nil || parsed < 1 || parsed > 200 {
					writeError(w, http.StatusBadRequest, "page_size 必须是 1 到 200 的整数")
					return
				}
				pageSize = parsed
			}
			start := 0
			if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
				found := false
				for index, item := range changes {
					if item.ID == raw {
						start = index + 1
						found = true
						break
					}
				}
				if !found {
					writeError(w, http.StatusBadRequest, "cursor 无效或已过期")
					return
				}
			} else if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
				page, parseErr := strconv.Atoi(raw)
				if parseErr != nil || page < 1 {
					writeError(w, http.StatusBadRequest, "page 必须是正整数")
					return
				}
				start = (page - 1) * pageSize
			}
			if start > len(changes) {
				start = len(changes)
			}
			end := start + pageSize
			if end > len(changes) {
				end = len(changes)
			}
			nextCursor := ""
			if end < len(changes) && end > start {
				nextCursor = changes[end-1].ID
			}
			writeJSON(w, http.StatusOK, map[string]any{"items": changes[start:end], "next_cursor": nextCursor, "has_more": end < len(changes), "total": len(changes)})
			return
		}
		writeJSON(w, http.StatusOK, changes)
	case http.MethodPost:
		var input model.CreateChangeInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "请求数据格式不正确")
			return
		}
		change, err := s.service.Create(input, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, change)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleChange(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/changes/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "变更单不存在")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			change, err := s.service.ChangeFor(id, actorID(r))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, change)
		case http.MethodPut:
			var input model.CreateChangeInput
			if err := decodeJSON(r, &input); err != nil {
				writeError(w, http.StatusBadRequest, "请求数据格式不正确")
				return
			}
			change, err := s.service.Update(id, input, actorID(r))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, change)
		default:
			methodNotAllowed(w)
		}
		return
	}
	action := parts[1]
	if action == "passports" {
		s.handleChangePassports(w, r, id, parts)
		return
	}
	if action == "agent-ask" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var input service.AskChangeAssistantInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "问题格式不正确")
			return
		}
		message, err := s.service.AskChangeAssistant(r.Context(), id, actorID(r), input)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, message)
		return
	}
	if action == "agent-conversations" {
		switch {
		case len(parts) == 2 && r.Method == http.MethodGet:
			items, err := s.service.AgentConversationsForChange(id, actorID(r))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, items)
		case len(parts) == 3 && r.Method == http.MethodGet:
			summary, err := s.service.AgentConversationFor(id, parts[2], actorID(r))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, summary)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if action == "report" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleChangeReport(w, id, r.URL.Query().Get("format"), actorID(r))
		return
	}
	if action == "findings" {
		if r.Method != http.MethodPost || len(parts) < 4 {
			methodNotAllowed(w)
			return
		}
		s.handleFindingAction(w, r, id, parts[2], parts[3])
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var (
		change model.ChangeRequest
		err    error
	)
	switch action {
	case "submit":
		change, err = s.service.Submit(id, actorID(r))
	case "experiment":
		key, ok := validatedIdempotencyKey(w, r)
		if !ok {
			return
		}
		if key == "" {
			w.Header().Set("Idempotency-Status", "not-requested")
			change, err = s.service.QueueExperiment(id, actorID(r))
		} else {
			var replayed bool
			change, replayed, err = s.service.QueueExperimentIdempotent(id, actorID(r), key, requestDigest(action, id, nil))
			if replayed {
				w.Header().Set("Idempotency-Replayed", "true")
			}
		}
	case "approve", "reject":
		var body struct{ Comment string }
		if decodeErr := decodeJSON(r, &body); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "审批意见格式不正确")
			return
		}
		if action == "approve" {
			key, ok := validatedIdempotencyKey(w, r)
			if !ok {
				return
			}
			if key == "" {
				w.Header().Set("Idempotency-Status", "not-requested")
				change, err = s.service.Approve(id, actorID(r), strings.TrimSpace(body.Comment))
			} else {
				var replayed bool
				change, replayed, err = s.service.ApproveIdempotent(id, actorID(r), key, requestDigest(action, id, body), strings.TrimSpace(body.Comment))
				if replayed {
					w.Header().Set("Idempotency-Replayed", "true")
				}
			}
		} else {
			change, err = s.service.Reject(id, actorID(r), strings.TrimSpace(body.Comment))
		}
	case "comments":
		var body struct{ Content string }
		if decodeErr := decodeJSON(r, &body); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "评论格式不正确")
			return
		}
		change, err = s.service.AddComment(id, actorID(r), body.Content)
	default:
		writeError(w, http.StatusNotFound, "未知操作")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, change)
}

func (s *Server) handlePassports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.service.PassportsFor(actorID(r))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleChangePassports(w http.ResponseWriter, r *http.Request, changeID string, parts []string) {
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			items, err := s.service.PassportsForChange(changeID, actorID(r))
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, items)
		case http.MethodPost:
			var input model.IssuePassportInput
			if err := decodeJSON(r, &input); err != nil {
				writeError(w, http.StatusBadRequest, "签发参数格式不正确")
				return
			}
			key, ok := validatedIdempotencyKey(w, r)
			if !ok {
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if key == "" {
				w.Header().Set("Idempotency-Status", "not-requested")
				credential, err := s.service.IssuePassport(changeID, actorID(r), input.TTLSeconds)
				if err != nil {
					writeServiceError(w, err)
					return
				}
				writeJSON(w, http.StatusCreated, credential)
				return
			}
			result, replayed, err := s.service.IssuePassportIdempotent(changeID, actorID(r), key, requestDigest("passports", changeID, input), input.TTLSeconds)
			if err != nil {
				writeServiceError(w, err)
				return
			}
			if replayed {
				w.Header().Set("Idempotency-Replayed", "true")
				writeJSON(w, http.StatusOK, result)
				return
			}
			writeJSON(w, http.StatusCreated, result.Credential)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) == 4 && parts[3] == "revoke" && r.Method == http.MethodPost {
		passport, err := s.service.RevokePassport(changeID, parts[2], actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, passport)
		return
	}
	writeError(w, http.StatusNotFound, "未知通行证操作")
}

func (s *Server) handleGateVerify(w http.ResponseWriter, r *http.Request) {
	s.handleGate(w, r, false)
}

func (s *Server) handleGateConsume(w http.ResponseWriter, r *http.Request) {
	s.handleGate(w, r, true)
}

func (s *Server) handleGate(w http.ResponseWriter, r *http.Request, consume bool) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var input model.GateRequest
	if err := decodeJSON(r, &input); err != nil {
		writeGateError(w, http.StatusBadRequest, "INVALID_REQUEST", "Gate 请求格式不正确")
		return
	}
	authorization := strings.Fields(strings.TrimSpace(r.Header.Get("Authorization")))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") || strings.TrimSpace(authorization[1]) == "" {
		writeGateError(w, http.StatusUnauthorized, "TOKEN_REQUIRED", "请通过 Authorization: Bearer 提交通行证")
		return
	}
	input.Token = authorization[1]
	result, err := s.service.VerifyGate(input, consume)
	if err != nil {
		writeGateServiceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func writeGateServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrPassportExpired):
		writeGateError(w, http.StatusGone, "PASSPORT_EXPIRED", err.Error())
	case errors.Is(err, service.ErrPassportReplay):
		writeGateError(w, http.StatusConflict, "PASSPORT_REPLAY", err.Error())
	case errors.Is(err, service.ErrPassportRevoked), errors.Is(err, service.ErrRuleSetChanged):
		writeGateError(w, http.StatusConflict, "PASSPORT_INACTIVE", err.Error())
	case errors.Is(err, service.ErrArtifactMismatch):
		writeGateError(w, http.StatusForbidden, "ARTIFACT_MISMATCH", err.Error())
	case errors.Is(err, service.ErrEnvironmentMismatch):
		writeGateError(w, http.StatusForbidden, "ENVIRONMENT_MISMATCH", err.Error())
	case errors.Is(err, service.ErrPassportInvalid):
		writeGateError(w, http.StatusForbidden, "PASSPORT_INVALID", err.Error())
	case errors.Is(err, service.ErrPassportUnavailable):
		writeGateError(w, http.StatusServiceUnavailable, "PASSPORT_UNAVAILABLE", err.Error())
	case errors.Is(err, service.ErrValidation):
		writeGateError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	default:
		writeGateError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Gate 服务处理失败")
	}
}

func writeGateError(w http.ResponseWriter, status int, code, reason string) {
	writeJSON(w, status, model.GateResult{Allowed: false, Code: code, Reason: reason})
}

func (s *Server) handleFindingAction(w http.ResponseWriter, r *http.Request, changeID, findingID, action string) {
	var (
		change model.ChangeRequest
		err    error
	)
	switch action {
	case "assign":
		var input model.AssignFindingInput
		if decodeErr := decodeJSON(r, &input); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "派单参数格式不正确")
			return
		}
		change, err = s.service.AssignFinding(changeID, findingID, actorID(r), input)
	case "resolve":
		var input model.ResolveFindingInput
		if decodeErr := decodeJSON(r, &input); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "整改参数格式不正确")
			return
		}
		change, err = s.service.ResolveFinding(changeID, findingID, actorID(r), input)
	case "verify":
		var input model.VerifyFindingInput
		if decodeErr := decodeJSON(r, &input); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "复核参数格式不正确")
			return
		}
		change, err = s.service.VerifyFinding(changeID, findingID, actorID(r), input)
	default:
		writeError(w, http.StatusNotFound, "未知风险处理操作")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, change)
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
	case errors.Is(err, service.ErrInvalidState), errors.Is(err, store.ErrConcurrentWrite), errors.Is(err, service.ErrRuleSetChanged), errors.Is(err, service.ErrPassportRevoked), errors.Is(err, service.ErrPassportReplay), errors.Is(err, store.ErrPassportInactive):
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
