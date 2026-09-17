package httpapi

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/kyfd/changeguard/internal/model"
)

// 变更准备 Agent 的**内部只读**接口。
//
// 为什么需要它：Agent 后端的三个远程只读工具（get_change_context / get_rule_findings /
// get_experiment_report）原先直接请求 `/api/changes/{id}`，而那条路径挂在会话中间件下。
// Agent 进程没有、也不应该持有治理会话（`handleAgentProxy` 反过来还刻意删掉了
// Cookie 与 Authorization），所以真实部署下这三个工具必然拿到 401。
//
// 这里的信任模型是两层，**缺一不可**：
//
//  1. **服务认证**：调用方必须持有共享密钥，常量时间比较。密钥未配置时一律返回 503，
//     而不是"没配就不检查"——那会变成一个人人可用的匿名只读入口。
//  2. **受约束的用户委托**：`X-Actor-Id` / `X-Org-Id` 只是**声明**，声明本身不产生任何权限。
//     必须回查存储确认该成员存在且启用、组织与声明一致，再由 service 做组织与应用级授权。
//
// 该接口只读、只暴露三个窄投影，且**不复用会话中间件**——浏览器拿不到共享密钥，
// 因此这个入口对浏览器不可达。
const (
	agentToolsPrefix       = "/api/agent-tools/"
	agentToolsChangePrefix = "/api/agent-tools/changes/"
	agentToolsSecretEnv    = "DBGUARD_AGENT_UPSTREAM_TOKEN"

	agentProjectionContext    = "context"
	agentProjectionFindings   = "findings"
	agentProjectionExperiment = "experiment"
)

// AgentToolsEnabled 表示内部只读工具接口是否已启用。未配置共享密钥即视为未启用。
func AgentToolsEnabled() bool {
	return strings.TrimSpace(os.Getenv(agentToolsSecretEnv)) != ""
}

// agentToolChangeView 是内部只读接口的响应。
//
// 每个投影只填自己那几个字段，其余留空——窄契约比"返回整个变更对象"更容易审查，
// 也让"Agent 到底能读到什么"在类型上就是可见的。
type agentToolChangeView struct {
	Projection string `json:"projection"`
	Version    int    `json:"version"`

	// context 投影
	ID             string `json:"id,omitempty"`
	Title          string `json:"title,omitempty"`
	ApplicationID  string `json:"application_id,omitempty"`
	Environment    string `json:"environment,omitempty"`
	ChangeType     string `json:"change_type,omitempty"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	// DescriptionUntrusted 是**不可信文本**。字段名里保留 untrusted 是刻意的：
	// 契约本身就要提醒调用方，这是数据而不是指令。
	DescriptionUntrusted string `json:"description_untrusted,omitempty"`

	// findings 投影
	Risk     string          `json:"risk,omitempty"`
	Findings []model.Finding `json:"findings,omitempty"`

	// experiment 投影
	Experiment       *model.ExperimentReport `json:"experiment,omitempty"`
	ExperimentStatus string                  `json:"status,omitempty"`
}

// handleAgentTools 处理内部只读工具请求。它不依赖会话，只依赖共享密钥与成员委托。
func (s *Server) handleAgentTools(w http.ResponseWriter, r *http.Request) {
	expected := strings.TrimSpace(os.Getenv(agentToolsSecretEnv))
	if expected == "" {
		// 失败关闭：没有共享密钥就没有可信调用方，不提供匿名只读入口。
		writeError(w, http.StatusServiceUnavailable, "内部只读工具接口未启用：需要配置 "+agentToolsSecretEnv)
		return
	}
	provided := strings.TrimSpace(r.Header.Get("X-Agent-Upstream-Token"))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeError(w, http.StatusUnauthorized, "服务凭据无效")
		return
	}

	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !strings.HasPrefix(r.URL.Path, agentToolsChangePrefix) {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	changeID := strings.TrimPrefix(r.URL.Path, agentToolsChangePrefix)
	if changeID == "" || strings.Contains(changeID, "/") {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}

	projection := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("projection")))
	switch projection {
	case agentProjectionContext, agentProjectionFindings, agentProjectionExperiment:
	default:
		// 未知投影直接拒绝，不做"默认返回全部"的兜底。
		writeError(w, http.StatusBadRequest, "projection 必须是 context / findings / experiment 之一")
		return
	}

	actorID := strings.TrimSpace(r.Header.Get("X-Actor-Id"))
	declaredOrganization := strings.TrimSpace(r.Header.Get("X-Org-Id"))
	if actorID == "" || declaredOrganization == "" {
		writeError(w, http.StatusUnauthorized, "缺少委托成员身份")
		return
	}
	actor, err := s.service.ActorFor(actorID)
	if err != nil {
		// 成员不存在或已停用：与"无权"同等处理，不区分，避免探测成员是否存在。
		writeError(w, http.StatusForbidden, "委托成员不存在或已停用")
		return
	}
	if actor.OrganizationID != declaredOrganization {
		// 声明的组织与成员真实归属不一致，说明调用方在冒充另一个组织。
		writeError(w, http.StatusForbidden, "委托组织与成员归属不一致")
		return
	}

	// ChangeFor 内部会重新校验组织隔离与应用的 view 授权。
	change, err := s.service.ChangeFor(changeID, actorID)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, agentToolChangeProjection(change, projection))
}

func agentToolChangeProjection(change model.ChangeRequest, projection string) agentToolChangeView {
	view := agentToolChangeView{Projection: projection, Version: change.Version}
	switch projection {
	case agentProjectionContext:
		view.ID = change.ID
		view.Title = change.Title
		view.ApplicationID = change.ApplicationID
		view.Environment = change.Environment
		view.ChangeType = change.ChangeType
		view.ArtifactSHA256 = change.ArtifactSHA256
		view.DescriptionUntrusted = change.Description
	case agentProjectionFindings:
		view.Risk = string(change.Risk)
		view.Findings = change.Findings
	case agentProjectionExperiment:
		view.Experiment = change.Experiment
		view.ExperimentStatus = "NOT_RUN"
		if change.Experiment != nil && strings.TrimSpace(change.Experiment.Status) != "" {
			view.ExperimentStatus = change.Experiment.Status
		}
	}
	return view
}
