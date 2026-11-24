package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/checker"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

// Create 创建一个正式变更单（无幂等键）。兼容既有调用方；需要幂等请用 CreateIdempotent。
//
// 它**不接受**输入里的 agent_task_id：可信的 Agent 关联只能由 CreateIdempotent 在服务端
// 核对通过后传入。这样"来源标记"就不会退化成客户端可以自己填的字段。
func (s *Service) Create(input model.CreateChangeInput, actorID string) (model.ChangeRequest, error) {
	return s.create(input, actorID, "", "")
}

// CreateVerified 用**服务端已核对通过**的 Agent 任务 ID 创建变更单（无幂等键）。
// 与 Create 的区别只有一点：它允许携带可信的 agent_task_id。
func (s *Service) CreateVerified(input model.CreateChangeInput, actorID, verifiedAgentTaskID string) (model.ChangeRequest, error) {
	return s.create(input, actorID, "", verifiedAgentTaskID)
}

// create 是所有创建路径的唯一实现。agentTaskID 是**服务端核对通过**的任务 ID（可为空）；
// 客户端在请求体里声明的 agent_task_id 必须与它一致，否则拒绝。
func (s *Service) create(input model.CreateChangeInput, actorID, requestKey, agentTaskID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	app, err := s.store.Application(input.ApplicationID)
	if err != nil || app.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, app.ID, "submit") {
		return model.ChangeRequest{}, ErrValidation
	}
	title := strings.TrimSpace(input.Title)
	sqlText := strings.TrimSpace(input.SQL)
	rollbackText := strings.TrimSpace(input.RollbackSQL)
	rollbackPlan := strings.TrimSpace(input.RollbackPlan)
	if changegate.Redact(input.SQL) != input.SQL || changegate.Redact(input.RollbackSQL) != input.RollbackSQL {
		return model.ChangeRequest{}, fmt.Errorf("%w：SQL 或回滚 SQL 包含疑似凭据，禁止脱敏后执行；请改用受管密钥引用", ErrValidation)
	}
	artifacts, err := normalizeArtifacts(input.Artifacts, input.SQL)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	environment := defaultString(input.Environment, "生产环境")
	changeType := strings.TrimSpace(input.ChangeType)
	if changeType == "" {
		changeType = deriveChangeType(artifacts)
	}
	sqlSHA256 := changegate.SHA256(input.SQL)
	rollbackSHA256 := changegate.SHA256(input.RollbackSQL)
	artifactSHA256 := changegate.ChangeDigest(environment, changeType, artifacts, sqlSHA256, rollbackSHA256, input.RollbackPlan)
	if hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：DATABASE 制品必须同时提供可执行 SQL，不能绕过 PostgreSQL 影子演练", ErrValidation)
	}
	if !validRuneLength(title, 1, 120) || !validRuneLength(strings.TrimSpace(input.Environment), 0, 64) ||
		!validRuneLength(strings.TrimSpace(input.ChangeType), 0, 64) || !validRuneLength(strings.TrimSpace(input.Description), 0, 4000) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) || !validRuneLength(strings.TrimSpace(input.Branch), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.CommitSHA), 0, 120) || len(sqlText) > 1<<20 || len(rollbackText) > 1<<20 ||
		len(rollbackPlan) > 8000 || artifactBytes(artifacts) > 2<<20 || (sqlText == "" && len(artifacts) == 0) ||
		(hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "") {
		return model.ChangeRequest{}, fmt.Errorf("%w：标题、环境、仓库信息或变更证据不符合要求", ErrValidation)
	}
	// 可信关联只认服务端核对过的值：客户端声明的 ID 必须与它一致，否则拒绝。
	agentTaskID = strings.TrimSpace(agentTaskID)
	if declared := strings.TrimSpace(input.AgentTaskID); declared != agentTaskID {
		return model.ChangeRequest{}, fmt.Errorf("%w：agent_task_id 必须先通过服务端核对，不能由请求直接声明", ErrValidation)
	}
	if !validAgentTaskID(agentTaskID) {
		return model.ChangeRequest{}, fmt.Errorf("%w：agent_task_id 不合法（仅允许字母、数字、下划线和连字符，最长 128 字符）", ErrValidation)
	}
	now := time.Now()
	if input.PlannedAt.IsZero() {
		input.PlannedAt = now.Add(24 * time.Hour)
	} else if !input.PlannedAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：计划执行时间必须晚于当前时间", ErrValidation)
	}
	change := model.ChangeRequest{
		OrganizationID: actor.OrganizationID, ID: store.NewID("chg_"), Title: strings.TrimSpace(input.Title),
		Source: changeSource(agentTaskID), AgentTaskID: agentTaskID, RequestKey: requestKey,
		ApplicationID: app.ID, ApplicationName: app.Name,
		Environment:   environment,
		ChangeType:    changeType,
		RepositoryURL: strings.TrimSpace(input.RepositoryURL), Branch: strings.TrimSpace(input.Branch), CommitSHA: strings.TrimSpace(input.CommitSHA),
		ArtifactSHA256: artifactSHA256, SQLSHA256: sqlSHA256, RollbackSHA256: rollbackSHA256,
		Artifacts: artifacts,
		SQL:       changegate.Redact(sqlText), RollbackSQL: changegate.Redact(rollbackText),
		RollbackPlan: changegate.Redact(rollbackPlan), ReleasePlan: normalizeReleasePlan(input.ReleasePlan),
		Description: changegate.Redact(strings.TrimSpace(input.Description)),
		SubmitterID: actor.ID, SubmitterName: actor.Name,
		Status: model.StatusDraft, Risk: model.RiskUnknown,
		PlannedAt: input.PlannedAt, CreatedAt: now, UpdatedAt: now, Version: 1, ArtifactVersion: 1,
		Timeline: []model.TimelineEntry{timeline(model.StatusDraft, "创建统一变更单", fmt.Sprintf("已登记 %d 类变更证据、回滚方案和计划窗口", len(artifacts)), actor.Name)},
	}
	if err := s.store.CreateChange(change, audit(actor, change.ID, "CREATE", "创建研发发布变更单")); err != nil {
		return model.ChangeRequest{}, err
	}
	s.publish(change, "变更单已创建")
	return change, nil
}

// validAgentTaskID 接受空值或一个受限字符集的非空标识，避免把任意文本塞进关联字段。
func validAgentTaskID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// changeSource 由服务端根据是否携带 Agent 任务关联判定来源；调用方不能直接声明 source。
func changeSource(agentTaskID string) string {
	if strings.TrimSpace(agentTaskID) != "" {
		return "agent_task"
	}
	return "console"
}

// trustedReleaseSources 是**允许进入生产放行流程**的来源白名单，失败关闭：
// 未列出的来源（例如评测 / 演示产物）不得提交、审批或签发通行证。空串是历史记录，
// 按 console 处理。来源标记只是溯源，不是授权凭据；这份白名单只做"来源不被信任时拒绝"，
// 不会因为来源被信任就跳过其它任何检查。
var trustedReleaseSources = map[string]bool{"": true, "console": true, "agent_task": true}

func trustedForRelease(change model.ChangeRequest) bool {
	return trustedReleaseSources[strings.TrimSpace(change.Source)]
}

func releaseSourceRefusal(change model.ChangeRequest) error {
	return fmt.Errorf("%w：变更来源 %q 不允许进入生产放行流程（来源标记不是授权凭据）", ErrForbidden, change.Source)
}

func (s *Service) Update(id string, input model.CreateChangeInput, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || change.SubmitterID != actor.ID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusDraft && change.Status != model.StatusCheckFailed && change.Status != model.StatusRejected && change.Status != model.StatusApproved {
		return model.ChangeRequest{}, ErrInvalidState
	}
	app, err := s.store.Application(input.ApplicationID)
	if err != nil || app.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, app.ID, "submit") {
		return model.ChangeRequest{}, ErrValidation
	}
	title := strings.TrimSpace(input.Title)
	sqlText := strings.TrimSpace(input.SQL)
	rollbackText := strings.TrimSpace(input.RollbackSQL)
	rollbackPlan := strings.TrimSpace(input.RollbackPlan)
	if changegate.Redact(input.SQL) != input.SQL || changegate.Redact(input.RollbackSQL) != input.RollbackSQL {
		return model.ChangeRequest{}, fmt.Errorf("%w：SQL 或回滚 SQL 包含疑似凭据，禁止脱敏后执行；请改用受管密钥引用", ErrValidation)
	}
	artifacts, err := normalizeArtifacts(input.Artifacts, input.SQL)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	environment := defaultString(input.Environment, change.Environment)
	changeType := strings.TrimSpace(input.ChangeType)
	if changeType == "" {
		changeType = deriveChangeType(artifacts)
	}
	sqlSHA256 := changegate.SHA256(input.SQL)
	rollbackSHA256 := changegate.SHA256(input.RollbackSQL)
	artifactSHA256 := changegate.ChangeDigest(environment, changeType, artifacts, sqlSHA256, rollbackSHA256, input.RollbackPlan)
	if hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：DATABASE 制品必须同时提供可执行 SQL，不能绕过 PostgreSQL 影子演练", ErrValidation)
	}
	if !validRuneLength(title, 1, 120) || !validRuneLength(strings.TrimSpace(input.Environment), 0, 64) ||
		!validRuneLength(strings.TrimSpace(input.ChangeType), 0, 64) || !validRuneLength(strings.TrimSpace(input.Description), 0, 4000) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) || !validRuneLength(strings.TrimSpace(input.Branch), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.CommitSHA), 0, 120) || len(sqlText) > 1<<20 || len(rollbackText) > 1<<20 ||
		len(rollbackPlan) > 8000 || artifactBytes(artifacts) > 2<<20 || (sqlText == "" && len(artifacts) == 0) ||
		(hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "") {
		return model.ChangeRequest{}, fmt.Errorf("%w：标题、环境、仓库信息或变更证据不符合要求", ErrValidation)
	}
	now := time.Now()
	if input.PlannedAt.IsZero() {
		input.PlannedAt = change.PlannedAt
	}
	if !input.PlannedAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：计划执行时间必须晚于当前时间", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		if item.Version != change.Version {
			return ErrInvalidState
		}
		item.Title = strings.TrimSpace(input.Title)
		item.ApplicationID = app.ID
		item.ApplicationName = app.Name
		item.Environment = environment
		item.ChangeType = changeType
		item.RepositoryURL = strings.TrimSpace(input.RepositoryURL)
		item.Branch = strings.TrimSpace(input.Branch)
		item.CommitSHA = strings.TrimSpace(input.CommitSHA)
		item.ArtifactSHA256 = artifactSHA256
		item.ArtifactVersion = item.MaterialVersion() + 1
		item.SQLSHA256 = sqlSHA256
		item.RollbackSHA256 = rollbackSHA256
		item.RuleSetVersion = ""
		item.Artifacts = artifacts
		item.SQL = changegate.Redact(sqlText)
		item.RollbackSQL = changegate.Redact(rollbackText)
		item.RollbackPlan = changegate.Redact(rollbackPlan)
		item.ReleasePlan = normalizeReleasePlan(input.ReleasePlan)
		item.Description = changegate.Redact(strings.TrimSpace(input.Description))
		item.PlannedAt = input.PlannedAt
		item.Status = model.StatusDraft
		item.Risk = model.RiskUnknown
		item.Findings = nil
		item.CheckRun = nil
		item.Experiment = nil
		item.Analysis = nil
		item.ReviewerID = ""
		item.ReviewerName = ""
		item.ReviewComment = ""
		item.Timeline = append(item.Timeline, timeline(model.StatusDraft, "更新变更方案", "SQL、配置、Kubernetes 或回滚方案已修改，历史记录保留", actor.Name))
		return nil
	}, audit(actor, id, "UPDATE", "修改变更内容并重置待检查状态"))
	if err == nil {
		_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "变更内容修改，撤销旧通行证"))
		s.publish(updated, "变更方案已更新")
	}
	return updated, err
}

func (s *Service) Submit(id, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || change.SubmitterID != actor.ID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if !trustedForRelease(change) {
		return model.ChangeRequest{}, releaseSourceRefusal(change)
	}
	if change.Status != model.StatusDraft && change.Status != model.StatusCheckFailed {
		return model.ChangeRequest{}, ErrInvalidState
	}
	_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "变更重新提交检查，撤销旧通行证"))
	policies := s.store.PoliciesByOrganization(actor.OrganizationID)
	ruleSetVersion := changegate.RuleSetVersion(policies)
	result := checker.CheckReleaseWithPolicies(checker.ReleaseInput{SQL: change.SQL, RollbackSQL: change.RollbackSQL, RollbackPlan: change.RollbackPlan, Artifacts: change.Artifacts, ReleasePlan: change.ReleasePlan}, checker.Context{Environment: change.Environment, ChangeType: change.ChangeType}, policies)
	windowFindings, windowPolicyCodes := releaseWindowFindings(
		change,
		s.store.ApplicationsByOrganization(actor.OrganizationID),
		s.store.ChangesByOrganization(actor.OrganizationID),
		policies,
	)
	if len(windowFindings) > 0 {
		result.Findings = removeBaselineFindings(result.Findings)
		result.Findings = append(result.Findings, windowFindings...)
		result.MatchedPolicyCodes = appendUniqueStrings(result.MatchedPolicyCodes, windowPolicyCodes...)
		for _, finding := range windowFindings {
			result.Risk = maxRiskLevel(result.Risk, finding.Severity)
		}
	}
	_ = s.store.RecordPolicyHitsForOrganization(actor.OrganizationID, result.MatchedPolicyCodes)
	next := model.StatusReadyForExperiment
	title := "规则检查通过"
	detail := fmt.Sprintf("识别 %d 类变更证据和 %d 条 SQL，命中 %d 项规则证据", len(change.Artifacts), len(result.Statements), len(result.Findings))
	if hasBlockingFinding(result.Findings) {
		next = model.StatusCheckFailed
		title = "规则检查阻断"
		detail = "发现发布窗口冲突、配置泄密、Kubernetes 安全基线、危险 SQL 或缺失回滚等阻断项"
	} else if strings.TrimSpace(change.SQL) == "" {
		next = model.StatusWaitingApproval
		title = "确定性静态门禁通过，等待审批"
		detail = "配置与 Kubernetes 制品已完成真实规则检查；未伪造运行时演练结果"
	}
	blockingCount := 0
	for _, finding := range result.Findings {
		if isBlockingFinding(finding) && finding.Status != model.FindingVerified {
			blockingCount++
		}
	}
	checkStatus := "PASSED"
	if blockingCount > 0 {
		checkStatus = "BLOCKED"
	}
	checkRun := &model.CheckRun{ID: store.NewID("check_"), ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: ruleSetVersion, Status: checkStatus, Findings: len(result.Findings), Blocking: blockingCount, CheckedAt: time.Now()}
	// AI reference analysis after deterministic checks (never decides release).
	analysisInput := change
	analysisInput.Findings = result.Findings
	analysisInput.CheckRun = checkRun
	analysisInput.Risk = result.Risk
	analysisInput.RuleSetVersion = ruleSetVersion
	analysis := s.analyzer.Analyze(context.Background(), analysisInput)
	normalizeAdvisoryRisk(&analysis)
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		if item.Version != change.Version {
			return ErrInvalidState
		}
		item.Findings = result.Findings
		item.CheckRun = checkRun
		item.Risk = result.Risk
		item.RuleSetVersion = ruleSetVersion
		item.Experiment = nil
		item.Analysis = &analysis
		item.Status = next
		item.Timeline = append(item.Timeline, timeline(next, title, detail, actor.Name))
		if analysis.Summary != "" {
			item.Timeline = append(item.Timeline, timeline(next, "生成 AI 参考意见", "模型仅输出参考结论，不参与放行决策；provider="+analysis.Provider, "系统"))
		}
		return nil
	}, auditChangeVersion(audit(actor, id, "SUBMIT_CHECK", detail), change))
	if err == nil {
		s.publish(updated, detail)
	}
	return updated, err
}

func normalizeAdvisoryRisk(analysis *model.AgentAnalysis) {
	if analysis == nil {
		return
	}
	if analysis.AdvisoryRisk == "" {
		analysis.AdvisoryRisk = analysis.Risk
	}
	// Preserve the legacy JSON field while making its advisory semantics explicit.
	analysis.Risk = analysis.AdvisoryRisk
}

func (s *Service) QueueExperiment(id, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusReadyForExperiment {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if strings.TrimSpace(change.SQL) == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：非 SQL 变更已由确定性静态门禁直接进入审批，无需演练", ErrInvalidState)
	}
	if change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, ErrForbidden
	}
	updated, err := s.store.UpdateChangeWithOutbox(id, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		item.Timeline = append(item.Timeline, timeline(model.StatusExperimentQueued, "进入演练队列", "可靠 Worker 将执行影子库变更、回滚与指标采集", actor.Name))
		return nil
	}, model.OutboxEvent{
		OrganizationID: actor.OrganizationID, AggregateType: "change", AggregateID: id,
		EventType: "experiment.requested", Status: model.OutboxPending, MaxAttempts: 5, InputSHA256: change.ArtifactSHA256,
	}, audit(actor, id, "QUEUE_EXPERIMENT", "提交影子库演练任务并写入事务 Outbox"))
	if err != nil {
		return model.ChangeRequest{}, err
	}
	select {
	case s.queue <- id:
	default:
	}
	s.publish(updated, "已进入影子演练队列")
	return updated, nil
}

func (s *Service) AddComment(id, actorID, content string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "view") {
		return model.ChangeRequest{}, ErrForbidden
	}
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：评论内容应为 1 到 1000 个字符", ErrValidation)
	}
	comment := model.ChangeComment{
		ID: store.NewID("cmt_"), AuthorID: actor.ID, AuthorName: actor.Name,
		AuthorRole: actor.Role, Content: content, CreatedAt: time.Now(),
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Comments = append(item.Comments, comment)
		return nil
	}, audit(actor, id, "COMMENT", "添加协作评论"))
	if err == nil {
		s.publish(updated, "变更单新增协作评论")
	}
	return updated, err
}

func (s *Service) Approve(id, actorID, comment string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if !trustedForRelease(change) {
		return model.ChangeRequest{}, releaseSourceRefusal(change)
	}
	if change.Status != model.StatusWaitingApproval {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if actor.ID == change.SubmitterID || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if strings.TrimSpace(change.SQL) != "" {
		if !trustedSQLExperiment(change.Experiment, change) {
			return model.ChangeRequest{}, fmt.Errorf("%w：SQL 变更必须通过真实 PostgreSQL 影子演练", ErrInvalidState)
		}
	} else if change.CheckRun == nil || change.CheckRun.Status != "PASSED" || change.CheckRun.Blocking != 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：静态门禁尚未通过", ErrInvalidState)
	}
	if !allBlockingFindingsVerified(change.Findings) && hasBlockingFinding(change.Findings) {
		return model.ChangeRequest{}, fmt.Errorf("%w：仍有未复核的高风险项", ErrInvalidState)
	}
	if change.Risk == model.RiskHigh && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, fmt.Errorf("%w：高风险变更需要技术负责人审批", ErrForbidden)
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = "证据已核对，同意按计划窗口执行"
	}
	if !validRuneLength(comment, 1, 2000) {
		return model.ChangeRequest{}, fmt.Errorf("%w：审批意见不能超过 2000 个字符", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		if item.Version != change.Version {
			return ErrInvalidState
		}
		item.Status = model.StatusApproved
		item.ReviewerID = actor.ID
		item.ReviewerName = actor.Name
		item.ReviewComment = comment
		item.Timeline = append(item.Timeline, timeline(model.StatusApproved, "审批通过", comment, actor.Name))
		return nil
	}, auditChangeVersion(audit(actor, id, "APPROVE", comment), change))
	if err == nil {
		s.publish(updated, "变更审批通过")
	}
	return updated, err
}

func (s *Service) Reject(id, actorID, comment string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if !trustedForRelease(change) {
		return model.ChangeRequest{}, releaseSourceRefusal(change)
	}
	if change.Status != model.StatusWaitingApproval {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if actor.ID == change.SubmitterID || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	comment = strings.TrimSpace(comment)
	if !validRuneLength(comment, 1, 2000) {
		return model.ChangeRequest{}, fmt.Errorf("%w：驳回原因应为 1 到 2000 个字符", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Status = model.StatusRejected
		item.ReviewerID = actor.ID
		item.ReviewerName = actor.Name
		item.ReviewComment = comment
		item.Timeline = append(item.Timeline, timeline(model.StatusRejected, "审批驳回", comment, actor.Name))
		return nil
	}, audit(actor, id, "REJECT", comment))
	if err == nil {
		_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "审批驳回，撤销旧通行证"))
		s.publish(updated, "变更已驳回")
	}
	return updated, err
}

func (s *Service) Complete(id, actorID string) (model.ChangeRequest, error) {
	return model.ChangeRequest{}, fmt.Errorf("%w：生产变更只能由 CI 消费一次性通行证后自动完成", ErrInvalidState)
}

func timeline(status model.ChangeStatus, title, detail, actor string) model.TimelineEntry {
	return model.TimelineEntry{ID: store.NewID("tl_"), Status: status, Title: title, Detail: detail, Actor: actor, CreatedAt: time.Now()}
}

func audit(actor model.User, changeID, action, detail string) model.AuditEvent {
	actorType, authMethod := "USER", "SESSION"
	if strings.HasPrefix(strings.ToLower(actor.ID), "ci:") {
		actorType, authMethod = "CI", "BEARER_PASSPORT"
	} else if strings.HasPrefix(strings.ToLower(actor.ID), "system") || strings.HasPrefix(strings.ToLower(actor.ID), "integration") {
		actorType, authMethod = "SYSTEM", "INTERNAL"
	}
	return model.AuditEvent{
		OrganizationID: actor.OrganizationID, ID: store.NewID("audit_"), ChangeID: changeID,
		ActorID: actor.ID, ActorName: actor.Name, ActorType: actorType, AuthMethod: authMethod,
		Action: action, ResourceType: "CHANGE", ResourceID: changeID, Result: "SUCCESS",
		Detail: detail, CreatedAt: time.Now().UTC(),
	}
}

func auditChangeVersion(event model.AuditEvent, before model.ChangeRequest) model.AuditEvent {
	event.ResourceVersionBefore = before.Version
	event.ResourceVersionAfter = before.Version + 1
	event.RequestDigest = before.ArtifactSHA256
	return event
}

func auditPassport(event model.AuditEvent, passportID string) model.AuditEvent {
	event.PassportID = passportID
	return event
}

func auditExperiment(event model.AuditEvent, relatedEventID, attemptID string) model.AuditEvent {
	event.RelatedEventID = relatedEventID
	event.AttemptID = attemptID
	return event
}

func normalizeArtifacts(items []model.ChangeArtifact, sqlText string) ([]model.ChangeArtifact, error) {
	allowed := map[model.ArtifactKind]bool{
		model.ArtifactConfig: true, model.ArtifactKubernetes: true, model.ArtifactDatabase: true,
	}
	result := make([]model.ChangeArtifact, 0, len(items)+1)
	hasDatabase := false
	for index, item := range items {
		if !allowed[item.Kind] {
			return nil, fmt.Errorf("%w：第 %d 个制品类型 %q 不在 v1 支持范围（仅支持 DATABASE、CONFIG、KUBERNETES）", ErrValidation, index+1, item.Kind)
		}
		if strings.TrimSpace(item.Content) == "" {
			return nil, fmt.Errorf("%w：第 %d 个制品内容不能为空", ErrValidation, index+1)
		}
		if item.ID == "" {
			item.ID = store.NewID("artifact_")
		}
		item.Name = defaultString(item.Name, string(item.Kind)+" 变更证据")
		item = changegate.PrepareArtifact(item)
		if item.Kind == model.ArtifactDatabase {
			hasDatabase = true
		}
		result = append(result, item)
	}
	if strings.TrimSpace(sqlText) != "" && !hasDatabase {
		item := model.ChangeArtifact{ID: store.NewID("artifact_"), Kind: model.ArtifactDatabase, Name: "数据库 SQL", Source: "变更单", Language: "SQL", Content: sqlText}
		result = append(result, changegate.PrepareArtifact(item))
	}
	return result, nil
}

func deriveChangeType(items []model.ChangeArtifact) string {
	kinds := make(map[model.ArtifactKind]struct{}, 3)
	for _, item := range items {
		kinds[item.Kind] = struct{}{}
	}
	if len(kinds) > 1 {
		return "联合变更"
	}
	if _, ok := kinds[model.ArtifactDatabase]; ok {
		return "数据库变更"
	}
	if _, ok := kinds[model.ArtifactConfig]; ok {
		return "配置变更"
	}
	if _, ok := kinds[model.ArtifactKubernetes]; ok {
		return "Kubernetes 变更"
	}
	return "生产变更"
}

func artifactBytes(items []model.ChangeArtifact) int {
	total := 0
	for _, item := range items {
		total += len(item.Name) + len(item.Source) + len(item.Language) + len(item.Content)
	}
	return total
}

func hasArtifactKind(items []model.ChangeArtifact, kind model.ArtifactKind) bool {
	for _, item := range items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func normalizeReleasePlan(plan model.ReleasePlan) model.ReleasePlan {
	plan.Strategy = defaultString(plan.Strategy, "全量发布")
	if plan.CanaryPercent < 0 {
		plan.CanaryPercent = 0
	}
	if plan.CanaryPercent > 100 {
		plan.CanaryPercent = 100
	}
	if plan.ObservationMinutes <= 0 {
		plan.ObservationMinutes = 15
	}
	if plan.ObservationMinutes > 1440 {
		plan.ObservationMinutes = 1440
	}
	plan.SuccessMetrics = normalizeStringList(plan.SuccessMetrics)
	return plan
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}
