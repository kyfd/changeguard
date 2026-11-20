package service

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/checker"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func (s *Service) CreatePolicy(input model.SaveRiskPolicyInput, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	input.Code = strings.ToUpper(strings.TrimSpace(input.Code))
	if err := validatePolicyInput(input, false); err != nil {
		return model.RiskPolicy{}, err
	}
	if _, err := s.store.PolicyByCodeForOrganization(actor.OrganizationID, input.Code); err == nil {
		return model.RiskPolicy{}, fmt.Errorf("%w：规则编码已存在", ErrValidation)
	}
	now := time.Now()
	policy := model.RiskPolicy{
		OrganizationID: actor.OrganizationID, ID: store.NewID("pol_"), Code: input.Code, Name: strings.TrimSpace(input.Name),
		Description: strings.TrimSpace(input.Description), Pattern: strings.TrimSpace(input.Pattern),
		Severity: input.Severity, Blocking: input.Blocking, Enabled: input.Enabled, Builtin: false,
		Environments: normalizeStringList(input.Environments), ChangeTypes: normalizeStringList(input.ChangeTypes),
		ArtifactKinds: normalizeStringList(input.ArtifactKinds),
		Suggestion:    strings.TrimSpace(input.Suggestion), Version: 1, CreatedAt: now, UpdatedAt: now, UpdatedBy: actor.Name,
	}
	detail := fmt.Sprintf("创建自定义风险规则 %s（%s）", policy.Name, policy.Code)
	if err := s.store.CreatePolicy(policy, audit(actor, "", "CREATE_POLICY", detail)); err != nil {
		return model.RiskPolicy{}, err
	}
	return policy, nil
}

func (s *Service) UpdatePolicy(id string, input model.SaveRiskPolicyInput, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	current, err := s.store.PolicyForOrganization(actor.OrganizationID, id)
	if err != nil {
		return model.RiskPolicy{}, err
	}
	input.Code = current.Code
	if current.Builtin {
		input.Pattern = current.Pattern
	}
	if err := validatePolicyInput(input, current.Builtin); err != nil {
		return model.RiskPolicy{}, err
	}
	detail := fmt.Sprintf("更新风险规则 %s（%s）", strings.TrimSpace(input.Name), current.Code)
	return s.store.UpdatePolicy(id, func(policy *model.RiskPolicy) error {
		policy.Name = strings.TrimSpace(input.Name)
		policy.Description = strings.TrimSpace(input.Description)
		policy.Pattern = strings.TrimSpace(input.Pattern)
		policy.Severity = input.Severity
		policy.Blocking = input.Blocking
		policy.Enabled = input.Enabled
		policy.Environments = normalizeStringList(input.Environments)
		policy.ChangeTypes = normalizeStringList(input.ChangeTypes)
		policy.ArtifactKinds = normalizeStringList(input.ArtifactKinds)
		policy.Suggestion = strings.TrimSpace(input.Suggestion)
		policy.UpdatedBy = actor.Name
		return nil
	}, audit(actor, "", "UPDATE_POLICY", detail))
}

func (s *Service) TogglePolicy(id, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	current, err := s.store.PolicyForOrganization(actor.OrganizationID, id)
	if err != nil {
		return model.RiskPolicy{}, err
	}
	action := "启用"
	if current.Enabled {
		action = "停用"
	}
	detail := fmt.Sprintf("%s风险规则 %s（%s）", action, current.Name, current.Code)
	return s.store.UpdatePolicy(id, func(policy *model.RiskPolicy) error {
		policy.Enabled = !policy.Enabled
		policy.UpdatedBy = actor.Name
		return nil
	}, audit(actor, "", "TOGGLE_POLICY", detail))
}

func (s *Service) TestPolicies(input model.TestRiskPolicyInput, actorID string) (checker.Result, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return checker.Result{}, ErrForbidden
	}
	if strings.TrimSpace(input.SQL) == "" && strings.TrimSpace(input.Content) == "" {
		return checker.Result{}, fmt.Errorf("%w：请输入需要试跑的 SQL 或变更制品内容", ErrValidation)
	}
	if strings.TrimSpace(input.Content) != "" {
		kind := model.ArtifactKind(strings.ToUpper(strings.TrimSpace(input.ArtifactKind)))
		result := checker.CheckReleaseWithPolicies(checker.ReleaseInput{
			Artifacts:    []model.ChangeArtifact{{ID: "artifact_test", Kind: kind, Name: "规则试跑制品", Content: input.Content}},
			RollbackPlan: "规则试跑使用的模拟回滚方案",
			ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, AutoRollback: true, SuccessMetrics: []string{"错误率", "P99 延迟"}},
		}, checker.Context{Environment: defaultString(input.Environment, "生产环境"), ChangeType: defaultString(input.ChangeType, "混合发布")}, s.store.PoliciesByOrganization(actor.OrganizationID))
		return result, nil
	}
	result := checker.CheckWithPolicies(
		input.SQL,
		input.RollbackSQL,
		checker.Context{Environment: defaultString(input.Environment, "生产环境"), ChangeType: defaultString(input.ChangeType, "DDL")},
		s.store.PoliciesByOrganization(actor.OrganizationID),
	)
	return result, nil
}

func validatePolicyInput(input model.SaveRiskPolicyInput, builtin bool) error {
	codePattern := regexp.MustCompile("^[A-Z][A-Z0-9_]{2,63}$")
	if !codePattern.MatchString(input.Code) {
		return fmt.Errorf("%w：规则编码只能包含大写字母、数字和下划线", ErrValidation)
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Description), 1, 1000) ||
		!validRuneLength(strings.TrimSpace(input.Suggestion), 1, 1000) ||
		!validRuneLength(strings.TrimSpace(input.Pattern), 0, 500) {
		return fmt.Errorf("%w：规则名称、说明、表达式或整改建议长度不符合要求", ErrValidation)
	}
	if input.Severity != model.RiskLow && input.Severity != model.RiskMedium && input.Severity != model.RiskHigh {
		return fmt.Errorf("%w：风险等级不正确", ErrValidation)
	}
	if !builtin && strings.TrimSpace(input.Pattern) == "" {
		return fmt.Errorf("%w：自定义规则必须填写正则表达式", ErrValidation)
	}
	if strings.TrimSpace(input.Pattern) != "" {
		if _, err := regexp.Compile(input.Pattern); err != nil {
			return fmt.Errorf("%w：正则表达式无效：%v", ErrValidation, err)
		}
	}
	return nil
}

func normalizeStringList(items []string) []string {
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (s *Service) AssignFinding(changeID, findingID, actorID string, input model.AssignFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	owner, err := s.store.User(input.OwnerID)
	if err != nil || !owner.Active {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险负责人不存在或已停用", ErrValidation)
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || owner.OrganizationID != actor.OrganizationID ||
		!s.canUseApplication(actor, change.ApplicationID, "review") ||
		!s.canUseApplication(owner, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if findingIndex(change.Findings, findingID) < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	now := time.Now()
	if input.DueAt.IsZero() {
		input.DueAt = now.Add(72 * time.Hour)
	}
	if !input.DueAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改期限必须晚于当前时间", ErrValidation)
	}
	detail := fmt.Sprintf("风险项 %s 已派给 %s，要求于 %s 前完成整改", findingID, owner.Name, input.DueAt.Format("2006-01-02 15:04"))
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		index := findingIndex(item.Findings, findingID)
		if index < 0 {
			return ErrValidation
		}
		finding := &item.Findings[index]
		finding.Status = model.FindingAssigned
		finding.OwnerID = owner.ID
		finding.OwnerName = owner.Name
		finding.DueAt = &input.DueAt
		finding.UpdatedAt = now
		item.Timeline = append(item.Timeline, timeline(item.Status, "风险整改派单", detail, actor.Name))
		return nil
	}, audit(actor, changeID, "ASSIGN_FINDING", detail))
	if err == nil {
		s.publish(updated, "风险项已派单")
	}
	return updated, err
}

func (s *Service) ResolveFinding(changeID, findingID, actorID string, input model.ResolveFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	index := findingIndex(change.Findings, findingID)
	if index < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	finding := change.Findings[index]
	if finding.OwnerID == "" {
		if change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
			return model.ChangeRequest{}, ErrForbidden
		}
	} else if finding.OwnerID != actor.ID && change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, ErrForbidden
	}
	if finding.Status == model.FindingVerified {
		return model.ChangeRequest{}, ErrInvalidState
	}
	resolution := strings.TrimSpace(input.Resolution)
	if len([]rune(resolution)) < 5 || len([]rune(resolution)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改说明应为 5 到 1000 个字符", ErrValidation)
	}
	now := time.Now()
	detail := fmt.Sprintf("风险项 %s 已提交整改，等待审核人复核", findingID)
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		idx := findingIndex(item.Findings, findingID)
		if idx < 0 {
			return ErrValidation
		}
		target := &item.Findings[idx]
		target.Status = model.FindingResolved
		if target.OwnerID == "" {
			target.OwnerID, target.OwnerName = actor.ID, actor.Name
		}
		target.Resolution = resolution
		target.ResolvedAt = &now
		target.VerifiedAt = nil
		target.VerifiedByID, target.VerifiedByName, target.VerificationComment = "", "", ""
		target.UpdatedAt = now
		item.Timeline = append(item.Timeline, timeline(item.Status, "提交风险整改", detail, actor.Name))
		return nil
	}, audit(actor, changeID, "RESOLVE_FINDING", detail))
	if err == nil {
		s.publish(updated, "风险项已提交整改")
	}
	return updated, err
}

func (s *Service) VerifyFinding(changeID, findingID, actorID string, input model.VerifyFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	index := findingIndex(change.Findings, findingID)
	if index < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	finding := change.Findings[index]
	if finding.Status != model.FindingResolved {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if finding.OwnerID == actor.ID {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改人与复核人必须分离", ErrForbidden)
	}
	comment := strings.TrimSpace(input.Comment)
	if len([]rune(comment)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：复核意见不能超过 1000 个字符", ErrValidation)
	}
	if comment == "" {
		if input.Approved {
			comment = "整改证据已核对，复核通过"
		} else {
			return model.ChangeRequest{}, fmt.Errorf("%w：退回整改时必须填写原因", ErrValidation)
		}
	}
	now := time.Now()
	actionTitle := "风险复核通过"
	actionCode := "VERIFY_FINDING"
	if !input.Approved {
		actionTitle, actionCode = "风险整改退回", "REJECT_FINDING"
	}
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		idx := findingIndex(item.Findings, findingID)
		if idx < 0 {
			return ErrValidation
		}
		target := &item.Findings[idx]
		target.VerifiedByID, target.VerifiedByName = actor.ID, actor.Name
		target.VerificationComment = comment
		target.UpdatedAt = now
		if input.Approved {
			target.Status = model.FindingVerified
			target.VerifiedAt = &now
		} else {
			target.Status = model.FindingAssigned
			target.VerifiedAt = nil
		}
		item.Timeline = append(item.Timeline, timeline(item.Status, actionTitle, comment, actor.Name))
		if input.Approved && item.Status == model.StatusCheckFailed && allBlockingFindingsVerified(item.Findings) {
			if item.CheckRun == nil {
				item.CheckRun = &model.CheckRun{ID: store.NewID("check_"), ArtifactSHA256: item.ArtifactSHA256, RuleSetVersion: item.RuleSetVersion}
			}
			item.CheckRun.ArtifactSHA256 = item.ArtifactSHA256
			item.CheckRun.RuleSetVersion = item.RuleSetVersion
			item.CheckRun.Status = "PASSED"
			item.CheckRun.Blocking = 0
			item.CheckRun.Findings = len(item.Findings)
			item.CheckRun.CheckedAt = now
			if strings.TrimSpace(item.SQL) != "" {
				item.Status = model.StatusReadyForExperiment
				item.Timeline = append(item.Timeline, timeline(model.StatusReadyForExperiment, "阻断风险已闭环", "所有阻断风险项均完成整改与独立复核，可以进入 PostgreSQL 影子演练", actor.Name))
			} else {
				item.Status = model.StatusWaitingApproval
				item.Timeline = append(item.Timeline, timeline(model.StatusWaitingApproval, "阻断风险已闭环", "所有阻断风险项均完成整改与独立复核，静态门禁通过，等待独立审批", actor.Name))
			}
		}
		return nil
	}, audit(actor, changeID, actionCode, comment))
	if err == nil {
		s.publish(updated, actionTitle)
	}
	return updated, err
}

func isBlockingFinding(item model.Finding) bool {
	legacyBlocking := map[string]bool{
		"EMPTY_SQL": true, "MISSING_ROLLBACK": true, "UPDATE_WITHOUT_WHERE": true,
		"DELETE_WITHOUT_WHERE": true, "DROP_TABLE": true, "TRUNCATE": true,
		"ADD_NOT_NULL_WITHOUT_DEFAULT": true,
		"FK_WITHOUT_NOT_VALID":         true, "HEAVY_DDL_REWRITE": true,
	}
	return item.Blocking || legacyBlocking[item.Code]
}

func hasBlockingFinding(findings []model.Finding) bool {
	for _, item := range findings {
		if isBlockingFinding(item) && item.Status != model.FindingVerified {
			return true
		}
	}
	return false
}

func findingIndex(findings []model.Finding, id string) int {
	for index := range findings {
		if findings[index].ID == id {
			return index
		}
	}
	return -1
}

func allBlockingFindingsVerified(findings []model.Finding) bool {
	hasBlocking := false
	for _, finding := range findings {
		if !isBlockingFinding(finding) {
			continue
		}
		hasBlocking = true
		if finding.Status != model.FindingVerified {
			return false
		}
	}
	return hasBlocking
}

func maxRiskLevel(left, right model.RiskLevel) model.RiskLevel {
	rank := map[model.RiskLevel]int{
		model.RiskUnknown: 0, model.RiskLow: 1, model.RiskMedium: 2, model.RiskHigh: 3,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

const releaseWindowProtection = 90 * time.Minute

func releaseWindowFindings(current model.ChangeRequest, applications []model.Application, changes []model.ChangeRequest, policies []model.RiskPolicy) ([]model.Finding, []string) {
	if current.PlannedAt.IsZero() {
		return nil, nil
	}
	policyByCode := make(map[string]model.RiskPolicy, 2)
	for _, policy := range policies {
		if policy.Enabled && (policy.Code == "CHANGE_WINDOW_CONFLICT" || policy.Code == "DEPENDENCY_WINDOW_OVERLAP") {
			policyByCode[policy.Code] = policy
		}
	}
	if len(policyByCode) == 0 {
		return nil, nil
	}

	applicationByID := make(map[string]model.Application, len(applications))
	for _, application := range applications {
		applicationByID[application.ID] = application
	}
	currentApplication, ok := applicationByID[current.ApplicationID]
	if !ok {
		return nil, nil
	}
	upstream := make(map[string]bool, len(currentApplication.Dependencies))
	for _, dependencyID := range currentApplication.Dependencies {
		upstream[dependencyID] = true
	}
	downstream := make(map[string]bool)
	for _, application := range applications {
		for _, dependencyID := range application.Dependencies {
			if dependencyID == current.ApplicationID {
				downstream[application.ID] = true
				break
			}
		}
	}

	now := time.Now()
	findings := make([]model.Finding, 0)
	policyCodes := make([]string, 0, 2)
	for _, other := range changes {
		if other.ID == current.ID || other.OrganizationID != current.OrganizationID || other.PlannedAt.IsZero() ||
			other.Status == model.StatusRejected || other.Status == model.StatusCompleted {
			continue
		}
		delta := current.PlannedAt.Sub(other.PlannedAt)
		if delta < 0 {
			delta = -delta
		}
		if delta > releaseWindowProtection {
			continue
		}

		code := ""
		relation := ""
		if other.ApplicationID == current.ApplicationID {
			code = "CHANGE_WINDOW_CONFLICT"
			relation = "同一服务"
		} else if upstream[other.ApplicationID] {
			code = "DEPENDENCY_WINDOW_OVERLAP"
			relation = "直接上游"
		} else if downstream[other.ApplicationID] {
			code = "DEPENDENCY_WINDOW_OVERLAP"
			relation = "直接下游"
		}
		policy, enabled := policyByCode[code]
		if code == "" || !enabled {
			continue
		}
		otherApplication := applicationByID[other.ApplicationID]
		otherName := other.ApplicationName
		if otherName == "" {
			otherName = otherApplication.Name
		}
		detail := fmt.Sprintf("%s“%s”在 90 分钟保护窗口内已有变更“%s”（%s）。", relation, otherName, other.Title, other.Status)
		evidence := fmt.Sprintf("change_id=%s; planned_at=%s; interval=%s", other.ID, other.PlannedAt.Format(time.RFC3339), formatWindowInterval(delta))
		findings = append(findings, model.Finding{
			ID:          "window_" + strings.ToLower(policy.Code) + "_" + other.ID,
			Code:        policy.Code,
			Severity:    policy.Severity,
			Title:       policy.Name,
			Detail:      detail,
			Evidence:    evidence,
			Suggestion:  policy.Suggestion,
			Blocking:    policy.Blocking,
			RuleVersion: policy.Version,
			Status:      model.FindingOpen,
			UpdatedAt:   now,
		})
		policyCodes = appendUniqueStrings(policyCodes, policy.Code)
	}
	return findings, policyCodes
}

func formatWindowInterval(delta time.Duration) string {
	minutes := int(delta.Round(time.Minute) / time.Minute)
	if minutes < 1 {
		return "不足 1 分钟"
	}
	return fmt.Sprintf("%d 分钟", minutes)
}

func removeBaselineFindings(findings []model.Finding) []model.Finding {
	filtered := findings[:0]
	for _, finding := range findings {
		if finding.Code == "BASELINE_PASS" || finding.Code == "RELEASE_BASELINE_PASS" {
			continue
		}
		filtered = append(filtered, finding)
	}
	return filtered
}

func appendUniqueStrings(items []string, values ...string) []string {
	seen := make(map[string]bool, len(items)+len(values))
	result := make([]string, 0, len(items)+len(values))
	for _, item := range append(items, values...) {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}
