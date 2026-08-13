package store

import (
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

// ensureDemoCoverage adds explicit DEMO_ONLY examples. They demonstrate the v1
// DATABASE/CONFIG/KUBERNETES boundary and never contain trusted execution results.
func ensureDemoCoverage(data *state, organization model.Organization, now time.Time) {
	if organization.ID != "org_demo" {
		return
	}
	applications := []model.Application{
		{OrganizationID: organization.ID, ID: "app_gateway", Name: "开放平台网关", Owner: "陈嘉", Kind: "平台服务", Runtime: "Go / Kubernetes", RepositoryURL: "https://git.example.com/platform/api-gateway", Tier: "核心", Lifecycle: "生产运行", Environment: "生产 / Kubernetes", Tags: []string{"网关", "限流"}, Description: "统一认证、路由与限流"},
		{OrganizationID: organization.ID, ID: "app_notification", Name: "消息通知服务", Owner: "赵可", Kind: "后端服务", Runtime: "Go / Kafka", RepositoryURL: "https://git.example.com/platform/notification-service", Tier: "重要", Lifecycle: "生产运行", Environment: "生产 / Kubernetes", Tags: []string{"短信", "Kafka"}, Description: "短信、邮件与站内信可靠投递"},
		{OrganizationID: organization.ID, ID: "app_file", Name: "文件中心", Owner: "周宁", Kind: "基础服务", Runtime: "Go / Kubernetes", RepositoryURL: "https://git.example.com/platform/file-service", Tier: "重要", Lifecycle: "生产运行", Environment: "生产 / Kubernetes", Tags: []string{"对象存储", "病毒扫描"}, Description: "文件上传、扫描和访问授权"},
	}
	existingApplications := make(map[string]bool, len(data.Applications))
	for _, application := range data.Applications {
		existingApplications[application.ID] = true
	}
	for _, application := range applications {
		if !existingApplications[application.ID] {
			data.Applications = append(data.Applications, application)
		}
	}

	applicationByID := make(map[string]model.Application, len(data.Applications))
	for _, application := range data.Applications {
		applicationByID[application.ID] = application
	}
	userByID := make(map[string]model.User, len(data.Users))
	for _, user := range data.Users {
		userByID[user.ID] = user
	}
	developer := userByID["usr_developer"]
	reviewer := userByID["usr_reviewer"]
	owner := userByID["usr_owner"]
	examples := []model.ChangeRequest{
		demoSecretConfigChange(organization.ID, applicationByID["app_notification"], developer, now),
		demoUnsafeKubernetesChange(organization.ID, applicationByID["app_file"], reviewer, now),
		demoConfigDraftChange(organization.ID, applicationByID["app_gateway"], developer, now),
		demoDDLNoConcurrentChange(organization.ID, applicationByID["app_order"], developer, now),
		demoBatchDMLChange(organization.ID, applicationByID["app_member"], developer, now),
		demoUnbatchedDMLChange(organization.ID, applicationByID["app_order"], developer, now),
		demoIndexMaintenanceChange(organization.ID, applicationByID["app_inventory"], developer, now),
		demoHeavyDDLChange(organization.ID, applicationByID["app_order"], developer, now),
		demoEmergencyRejectedChange(organization.ID, applicationByID["app_payment"], developer, reviewer, now),
		demoRollbackCompletedChange(organization.ID, applicationByID["app_order"], developer, owner, now),
		demoIncidentLinkedChange(organization.ID, applicationByID["app_payment"], developer, owner, now),
		demoAPIBreakingChange(organization.ID, applicationByID["app_gateway"], developer, now),
		demoPromotionCompletedChange(organization.ID, applicationByID["app_member"], developer, owner, now),
	}
	// Purge only STALE legacy demo records — IDs that the current seed no longer
	// provides (e.g. old chg_demo_gateway_limit / chg_demo_portal_draft). IDs that
	// ARE re-seeded must be left untouched so normalizeState stays restart-idempotent
	// (re-appending would rebuild their timestamps on every restart).
	legacyDemoIDs := map[string]bool{
		"chg_demo_api_break": true, "chg_demo_gateway_limit": true, "chg_demo_promotion_done": true,
		"chg_demo_portal_draft": true, "chg_demo_emergency_rejected": true,
	}
	exampleIDs := make(map[string]bool, len(examples))
	for _, example := range examples {
		exampleIDs[example.ID] = true
	}
	keptChanges := data.Changes[:0]
	for _, change := range data.Changes {
		if legacyDemoIDs[change.ID] && !exampleIDs[change.ID] {
			continue
		}
		keptChanges = append(keptChanges, change)
	}
	data.Changes = keptChanges
	keptAudits := data.Audits[:0]
	for _, audit := range data.Audits {
		if legacyDemoIDs[audit.ChangeID] && !exampleIDs[audit.ChangeID] {
			continue
		}
		keptAudits = append(keptAudits, audit)
	}
	data.Audits = keptAudits
	existingChanges := make(map[string]bool, len(data.Changes))
	for _, change := range data.Changes {
		existingChanges[change.ID] = true
	}
	for _, change := range examples {
		if !existingChanges[change.ID] {
			data.Changes = append(data.Changes, change)
		}
		auditID := "audit_demo_" + change.ID
		foundAudit := false
		for _, audit := range data.Audits {
			if audit.ID == auditID {
				foundAudit = true
				break
			}
		}
		if !foundAudit {
			data.Audits = append(data.Audits, model.AuditEvent{OrganizationID: organization.ID, ID: auditID, ChangeID: change.ID, ActorID: change.SubmitterID, ActorName: change.SubmitterName, Action: "DEMO_ONLY_CHANGE_SEEDED", Detail: "显式演示数据，不代表真实生产校验或执行结果：" + change.Title, CreatedAt: change.CreatedAt})
		}
	}

	existingGrants := make(map[string]bool, len(data.ApplicationGrants))
	for _, grant := range data.ApplicationGrants {
		existingGrants[grant.OrganizationID+"|"+grant.UserID+"|"+grant.ApplicationID] = true
	}
	for _, user := range data.Users {
		if user.OrganizationID != organization.ID {
			continue
		}
		for _, application := range applications {
			key := organization.ID + "|" + user.ID + "|" + application.ID
			if existingGrants[key] {
				continue
			}
			data.ApplicationGrants = append(data.ApplicationGrants, model.ApplicationGrant{OrganizationID: organization.ID, UserID: user.ID, ApplicationID: application.ID, CanSubmit: user.Role != model.RoleReviewer, CanReview: user.Role != model.RoleDeveloper, UpdatedBy: "system_demo_only", UpdatedAt: now})
		}
	}
}

func demoChangeBase(organizationID, id, title string, application model.Application, submitter model.User, status model.ChangeStatus, risk model.RiskLevel, createdAt, plannedAt time.Time) model.ChangeRequest {
	return model.ChangeRequest{
		OrganizationID: organizationID, ID: id, Title: title, ApplicationID: application.ID, ApplicationName: application.Name,
		Environment: "生产环境", ChangeType: "生产变更", RepositoryURL: application.RepositoryURL, Branch: "demo-only",
		Description: "DEMO_ONLY：用于展示规则命中与整改流程，不包含真实执行结果。", SubmitterID: submitter.ID, SubmitterName: submitter.Name,
		Status: status, Risk: risk, PlannedAt: plannedAt, CreatedAt: createdAt, UpdatedAt: createdAt, Version: 1,
		Timeline: []model.TimelineEntry{{ID: "tl_" + id + "_create", Status: model.StatusDraft, Title: "创建演示变更单", Detail: "DEMO_ONLY 数据，仅用于界面和流程演示", Actor: submitter.Name, CreatedAt: createdAt}},
	}
}

func demoSecretConfigChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-3 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_config_secret", "短信供应商配置安全整改", application, submitter, model.StatusCheckFailed, model.RiskHigh, createdAt, now.Add(5*time.Hour))
	change.ChangeType = "配置变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_config_secret", Kind: model.ArtifactConfig, Name: "短信通道配置", Source: "config/sms.yaml", Language: "YAML", Content: "provider: new-sms\napi_key: [REDACTED]\ndebug: true\nretry_count: 3"}}
	change.RollbackPlan = "恢复旧供应商配置，并将密钥改为 vault 引用。"
	dueAt := now.Add(12 * time.Hour)
	change.Findings = []model.Finding{
		{ID: "finding_demo_secret", Code: "CONFIG_SECRET_EXPOSURE", Severity: model.RiskHigh, Title: "配置包含疑似明文密钥", Detail: "原值已脱敏；演示如何阻断明文凭据进入生产。", Evidence: "api_key: [REDACTED]", Suggestion: "改用 Vault 或环境变量引用。", Blocking: true, RuleVersion: 1, Status: model.FindingAssigned, OwnerID: submitter.ID, OwnerName: submitter.Name, DueAt: &dueAt, UpdatedAt: createdAt.Add(3 * time.Minute)},
		{ID: "finding_demo_debug", Code: "CONFIG_DEBUG_ENABLED", Severity: model.RiskHigh, Title: "生产配置开启调试模式", Detail: "debug: true 会扩大敏感日志暴露。", Evidence: "debug: true", Suggestion: "生产环境关闭调试模式。", Blocking: true, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(3 * time.Minute)},
	}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_secret_check", Status: model.StatusCheckFailed, Title: "确定性配置检查阻断", Detail: "DEMO_ONLY：命中明文密钥痕迹与调试开关", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(3 * time.Minute)})
	return change
}

func demoUnsafeKubernetesChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-4 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_k8s_unsafe", "文件扫描 Worker 安全基线整改", application, submitter, model.StatusCheckFailed, model.RiskHigh, createdAt, now.Add(6*time.Hour))
	change.ChangeType = "Kubernetes 变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_k8s_unsafe", Kind: model.ArtifactKubernetes, Name: "扫描 Worker Deployment", Source: "deploy/scanner.yaml", Language: "YAML", Content: "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: scanner\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n        - name: scanner\n          image: registry.example.com/file-scanner:latest\n          securityContext:\n            privileged: true"}}
	change.RollbackPlan = "回退固定版本镜像并恢复安全上下文。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_latest", Code: "K8S_LATEST_IMAGE", Severity: model.RiskHigh, Title: "生产镜像使用 latest", Detail: "无法绑定不可变制品。", Evidence: "image=:latest", Suggestion: "使用固定版本或 digest。", Blocking: true, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(4 * time.Minute)},
		{ID: "finding_demo_privileged", Code: "K8S_PRIVILEGED", Severity: model.RiskHigh, Title: "容器启用特权模式", Detail: "扩大容器逃逸影响。", Evidence: "privileged=true", Suggestion: "移除 privileged。", Blocking: true, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(4 * time.Minute)},
	}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_k8s_check", Status: model.StatusCheckFailed, Title: "确定性 Kubernetes 检查阻断", Detail: "DEMO_ONLY：镜像和权限不满足生产基线", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(4 * time.Minute)})
	return change
}

func demoConfigDraftChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-20 * time.Minute)
	change := demoChangeBase(organizationID, "chg_demo_config_draft", "网关限流参数调整草稿", application, submitter, model.StatusDraft, model.RiskUnknown, createdAt, now.Add(24*time.Hour))
	change.ChangeType = "配置变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_gateway_config", Kind: model.ArtifactConfig, Name: "限流策略配置", Source: "config/rate-limit.yaml", Language: "YAML", Content: "tenant_rate_limit:\n  enabled: true\n  default_qps: 500\n  burst: 100"}}
	change.RollbackPlan = "恢复上一版限流参数并重新加载网关配置。"
	return change
}

func demoDDLNoConcurrentChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-90 * time.Minute)
	change := demoChangeBase(organizationID, "chg_demo_idx_offline", "订单历史分区表旧索引下线", application, submitter, model.StatusWaitingApproval, model.RiskHigh, createdAt, now.Add(2*time.Hour))
	change.ChangeType = "DDL"
	change.SQL = "DROP INDEX IF EXISTS ux_orders_legacy_status;"
	change.RollbackSQL = "CREATE INDEX IF NOT EXISTS ux_orders_legacy_status ON orders(status) WHERE archived_at IS NULL;"
	change.Description = "下线历史分区上长期未使用的大索引，释放存储并降低写入开销。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_idx_drop", Kind: model.ArtifactDatabase, Name: "订单分区表 DDL", Source: "migrations/024_drop_legacy_index.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "如出现性能回退，按回滚脚本重建受控索引。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_idx_drop_meta", Code: "DDL_FULL_SCAN", Severity: model.RiskMedium, Title: "索引删除可能在活跃分区触发长时间扫描", Detail: "该索引同时服务订单历史查询，删除需在低峰窗口进行。", Evidence: "DROP INDEX", Suggestion: "拆分批次并在维护窗口执行。", Blocking: false, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(5 * time.Minute)},
		{ID: "finding_demo_idx_drop_lock", Code: "DDL_LOCK_IMPACT", Severity: model.RiskHigh, Title: "线上 DDL 未使用 CONCURRENTLY", Detail: "DROP INDEX 直接执行会阻塞并发写入。", Evidence: "DROP INDEX IF EXISTS", Suggestion: "使用维护窗口或先改为占位再重建。", Blocking: true, RuleVersion: 1, Status: model.FindingAssigned, OwnerID: submitter.ID, OwnerName: submitter.Name, DueAt: &[]time.Time{now.Add(4 * time.Hour)}[0], UpdatedAt: createdAt.Add(5 * time.Minute)},
	}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_idx_drop_check", Status: model.StatusCheckFailed, Title: "确定性 DDL 检查提示锁影响", Detail: "DEMO_ONLY：命中非 CONCURRENTLY 与全表扫描风险", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(5 * time.Minute)})
	change.Analysis = &model.AgentAnalysis{Provider: "rules-fallback", Risk: model.RiskHigh, Summary: "旧索引存在长时间不可用风险，建议维护窗口分批执行。", Reasons: []string{"DROP INDEX 需保持排他锁", "历史分区重建成本较高"}, Suggestions: []string{"低峰期窗口执行", "执行后监控慢查询变化"}, EvidenceIDs: []string{"finding_demo_idx_drop_lock"}, Steps: 2, ToolCalls: 2, GeneratedAt: createdAt.Add(6 * time.Minute)}
	return change
}

func demoBatchDMLChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-3 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_points_archive", "会员积分过期批量归档", application, submitter, model.StatusReadyForExperiment, model.RiskMedium, createdAt, now.Add(5*time.Hour))
	change.ChangeType = "DML"
	// Good practice: SET LOCAL timeouts + LIMIT batch boundary.
	change.SQL = "SET LOCAL lock_timeout = '2s'; SET LOCAL statement_timeout = '30s'; UPDATE member_points SET status='EXPIRED' WHERE expires_at < NOW() - INTERVAL '7 days' AND status='ACTIVE' LIMIT 10000;"
	change.RollbackSQL = "UPDATE member_points SET status='ACTIVE' WHERE status='EXPIRED' AND updated_at >= NOW() - INTERVAL '1 hour' LIMIT 10000;"
	change.Description = "对超期积分进行分批归档，单批上限 1 万行，并声明锁/语句超时，避免大事务。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_batch_dml", Kind: model.ArtifactDatabase, Name: "积分归档 DML", Source: "scripts/archive_points.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "停止调度并执行回滚 UPDATE 恢复积分状态。"
	change.Experiment = &model.ExperimentReport{ID: "exp_demo_batch", Kind: "DEMO_ONLY", Mode: "DEMO_ONLY", Status: "PASSED", StartedAt: createdAt.Add(20 * time.Minute), FinishedAt: createdAt.Add(24 * time.Minute), DurationMS: 2400, DatasetRows: 10000, LockWaitMS: 40, P99BeforeMS: 8.2, P99AfterMS: 8.5, FailedTransactions: 0, RollbackVerified: true, ChecksTotal: 4, ChecksPassed: 4, ExecutionError: "显式演示数据未执行真实 PostgreSQL 影子演练", Evidence: []model.Evidence{{ID: "ev_demo_batch_timeout", Kind: "check", Title: "锁/语句超时基线", Value: "lock_timeout=2s; statement_timeout=30s", Source: "变更脚本", ObservedAt: createdAt.Add(24 * time.Minute)}, {ID: "ev_demo_batch_limit", Kind: "check", Title: "分批边界", Value: "LIMIT 10000", Source: "变更脚本", ObservedAt: createdAt.Add(24 * time.Minute)}}}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_batch_exp", Status: model.StatusReadyForExperiment, Title: "影子库演练通过", Detail: "DEMO_ONLY：批量上限与锁等待符合预期", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(24 * time.Minute)})
	return change
}

func demoUnbatchedDMLChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-110 * time.Minute)
	change := demoChangeBase(organizationID, "chg_demo_orders_unbatched", "历史订单状态一次性回填", application, submitter, model.StatusCheckFailed, model.RiskMedium, createdAt, now.Add(6*time.Hour))
	change.ChangeType = "DML"
	change.SQL = "UPDATE orders SET archive_flag=1 WHERE created_at < NOW() - INTERVAL '180 days' AND archive_flag=0;"
	change.RollbackSQL = "UPDATE orders SET archive_flag=0 WHERE archive_flag=1 AND updated_at >= NOW() - INTERVAL '2 hours';"
	change.Description = "演示缺少分批边界的大事务 DML：条件更新可能锁住海量行。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_unbatched_dml", Kind: model.ArtifactDatabase, Name: "订单归档回填", Source: "scripts/archive_orders_once.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "停止任务并按时间窗口反向 UPDATE。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_unbatched_dml", Code: "UNBATCHED_LARGE_DML", Severity: model.RiskMedium, Title: "大批量 DML 缺少分批边界", Detail: "条件 UPDATE 未声明 LIMIT，可能形成长事务并放大锁等待。", Evidence: "UPDATE ... WHERE ... (no LIMIT)", Suggestion: "按主键游标分批，单批限制行数并控制提交节奏。", Blocking: false, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(4 * time.Minute)},
		{ID: "finding_demo_missing_lock_timeout", Code: "MISSING_LOCK_TIMEOUT", Severity: model.RiskMedium, Title: "高锁风险变更未声明锁超时", Detail: "未设置 lock_timeout/statement_timeout，冲突时可能长时间阻塞业务。", Evidence: "lock_timeout 未声明", Suggestion: "在执行脚本声明 SET lock_timeout / statement_timeout。", Blocking: false, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(4 * time.Minute)},
	}
	change.Analysis = &model.AgentAnalysis{Provider: "rules-fallback", Risk: model.RiskMedium, Summary: "大事务回填缺少分批与超时保护，建议先整改再演练。", Reasons: []string{"无 LIMIT 的条件 UPDATE", "未声明 lock_timeout"}, Suggestions: []string{"按 id 游标分批 1 万行", "声明 lock_timeout=2s"}, EvidenceIDs: []string{"finding_demo_unbatched_dml"}, Steps: 2, ToolCalls: 1, GeneratedAt: createdAt.Add(5 * time.Minute)}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_unbatched_check", Status: model.StatusCheckFailed, Title: "事务优化检查提示大事务风险", Detail: "DEMO_ONLY：命中未分批 DML 与缺失锁超时", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(4 * time.Minute)})
	return change
}

func demoIndexMaintenanceChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-7 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_inventory_fk", "库存预占表补充外键约束", application, submitter, model.StatusCheckFailed, model.RiskHigh, createdAt, now.Add(3*time.Hour))
	change.ChangeType = "DDL"
	change.SQL = "ALTER TABLE inventory_reservation ADD CONSTRAINT fk_reservation_sku FOREIGN KEY (sku_id) REFERENCES sku(id);"
	change.RollbackSQL = "ALTER TABLE inventory_reservation DROP CONSTRAINT IF EXISTS fk_reservation_sku;"
	change.Description = "为预占记录补充外键约束；演示未使用 NOT VALID 分阶段时的长事务锁风险。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_fk_add", Kind: model.ArtifactDatabase, Name: "库存外键 DDL", Source: "migrations/022_add_fk.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "执行 DROP CONSTRAINT 回滚外键。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_fk_not_valid", Code: "FK_WITHOUT_NOT_VALID", Severity: model.RiskHigh, Title: "外键约束缺少 NOT VALID 分阶段", Detail: "直接 ADD FOREIGN KEY 会在创建时扫描整表校验，长事务期间持有锁。", Evidence: "ADD CONSTRAINT ... FOREIGN KEY (no NOT VALID)", Suggestion: "先 ADD CONSTRAINT ... NOT VALID，低峰期再 VALIDATE CONSTRAINT。", Blocking: true, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(6 * time.Minute)},
		{ID: "finding_demo_fk_lock_timeout", Code: "MISSING_LOCK_TIMEOUT", Severity: model.RiskMedium, Title: "高锁风险变更未声明锁超时", Detail: "外键校验可能长时间持锁。", Evidence: "lock_timeout 未声明", Suggestion: "声明 lock_timeout 并在低峰窗口执行。", Blocking: false, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(6 * time.Minute)},
	}
	change.Experiment = &model.ExperimentReport{ID: "exp_demo_fk", Kind: "DEMO_ONLY", Mode: "DEMO_ONLY", Status: "FAILED", StartedAt: createdAt.Add(2 * time.Hour), FinishedAt: createdAt.Add(2*time.Hour + 45*time.Second), DurationMS: 45000, DatasetRows: 2000000, LockWaitMS: 2100, FailedTransactions: 1, ExecutionError: "DEMO_ONLY：模拟外键整表校验触发 lock timeout", Evidence: []model.Evidence{{ID: "ev_demo_fk_lock", Kind: "check", Title: "事务失败分类", Value: "LOCK_TIMEOUT：锁等待超过阈值，建议拆分变更、低峰执行或改用 CONCURRENTLY/NOT VALID", Source: "影子事务诊断", ObservedAt: createdAt.Add(2*time.Hour + 45*time.Second)}}}
	change.Timeline = append(change.Timeline,
		model.TimelineEntry{ID: "tl_demo_fk_check", Status: model.StatusCheckFailed, Title: "外键事务优化检查阻断", Detail: "DEMO_ONLY：命中 FK_WITHOUT_NOT_VALID", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(6 * time.Minute)},
		model.TimelineEntry{ID: "tl_demo_fk_exp", Status: model.StatusExperimentRunning, Title: "影子库演练失败", Detail: "DEMO_ONLY：模拟 200 万行表外键校验超时", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(2*time.Hour + 45*time.Second)},
	)
	return change
}

func demoHeavyDDLChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-50 * time.Minute)
	change := demoChangeBase(organizationID, "chg_demo_vacuum_full", "订单表空间回收 VACUUM FULL", application, submitter, model.StatusCheckFailed, model.RiskHigh, createdAt, now.Add(8*time.Hour))
	change.ChangeType = "DDL"
	change.SQL = "VACUUM FULL orders;"
	change.RollbackSQL = "-- VACUUM FULL 不可按语句回滚；保留逻辑备份恢复路径"
	change.Description = "演示重写型 DDL 的长事务与锁风险。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_vacuum_full", Kind: model.ArtifactDatabase, Name: "订单表空间回收", Source: "ops/vacuum_full_orders.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "从逻辑备份恢复受影响分区；生产禁止直接 VACUUM FULL。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_heavy_ddl", Code: "HEAVY_DDL_REWRITE", Severity: model.RiskHigh, Title: "重写型 DDL 长事务风险", Detail: "VACUUM FULL 会触发表重写并长时间阻塞读写。", Evidence: "VACUUM FULL orders", Suggestion: "改用在线清理策略或维护窗口分片执行，并设置 lock_timeout。", Blocking: true, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(3 * time.Minute)},
		{ID: "finding_demo_heavy_timeout", Code: "MISSING_LOCK_TIMEOUT", Severity: model.RiskMedium, Title: "高锁风险变更未声明锁超时", Detail: "重写型 DDL 未声明超时保护。", Evidence: "lock_timeout 未声明", Suggestion: "声明 lock_timeout/statement_timeout 并准备可观测回滚。", Blocking: false, RuleVersion: 1, Status: model.FindingOpen, UpdatedAt: createdAt.Add(3 * time.Minute)},
	}
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_heavy_ddl", Status: model.StatusCheckFailed, Title: "重写型 DDL 被事务优化规则阻断", Detail: "DEMO_ONLY：命中 HEAVY_DDL_REWRITE", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(3 * time.Minute)})
	return change
}

func demoEmergencyRejectedChange(organizationID string, application model.Application, submitter, reviewer model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-26 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_emergency_rejected", "支付网关故障应急参数调整", application, submitter, model.StatusRejected, model.RiskHigh, createdAt, now.Add(-20*time.Hour))
	change.ChangeType = "配置变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_emergency", Kind: model.ArtifactConfig, Name: "支付网关超时配置", Source: "config/payment-timeout.yaml", Language: "YAML", Content: "payment_timeout_ms: 3000\nretry_policy:\n  max_retries: 1\n  backoff_ms: 200\ncallback:\n  enabled: false"}}
	change.RollbackPlan = "恢复标准超时配置。"
	change.Findings = []model.Finding{
		{ID: "finding_demo_emergency_cb", Code: "CONFIG_CALLBACK_DISABLED", Severity: model.RiskHigh, Title: "回调被禁用将影响对账", Detail: "应急参数跳过了支付回调，存在资金核对缺口。", Evidence: "callback.enabled=false", Suggestion: "恢复回调或补充人工对账。", Blocking: true, RuleVersion: 1, Status: model.FindingVerified, OwnerID: submitter.ID, OwnerName: submitter.Name, Resolution: "应急窗口结束已恢复回调", VerifiedByID: reviewer.ID, VerifiedByName: reviewer.Name, VerificationComment: "已确认恢复", UpdatedAt: createdAt.Add(30 * time.Minute)},
	}
	change.ReviewerID = reviewer.ID
	change.ReviewerName = reviewer.Name
	change.ReviewComment = "应急变更缺少可观测性闭环，拒绝放行并要求恢复回调后重提。"
	change.Timeline = append(change.Timeline,
		model.TimelineEntry{ID: "tl_demo_emergency_submit", Status: model.StatusDraft, Title: "应急变更提交", Detail: "故障处置通道", Actor: submitter.Name, CreatedAt: createdAt},
		model.TimelineEntry{ID: "tl_demo_emergency_reject", Status: model.StatusRejected, Title: "审核人拒绝", Detail: "要求补齐回调与对账方案", Actor: reviewer.Name, CreatedAt: createdAt.Add(2 * time.Hour)},
	)
	return change
}

func demoRollbackCompletedChange(organizationID string, application model.Application, submitter, reviewer model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-72 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_order_cache_ttl", "订单缓存 TTL 缩短为 30 秒", application, submitter, model.StatusCompleted, model.RiskLow, createdAt, now.Add(-60*time.Hour))
	change.ChangeType = "配置变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_cache_ttl", Kind: model.ArtifactConfig, Name: "缓存 TTL 配置", Source: "config/cache.yaml", Language: "YAML", Content: "order_cache_ttl_seconds: 30\nnegative_cache_ttl_seconds: 5"}}
	change.RollbackPlan = "恢复 300 秒 TTL 配置。"
	change.ReviewerID = reviewer.ID
	change.ReviewerName = reviewer.Name
	change.ReviewComment = "同意缩短缓存 TTL 以降低脏读窗口。"
	change.Timeline = append(change.Timeline,
		model.TimelineEntry{ID: "tl_demo_cache_ttl_review", Status: model.StatusApproved, Title: "审批通过", Detail: "低风险参数调整", Actor: reviewer.Name, CreatedAt: now.Add(-66 * time.Hour)},
		model.TimelineEntry{ID: "tl_demo_cache_ttl_done", Status: model.StatusCompleted, Title: "变更完成", Detail: "配置已生效，观察无回退", Actor: submitter.Name, CreatedAt: now.Add(-60 * time.Hour)},
	)
	return change
}

func demoIncidentLinkedChange(organizationID string, application model.Application, submitter, reviewer model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-5 * 24 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_pay_default", "支付渠道表默认值修复", application, submitter, model.StatusCompleted, model.RiskHigh, createdAt, now.Add(-4*24*time.Hour))
	change.ChangeType = "DDL"
	change.SQL = "ALTER TABLE payment_channel ALTER COLUMN callback_url SET DEFAULT 'PENDING';"
	change.RollbackSQL = "ALTER TABLE payment_channel ALTER COLUMN callback_url DROP DEFAULT;"
	change.Description = "修复新渠道无回调地址导致对账中断的问题，并关联事故复盘。"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_null_default", Kind: model.ArtifactDatabase, Name: "支付渠道默认值修复", Source: "migrations/021_fix_default.sql", Language: "SQL", Content: change.SQL}}
	change.RollbackPlan = "恢复无默认值状态并通知对账组。"
	change.ReviewerID = reviewer.ID
	change.ReviewerName = reviewer.Name
	change.ReviewComment = "事故复盘要求：默认值修复必须先在影子库验证。"
	change.Timeline = append(change.Timeline,
		model.TimelineEntry{ID: "tl_demo_null_default_incident", Status: model.StatusDraft, Title: "关联事故 INC-20260719", Detail: "回填事故关联与复盘结论", Actor: submitter.Name, CreatedAt: now.Add(-5 * 24 * time.Hour)},
		model.TimelineEntry{ID: "tl_demo_null_default_done", Status: model.StatusCompleted, Title: "修复已上线并验证", Detail: "对账中断恢复", Actor: reviewer.Name, CreatedAt: now.Add(-4 * 24 * time.Hour)},
	)
	return change
}

func demoAPIBreakingChange(organizationID string, application model.Application, submitter model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-2 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_api_break", "订单查询接口契约升级", application, submitter, model.StatusReadyForExperiment, model.RiskMedium, createdAt, now.Add(4*time.Hour))
	change.ChangeType = "API 变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_api_break", Kind: model.ArtifactAPI, Name: "OpenAPI 契约", Source: "api/openapi/orders.yaml", Language: "YAML", Content: "paths:\n  /v1/orders:\n    get:\n      parameters:\n        - name: cursor\n          in: query\n          required: true\n          schema:\n            type: string"}}
	change.RollbackPlan = "保留旧接口灰度期 30 天，回退路由即可恢复。"
	change.Timeline = append(change.Timeline, model.TimelineEntry{ID: "tl_demo_api_break_ready", Status: model.StatusReadyForExperiment, Title: "契约兼容性检查完成", Detail: "DEMO_ONLY：识别破坏性变更，需走双跑灰度", Actor: "ChangeGuard Worker", CreatedAt: createdAt.Add(15 * time.Minute)})
	return change
}

func demoPromotionCompletedChange(organizationID string, application model.Application, submitter, reviewer model.User, now time.Time) model.ChangeRequest {
	createdAt := now.Add(-10 * 24 * time.Hour)
	change := demoChangeBase(organizationID, "chg_demo_promotion_done", "大促参数预热与容量评估", application, submitter, model.StatusCompleted, model.RiskLow, createdAt, now.Add(-9*24*time.Hour))
	change.ChangeType = "配置变更"
	change.Artifacts = []model.ChangeArtifact{{ID: "artifact_demo_promotion", Kind: model.ArtifactConfig, Name: "大促参数", Source: "config/promotion-2026.yaml", Language: "YAML", Content: "promotion_mode: enabled\npeak_qps_capacity: 5000\ndowngrade_ratio: 0.2\nobserve_window_minutes: 120"}}
	change.RollbackPlan = "关闭大促模式并恢复默认容量评估。"
	change.ReviewerID = reviewer.ID
	change.ReviewerName = reviewer.Name
	change.ReviewComment = "容量评估通过，允许大促窗口启用。"
	change.Timeline = append(change.Timeline,
		model.TimelineEntry{ID: "tl_demo_promotion_done", Status: model.StatusCompleted, Title: "大促窗口结束，变更闭环", Detail: "流量回落，无事故", Actor: reviewer.Name, CreatedAt: now.Add(-9 * 24 * time.Hour)},
	)
	return change
}
