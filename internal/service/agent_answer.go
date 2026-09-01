package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

// assistantAnswer is the deterministic, evidence-carrying reply produced by the
// change-scoped assistant. Tools in Trace are read-only queries that were
// actually executed for the detected intent; unsupported questions execute no
// evidence tools and safely fall back to guidance.
type assistantAnswer struct {
	Intent    model.AgentQuestionIntent
	Answer    string
	Citations []model.AgentCitation
	Trace     []model.AgentToolTrace
	Proposals []model.AgentActionProposal
}

// buildAssistantAnswer routes a question to the minimum read-only evidence set
// needed to answer it. Data and trace always come from the same registry
// execution; failed queries remain visible and never produce citations.
func buildAssistantAnswer(ctx context.Context, change model.ChangeRequest, registry *EvidenceQueryRegistry, question string) assistantAnswer {
	intent := detectAssistantIntent(question)
	answer := assistantAnswer{Intent: intent}
	query := EvidenceQuery{Change: change, OrganizationID: change.OrganizationID}

	switch intent {
	case model.AgentIntentBlockingReason:
		query.Input = "filter=unresolved_blocking"
		result := registry.Execute(ctx, evidenceToolFindings, query)
		answer.Trace = append(answer.Trace, result.Trace)
		findings, ok := evidenceFindings(result)
		if !ok {
			answer.Answer = evidenceQueryFailureAnswer(result.Trace)
			break
		}
		answer.Citations = findingCitations(findings.Blocking)
		answer.Answer, answer.Proposals = composeBlockingAnswer(change, findings.Blocking)
	case model.AgentIntentNextStep:
		query.Input = "filter=unresolved"
		findingResult := registry.Execute(ctx, evidenceToolFindings, query)
		query.Input = "change=" + change.ID
		experimentResult := registry.Execute(ctx, evidenceToolExperiment, query)
		answer.Trace = append(answer.Trace, findingResult.Trace, experimentResult.Trace)
		findings, findingsOK := evidenceFindings(findingResult)
		experiment, experimentOK := evidenceExperiment(experimentResult)
		if !findingsOK || !experimentOK {
			answer.Answer = evidenceQueryFailuresAnswer(findingResult, experimentResult)
			break
		}
		answer.Citations = append(answer.Citations, changeCitation(change))
		answer.Citations = append(answer.Citations, findingCitations(append(findings.Blocking, findings.Open...))...)
		if experiment != nil {
			answer.Citations = append(answer.Citations, experimentCitation(experiment))
		}
		answer.Answer, answer.Proposals = composeNextStepAnswer(change, findings.Blocking, findings.Open, experiment)
	case model.AgentIntentFindingRemediation:
		query.Input = "filter=unresolved"
		result := registry.Execute(ctx, evidenceToolFindings, query)
		answer.Trace = append(answer.Trace, result.Trace)
		findings, ok := evidenceFindings(result)
		if !ok {
			answer.Answer = evidenceQueryFailureAnswer(result.Trace)
			break
		}
		unresolved := append(append([]model.Finding{}, findings.Blocking...), findings.Open...)
		answer.Citations = findingCitations(unresolved)
		answer.Answer, answer.Proposals = composeRemediationAnswer(unresolved)
	case model.AgentIntentPassportGate:
		query.Input = "status=all"
		passportResult := registry.Execute(ctx, evidenceToolPassports, query)
		query.Input = "change=" + change.ID
		experimentResult := registry.Execute(ctx, evidenceToolExperiment, query)
		answer.Trace = append(answer.Trace, passportResult.Trace, experimentResult.Trace)
		passports, passportsOK := evidencePassports(passportResult)
		experiment, experimentOK := evidenceExperiment(experimentResult)
		if !passportsOK || !experimentOK {
			answer.Answer = evidenceQueryFailuresAnswer(passportResult, experimentResult)
			break
		}
		answer.Citations = append(answer.Citations, changeCitation(change))
		for _, passport := range passports {
			answer.Citations = append(answer.Citations, passportCitation(passport))
		}
		if experiment != nil {
			answer.Citations = append(answer.Citations, experimentCitation(experiment))
		}
		answer.Answer = composePassportGateAnswer(change, passports, experiment)
	default:
		answer.Answer = "我无法确定你要查询哪类变更证据，因此没有执行任何工具查询。你可以问：为什么被阻断、下一步做什么、finding 如何整改，或 passport/CI Gate 状态。助手只读取证据，不会执行审批、签发护照、消费门禁、发布或其他写操作。"
	}
	return answer
}

func evidenceFindings(result EvidenceQueryResult) (findingEvidence, bool) {
	if result.Trace.Error != "" {
		return findingEvidence{}, false
	}
	findings, ok := result.Data.(findingEvidence)
	return findings, ok
}

func evidenceExperiment(result EvidenceQueryResult) (*model.ExperimentReport, bool) {
	if result.Trace.Error != "" {
		return nil, false
	}
	experiment, ok := result.Data.(*model.ExperimentReport)
	return experiment, ok
}

func evidencePassports(result EvidenceQueryResult) ([]model.Passport, bool) {
	if result.Trace.Error != "" {
		return nil, false
	}
	passports, ok := result.Data.([]model.Passport)
	return passports, ok
}

func evidenceQueryFailureAnswer(trace model.AgentToolTrace) string {
	return fmt.Sprintf("证据查询失败（%s）：%s。无法基于缺失证据给出通过结论；助手不会执行任何审批、门禁或发布写操作。", trace.Tool, trace.Error)
}

func evidenceQueryFailuresAnswer(results ...EvidenceQueryResult) string {
	failures := make([]string, 0, len(results))
	for _, result := range results {
		if result.Trace.Error != "" {
			failures = append(failures, result.Trace.Tool+"："+result.Trace.Error)
		}
	}
	if len(failures) == 0 {
		failures = append(failures, "工具返回了无效证据")
	}
	return "证据查询失败（" + strings.Join(failures, "；") + "）。无法基于缺失证据给出通过结论；助手不会执行任何审批、门禁或发布写操作。"
}

func detectAssistantIntent(question string) model.AgentQuestionIntent {
	normalized := strings.ToLower(strings.TrimSpace(question))
	containsAny := func(values ...string) bool {
		for _, value := range values {
			if strings.Contains(normalized, value) {
				return true
			}
		}
		return false
	}

	// Specific evidence domains take precedence over generic words such as
	// "为什么" and "下一步".
	switch {
	case containsAny("passport", "ci gate", "ci-gate", "gate", "护照", "门禁", "通行证"):
		return model.AgentIntentPassportGate
	case containsAny("finding", "发现项", "整改", "怎么修", "如何修", "修复", "remediat"):
		return model.AgentIntentFindingRemediation
	case containsAny("为什么不能", "为何不能", "为什么卡", "为何卡", "阻断原因", "被阻断", "卡住", "不能审批", "不能放行"):
		return model.AgentIntentBlockingReason
	case containsAny("下一步", "接下来", "然后呢", "该做什么", "怎么推进", "如何推进"):
		return model.AgentIntentNextStep
	default:
		return model.AgentIntentUnknown
	}
}

func findingCitations(findings []model.Finding) []model.AgentCitation {
	citations := make([]model.AgentCitation, 0, len(findings))
	for _, finding := range findings {
		citations = append(citations, model.AgentCitation{
			Kind: "rule_finding", ID: finding.ID, Title: finding.Title,
			Summary: fmt.Sprintf("%s · %s · %s", finding.Code, finding.Severity, finding.Status),
		})
	}
	return citations
}

func changeCitation(change model.ChangeRequest) model.AgentCitation {
	return model.AgentCitation{Kind: "change", ID: change.ID, Title: change.Title, Summary: fmt.Sprintf("%s · %s", change.Status, change.Risk)}
}

func experimentCitation(experiment *model.ExperimentReport) model.AgentCitation {
	return model.AgentCitation{Kind: "experiment", ID: experiment.ID, Title: "预发布演练", Summary: fmt.Sprintf("%s · %s", experiment.Mode, experiment.Status)}
}

func passportCitation(passport model.Passport) model.AgentCitation {
	return model.AgentCitation{Kind: "passport", ID: passport.ID, Title: "发布门禁护照", Summary: fmt.Sprintf("%s · 签发 %s", passport.Status, formatTime(passport.IssuedAt))}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format("01-02 15:04")
}

func composeBlockingAnswer(change model.ChangeRequest, blocking []model.Finding) (string, []model.AgentActionProposal) {
	if len(blocking) == 0 {
		return "未查询到未解决的阻断 finding。当前状态为「" + string(change.Status) + "」；这不等于已通过审批或 CI Gate，仍需按状态完成后续治理步骤。", nil
	}
	lines := []string{fmt.Sprintf("当前有 %d 个未解决的阻断 finding：", len(blocking))}
	for _, finding := range blocking {
		lines = append(lines, fmt.Sprintf("- %s（%s）：%s", finding.Title, finding.Code, finding.Suggestion))
	}
	lines = append(lines, "这些 finding 完成整改并经独立人员复核前，不能放行，也不能描述为已放行。")
	return strings.Join(lines, "\n"), assistantProposals([]model.ActionProposalType{model.ProposalRemediate})
}

func composeRemediationAnswer(findings []model.Finding) (string, []model.AgentActionProposal) {
	if len(findings) == 0 {
		return "未查询到 OPEN 或 ASSIGNED 的 finding。已解决或已验证的 finding 不会被重新描述为待整改项。", nil
	}
	lines := []string{fmt.Sprintf("以下 %d 个 finding 仍需整改：", len(findings))}
	for _, finding := range findings {
		lines = append(lines, fmt.Sprintf("- %s（%s）：%s", finding.Title, finding.Code, finding.Suggestion))
	}
	if hints := transactionRemediationHints(findings); len(hints) > 0 {
		lines = append(lines, "事务优化建议：")
		lines = append(lines, hints...)
	}
	lines = append(lines, "整改后应提交给独立人员复核；助手不会代为修改、指派或验证 finding。")
	return strings.Join(lines, "\n"), assistantProposals([]model.ActionProposalType{model.ProposalRemediate})
}

func composeNextStepAnswer(change model.ChangeRequest, blocking, open []model.Finding, experiment *model.ExperimentReport) (string, []model.AgentActionProposal) {
	if len(blocking) > 0 {
		return fmt.Sprintf("下一步：先整改 %d 个阻断 finding，再由独立人员复核；当前不能放行。", len(blocking)), assistantProposals([]model.ActionProposalType{model.ProposalRemediate})
	}
	if len(open) > 0 {
		return fmt.Sprintf("下一步：处理 %d 个非阻断 finding，并在审批前确认没有新增阻断。", len(open)), assistantProposals([]model.ActionProposalType{model.ProposalRemediate, model.ProposalReview})
	}
	if experimentNotReleaseEvidence(experiment) {
		return "下一步：执行真实 PostgreSQL 影子演练并验证回滚。NOT_RUN 或 DEMO_ONLY 仅表示未取得真实放行证据，不能描述为演练通过。", assistantProposals([]model.ActionProposalType{model.ProposalRerunExperiment})
	}
	if experiment == nil {
		return "下一步：执行预发布演练并验证回滚；当前没有演练报告，不能描述为验证通过。", assistantProposals([]model.ActionProposalType{model.ProposalRerunExperiment})
	}
	if !strings.EqualFold(experiment.Status, "PASSED") || !experiment.RollbackVerified {
		return "下一步：修复演练失败项并重新执行真实演练，确认状态为 PASSED 且回滚已验证。", assistantProposals([]model.ActionProposalType{model.ProposalRerunExperiment})
	}
	if change.Status == model.StatusWaitingApproval {
		return "下一步：由具备权限的独立 reviewer 人工审核。助手不会自动审批。", assistantProposals([]model.ActionProposalType{model.ProposalReview})
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return "下一步：按发布流程核对有效 passport/CI Gate，并观察发布后结果；审批通过不等于 CI Gate 已消费成功。", assistantProposals([]model.ActionProposalType{model.ProposalObserve})
	}
	return "下一步：按当前状态「" + string(change.Status) + "」推进治理流程；尚不能描述为审批或门禁已通过。", nil
}

func composePassportGateAnswer(change model.ChangeRequest, passports []model.Passport, experiment *model.ExperimentReport) string {
	if experimentNotReleaseEvidence(experiment) {
		return "CI Gate 当前没有可依赖的真实演练证据：演练为 NOT_RUN 或 DEMO_ONLY，不能描述为通过，也不应据此签发或消费 passport。助手只查询状态，不会签发或消费通行证。"
	}
	if change.Status != model.StatusApproved && change.Status != model.StatusCompleted {
		return "变更当前状态为「" + string(change.Status) + "」，尚不能视为已通过 CI Gate。passport 必须由授权流程签发；助手不会自动审批、签发或消费。"
	}
	for _, passport := range passports {
		if passport.Status == model.PassportActive && time.Now().UTC().Before(passport.ExpiresAt) {
			return "查询到有效期内的 ACTIVE passport（" + passport.ID + "）。这仅表示存在可供 CI 校验的一次性凭据，不表示 CI Gate 已通过或已消费；助手不会执行门禁消费。"
		}
	}
	if len(passports) == 0 {
		return "变更已审批，但没有查询到 passport，因此不能描述为 CI Gate 已通过。请由授权人员按正式流程签发；助手不会代为签发。"
	}
	return "变更已审批，但查询到的 passport 均非有效 ACTIVE 状态，因此不能描述为 CI Gate 已通过。请由授权人员检查过期、撤销或已消费状态。"
}

func experimentNotReleaseEvidence(experiment *model.ExperimentReport) bool {
	return experiment != nil && (strings.EqualFold(experiment.Mode, "DEMO_ONLY") || strings.EqualFold(experiment.Status, "NOT_RUN"))
}

// transactionRemediationHints maps transaction-optimization finding codes to
// concrete, production-oriented next steps.
func transactionRemediationHints(findings []model.Finding) []string {
	seen := make(map[string]bool)
	hints := make([]string, 0, 4)
	add := func(code, hint string) {
		if seen[code] {
			return
		}
		for _, finding := range findings {
			if finding.Code == code {
				seen[code] = true
				hints = append(hints, "  - "+hint)
				return
			}
		}
	}
	add("UNBATCHED_LARGE_DML", "按主键/时间窗口分批 UPDATE/DELETE，单批限制行数并提交，避免大事务")
	add("FK_WITHOUT_NOT_VALID", "外键改为 ADD ... NOT VALID，低峰期再 VALIDATE CONSTRAINT")
	add("INDEX_NOT_CONCURRENT", "索引创建改为 CREATE INDEX CONCURRENTLY，并观察锁等待")
	add("DDL_LOCK_IMPACT", "DDL 放到维护窗口，或改用在线/并发语法并设置 lock_timeout")
	add("HEAVY_DDL_REWRITE", "避免 VACUUM FULL/CLUSTER/REINDEX 直接上生产，改用在线重建或分窗口")
	add("MISSING_LOCK_TIMEOUT", "在执行脚本声明 SET lock_timeout / statement_timeout，冲突时快速失败")
	add("SELECT_FOR_UPDATE_UNBOUNDED", "缩小 FOR UPDATE 范围，补充 LIMIT 或主键条件，缩短事务内逻辑")
	add("MIXED_DDL_DML_TRANSACTION", "将结构变更与大批量数据回填拆成独立变更单分别演练")
	add("TOO_MANY_STATEMENTS", "按业务目标与回滚边界拆分变更单，降低单事务失败面")
	return hints
}

func assistantProposals(types []model.ActionProposalType) []model.AgentActionProposal {
	labels := map[model.ActionProposalType]string{
		model.ProposalRemediate:       "整改并提交复核",
		model.ProposalRerunExperiment: "重新执行真实演练",
		model.ProposalReassign:        "重新指派处理人",
		model.ProposalReview:          "提交人工审批",
		model.ProposalObserve:         "观察发布后指标",
	}
	result := make([]model.AgentActionProposal, 0, len(types))
	for _, proposalType := range types {
		result = append(result, model.AgentActionProposal{Type: string(proposalType), Title: labels[proposalType]})
	}
	return result
}
