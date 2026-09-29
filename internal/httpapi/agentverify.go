package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/service"
)

// 正式变更的 Agent 任务关联必须**服务端核对**，不能只凭客户端填写的 ID。
//
// 客户端提供 agent_task_id 时，服务端以**发起人自己的身份**去变更准备服务读取该任务：
// 该接口只返回创建者本人、同组织的任务，因此一个 200 就证明了"任务存在且属于这名成员"。
// 在此基础上再核对来源、应用与材料是否一致；任一项不满足即拒绝可信关联。
//
// 无法核对（未配置 Agent、无共享密钥、不可达、非 200）一律**拒绝**：宁可不建立可信关联，
// 也不能把一个无法验证的 ID 当成"来自 Agent 的正式变更"。
type verifiedAgentTask struct {
	TaskID string `json:"task_id"`
	Source string `json:"source"`
	Status string `json:"status"`
	Slots  struct {
		Application string `json:"application"`
	} `json:"slots"`
	Draft *struct {
		SQL         string `json:"sql"`
		RollbackSQL string `json:"rollback_sql"`
	} `json:"draft"`
}

const verifyAgentTaskTimeout = 5 * time.Second

// resolveVerifiedAgentTask 把请求里的 agent_task_id 换成服务端核对通过的 ID（无关联时返回空串）。
// 核对不通过时返回包装了 service.ErrUntrustedAgentTask 的错误，由调用方映射为拒绝响应。
func (s *Server) resolveVerifiedAgentTask(ctx context.Context, actorID string, input model.CreateChangeInput) (string, error) {
	taskID := strings.TrimSpace(input.AgentTaskID)
	if taskID == "" {
		return "", nil
	}
	organizationID, err := s.auth.ActorOrganization(actorID)
	if err != nil {
		return "", fmt.Errorf("%w：缺少可信身份", service.ErrUntrustedAgentTask)
	}
	if verified, reason := s.verifyAgentTask(ctx, actorID, organizationID, input); !verified {
		return "", fmt.Errorf("%w：%s", service.ErrUntrustedAgentTask, reason)
	}
	return taskID, nil
}

// verifyAgentTask 返回 (是否可信, 拒绝原因)。只有全部核对通过才算可信。
func (s *Server) verifyAgentTask(ctx context.Context, actorID, organizationID string, input model.CreateChangeInput) (bool, string) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(agentBaseURLEnv)), "/")
	if base == "" {
		return false, "变更准备服务未配置（缺少 " + agentBaseURLEnv + "）：无法核对 Agent 任务来源"
	}
	token := strings.TrimSpace(os.Getenv(agentUpstreamToken))
	if token == "" {
		return false, "缺少 Agent 上游凭据（" + agentUpstreamToken + "）：无法核对 Agent 任务来源"
	}
	if actorID == "" || organizationID == "" {
		return false, "缺少可信身份：无法核对 Agent 任务来源"
	}

	target := base + "/api/agent/tasks/" + url.PathEscape(strings.TrimSpace(input.AgentTaskID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, "无法构造 Agent 任务核对请求"
	}
	// 用发起人身份读取：变更准备服务只会返回该成员自己的任务。
	request.Header.Set("X-Actor-Id", actorID)
	request.Header.Set("X-Org-Id", organizationID)
	request.Header.Set("X-Agent-Upstream-Token", token)

	client := &http.Client{Timeout: verifyAgentTaskTimeout, Transport: agentTransport}
	response, err := client.Do(request)
	if err != nil {
		return false, "变更准备服务不可达：无法核对 Agent 任务来源"
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		// 不存在、不属于当前成员或组织：一律同等处理，不区分，避免探测。
		return false, "无法核对 Agent 任务：任务不存在，或不属于当前成员与组织"
	case http.StatusOK:
	default:
		return false, fmt.Sprintf("变更准备服务返回状态码 %d：无法核对 Agent 任务来源", response.StatusCode)
	}

	var task verifiedAgentTask
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAgentRequestBodyBytes)).Decode(&task); err != nil {
		return false, "无法解析 Agent 任务响应：核对未完成"
	}
	if source := strings.TrimSpace(task.Source); source != "production" {
		return false, fmt.Sprintf("Agent 任务来源为 %q：评测 / 演示来源的任务不能用于正式变更", source)
	}
	application := strings.TrimSpace(task.Slots.Application)
	if application == "" || application != strings.TrimSpace(input.ApplicationID) {
		return false, "Agent 任务的应用与本次变更不一致：不能在另一个应用下建立可信关联"
	}
	if task.Draft == nil || strings.TrimSpace(task.Draft.SQL) == "" {
		return false, "Agent 任务没有可核对的草案材料"
	}
	if strings.TrimSpace(task.Draft.SQL) != strings.TrimSpace(input.SQL) ||
		strings.TrimSpace(task.Draft.RollbackSQL) != strings.TrimSpace(input.RollbackSQL) {
		return false, "提交的材料与 Agent 任务当前草案不一致：请使用任务当前版本的 SQL 与回滚方案"
	}
	return true, ""
}
