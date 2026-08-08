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
	legacyDemoIDs := map[string]bool{
		"chg_demo_api_break": true, "chg_demo_gateway_limit": true, "chg_demo_promotion_done": true,
		"chg_demo_portal_draft": true, "chg_demo_emergency_rejected": true,
	}
	keptChanges := data.Changes[:0]
	for _, change := range data.Changes {
		if !legacyDemoIDs[change.ID] {
			keptChanges = append(keptChanges, change)
		}
	}
	data.Changes = keptChanges
	keptAudits := data.Audits[:0]
	for _, audit := range data.Audits {
		if !legacyDemoIDs[audit.ChangeID] {
			keptAudits = append(keptAudits, audit)
		}
	}
	data.Audits = keptAudits
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
	examples := []model.ChangeRequest{
		demoSecretConfigChange(organization.ID, applicationByID["app_notification"], developer, now),
		demoUnsafeKubernetesChange(organization.ID, applicationByID["app_file"], reviewer, now),
		demoConfigDraftChange(organization.ID, applicationByID["app_gateway"], developer, now),
	}
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
