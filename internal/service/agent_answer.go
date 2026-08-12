package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

// assistantAnswer is the deterministic, evidence-carrying reply produced by the
// change-scoped Clawbot. It is intentionally built from local governance state
// so the assistant never depends on a paid model for v1, and it never mutates
// anything.
type assistantAnswer struct {
	Answer    string
	Citations []model.AgentCitation
	Trace     []model.AgentToolTrace
	Proposals []model.AgentActionProposal
}

// buildAssistantAnswer answers a question about a change from deterministic
// evidence: current status, blocking findings, experiment state, approval
// requirements, passports, and post-release outcome signals.
func buildAssistantAnswer(change model.ChangeRequest, data *store.Store, actor model.User, question string) assistantAnswer {
	answer := assistantAnswer{
		Trace: []model.AgentToolTrace{{Tool: "get_change_context", Input: change.ID, Output: string(change.Status) + " / " + change.Title}},
	}

	// Blocking findings are the core of "why is this stuck".
	blocking := make([]model.Finding, 0)
	open := make([]model.Finding, 0)
	for _, finding := range change.Findings {
		if finding.Blocking {
			blocking = append(blocking, finding)
		}
		if finding.Status == model.FindingOpen || finding.Status == model.FindingAssigned {
			open = append(open, finding)
		}
	}
	for _, finding := range blocking {
		answer.Citations = append(answer.Citations, model.AgentCitation{
			Kind: "rule_finding", ID: finding.ID, Title: finding.Title,
			Summary: fmt.Sprintf("%s · %s", finding.Code, finding.Severity),
		})
	}
	answer.Trace = append(answer.Trace, agentFindingTrace(blocking, open))

	// Experiment evidence distinguishes real rehearsals from DEMO_ONLY.
	if change.Experiment != nil {
		experiment := change.Experiment
		summary := fmt.Sprintf("%s / %s", experiment.Mode, experiment.Status)
		answer.Citations = append(answer.Citations, model.AgentCitation{
			Kind: "experiment", ID: experiment.ID, Title: "预发布演练", Summary: summary,
		})
		answer.Trace = append(answer.Trace, model.AgentToolTrace{Tool: "get_experiment_report", Input: experiment.ID, Output: summary})
	}

	// Passport status explains gate failures without exposing tokens.
	passports := data.PassportsByChange(change.OrganizationID, change.ID)
	for _, passport := range passports {
		answer.Citations = append(answer.Citations, model.AgentCitation{
			Kind: "passport", ID: passport.ID, Title: "发布门禁护照",
			Summary: fmt.Sprintf("%s · 签发 %s", passport.Status, formatTime(passport.IssuedAt)),
		})
	}
	answer.Trace = append(answer.Trace, agentPassportTrace(passports))

	// Post-release outcome signals power the retrospective workflow.
	signals := data.OutcomeSignalsByChange(change.OrganizationID, change.ID)
	if len(signals) > 0 {
		answer.Citations = append(answer.Citations, model.AgentCitation{
			Kind: "outcome", ID: change.ID, Title: "发布后结果信号", Summary: fmt.Sprintf("%d 条结果信号", len(signals)),
		})
		answer.Trace = append(answer.Trace, model.AgentToolTrace{Tool: "get_outcome_signals", Input: change.ID, Output: fmt.Sprintf("%d signals", len(signals))})
	}

	answer.Answer, answer.Proposals = composeAssistantAnswer(change, blocking, open, answer.Citations)
	return answer
}

func agentFindingTrace(blocking, open []model.Finding) model.AgentToolTrace {
	output := fmt.Sprintf("%d blocking, %d open", len(blocking), len(open))
	return model.AgentToolTrace{Tool: "get_rule_findings", Input: "blocking=true", Output: output}
}

func agentPassportTrace(passports []model.Passport) model.AgentToolTrace {
	output := "无"
	if len(passports) > 0 {
		output = fmt.Sprintf("%d passports", len(passports))
	}
	return model.AgentToolTrace{Tool: "get_change_passports", Input: "status=all", Output: output}
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format("01-02 15:04")
}

// composeAssistantAnswer turns the change state into a plain-language verdict.
// The verdict is deterministic and always ends with a concrete next step; it
// never claims a release is safe when a blocking finding is unresolved.
func composeAssistantAnswer(change model.ChangeRequest, blocking, open []model.Finding, citations []model.AgentCitation) (string, []model.AgentActionProposal) {
	var lines []string
	proposals := make([]model.ActionProposalType, 0)
	next := "等待负责人处理阻断项后再继续"

	if len(blocking) > 0 {
		lines = append(lines, "结论：当前变更不能放行。")
		lines = append(lines, fmt.Sprintf("原因：还有 %d 个阻断项未解决：", len(blocking)))
		for _, finding := range blocking {
			lines = append(lines, fmt.Sprintf("  - %s（%s）：%s", finding.Title, finding.Code, finding.Suggestion))
		}
		if hints := transactionRemediationHints(append(append([]model.Finding{}, blocking...), open...)); len(hints) > 0 {
			lines = append(lines, "事务优化建议：")
			lines = append(lines, hints...)
		}
		proposals = append(proposals, "remediate")
		lines = append(lines, "下一步：先按建议完成整改，再由独立人员复核验证。")
		next = "整改并提交复核"
	} else if len(open) > 0 {
		lines = append(lines, "结论：当前变更暂不能审批。")
		lines = append(lines, fmt.Sprintf("原因：还有 %d 个待处理发现项（非阻断）：", len(open)))
		for _, finding := range open {
			lines = append(lines, fmt.Sprintf("  - %s（%s）", finding.Title, finding.Code))
		}
		if hints := transactionRemediationHints(open); len(hints) > 0 {
			lines = append(lines, "事务优化建议：")
			lines = append(lines, hints...)
		}
		proposals = append(proposals, "remediate", "review")
		lines = append(lines, "下一步：处理非阻断项并在审批前确认无新增阻断。")
		next = "处理发现项"
	} else if change.Experiment != nil && strings.EqualFold(change.Experiment.Mode, "DEMO_ONLY") {
		lines = append(lines, "结论：当前变更仍处于预发布验证阶段。")
		lines = append(lines, "原因：演练证据为 DEMO_ONLY，不能作为真实放行依据。")
		proposals = append(proposals, "rerun_experiment")
		lines = append(lines, "下一步：使用真实 PostgreSQL 影子演练验证，并确认回滚已验证。")
		next = "执行真实演练"
	} else if change.Experiment != nil && (change.Experiment.LockWaitMS > 1000 || change.Experiment.DurationMS >= 5000) {
		lines = append(lines, "结论：演练已完成，但事务成本偏高，建议优化后再审批。")
		lines = append(lines, fmt.Sprintf("证据：锁/最慢语句约 %dms，总耗时 %dms。", change.Experiment.LockWaitMS, change.Experiment.DurationMS))
		lines = append(lines, "事务优化建议：")
		lines = append(lines, "  - 分批 DML（LIMIT + 主键游标），控制单事务行数")
		lines = append(lines, "  - DDL 使用 CONCURRENTLY / NOT VALID 分阶段")
		lines = append(lines, "  - 生产执行声明 lock_timeout 与 statement_timeout")
		proposals = append(proposals, "remediate", "rerun_experiment")
		next = "优化事务后重新演练"
	} else if change.Status == model.StatusWaitingApproval {
		lines = append(lines, "结论：当前变更可以进入审批环节。")
		lines = append(lines, "状态：等待审批，且无阻断项、无未处理发现项。")
		proposals = append(proposals, "review")
		lines = append(lines, "下一步：由具备审批权限的独立 reviewer 审核并决定放行。")
		next = "提交审批"
	} else if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		lines = append(lines, "结论：当前变更已通过审批。")
		lines = append(lines, "状态："+string(change.Status)+"。")
		next = "查看发布结果"
	} else {
		lines = append(lines, "结论：当前变更处于「"+string(change.Status)+"」阶段。")
		lines = append(lines, "尚无阻断项，也没有明确的可放行状态。")
		next = "推进到预发布验证"
	}

	answer := strings.Join(lines, "\n") + "\n\n下一步：" + next
	return answer, assistantProposals(proposals)
}

// transactionRemediationHints maps transaction-optimization finding codes to
// concrete, production-oriented next steps for Clawbot answers.
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
		"remediate":        "整改并提交复核",
		"rerun_experiment": "重新执行真实演练",
		"reassign":         "重新指派处理人",
		"review":           "提交人工审批",
		"observe":          "观察发布后指标",
	}
	result := make([]model.AgentActionProposal, 0, len(types))
	for _, proposalType := range types {
		result = append(result, model.AgentActionProposal{
			Type: string(proposalType), Title: labels[proposalType],
		})
	}
	return result
}
