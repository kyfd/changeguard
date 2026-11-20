package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/report"
	"github.com/kyfd/changeguard/internal/service"
)

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

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		changes, err := s.service.ChangesFor(actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		// 按 Agent 任务关联过滤：关联是服务端记录的，这里只做同组织内查询。
		if agentTask := strings.TrimSpace(r.URL.Query().Get("agent_task_id")); agentTask != "" {
			filtered := make([]model.ChangeRequest, 0, len(changes))
			for _, change := range changes {
				if change.AgentTaskID == agentTask {
					filtered = append(filtered, change)
				}
			}
			changes = filtered
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
		actor := actorID(r)
		key, ok := validatedIdempotencyKey(w, r)
		if !ok {
			return
		}
		if key == "" {
			// 无幂等键：没有可重放的结果，先核对 Agent 任务再创建。
			verifiedAgentTaskID, resolveErr := s.resolveVerifiedAgentTask(r.Context(), actor, input)
			if resolveErr != nil {
				writeServiceError(w, resolveErr)
				return
			}
			w.Header().Set("Idempotency-Status", "not-requested")
			change, err := s.service.CreateVerified(input, actor, verifiedAgentTaskID)
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, change)
			return
		}
		// 幂等：先按（幂等键 + 请求摘要）判定重放——已经成功的创建必须原样返回，
		// 不能因为下游暂时不可用就把一次成功的创建误报成失败。只有**首次创建**
		// 才去核对 Agent 任务的当前材料（核对在 execute 内部执行）。
		change, replayed, err := s.service.CreateIdempotent(
			input, actor, key, requestDigest("CREATE_CHANGE", "change", input),
			func() (string, error) { return s.resolveVerifiedAgentTask(r.Context(), actor, input) },
		)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if replayed {
			w.Header().Set("Idempotency-Replayed", "true")
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
	if action == "outcomes" && len(parts) == 2 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		status, signals, err := s.service.OutcomeSummaryForChange(id, actorID(r))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  status,
			"signals": signals,
			"source":  "recorded_outcome_signals",
		})
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
	if consume && result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
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
