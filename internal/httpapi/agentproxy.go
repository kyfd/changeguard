package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/auth"
)

// 变更准备 Agent 的对外前缀。浏览器只和本服务说话，Agent 服务退到内网。
//
// 为什么必须由本服务代理，而不是让浏览器直连 Agent 服务：
//
//  1. 安全策略是 `connect-src 'self'` + `frame-ancestors 'none'`，跨源直连会被浏览器拒绝；
//  2. 身份只能由服务端解析。若由浏览器声明身份，任何登录用户都能冒充他人，
//     这正是 `docs/adr/0002` 里反复强调必须避免的失败模式。
const (
	agentBaseURLEnv    = "DBGUARD_AGENT_BASE_URL"
	agentUpstreamToken = "DBGUARD_AGENT_UPSTREAM_TOKEN"
	// 与 JSON 接口保持同一量级的请求体上限。
	maxAgentRequestBodyBytes = 1 << 20
	agentRequestTimeout      = 150 * time.Second
)

// 客户端不得自行携带这些头，一律由代理按已认证会话重写。
var agentIdentityHeaders = []string{"X-Actor-Id", "X-Org-Id", "X-Agent-Upstream-Token"}

// agentTransport 复用连接。Agent 服务是本机/内网依赖，超时按失败处理。
var agentTransport = &http.Transport{
	Proxy: nil,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:          32,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       60 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// agentWorkbenchFS 提供变更准备工作台的静态资源。资源编译进二进制，随服务一起发布。
var agentWorkbenchFS = func() http.FileSystem {
	sub, err := fs.Sub(webAssets, "web/agent")
	if err != nil {
		panic(fmt.Errorf("变更准备工作台资源加载失败: %w", err))
	}
	return http.FS(sub)
}()

// AgentEnabled 表示下游 Agent 服务是否已配置。界面据此决定是否显示入口。
func AgentEnabled() bool {
	return strings.TrimSpace(os.Getenv(agentBaseURLEnv)) != ""
}

// handleAgentWorkbench 提供 /agent/ 下的变更准备工作台。
//
// 为什么需要单独的处理器：根静态处理器会用 `fs.Stat(staticFS, "agent/")` 判断
// 目标是否存在，而带尾斜杠的路径对 `fs.ValidPath` 是非法的，判断必然失败，
// 最终把这个目录路径当成未知页面**回退成控制台首页**。
// 也就是说：依赖根处理器会让 /agent/ 静默显示成控制台。
func (s *Server) handleAgentWorkbench(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	// 复制请求再改写路径：不要就地改写入参，避免影响日志与其他中间件。
	cloned := r.Clone(r.Context())
	cloned.URL.Path = strings.TrimPrefix(r.URL.Path, "/agent")
	if cloned.URL.Path == "" {
		cloned.URL.Path = "/"
	}
	switch {
	case strings.HasSuffix(cloned.URL.Path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(cloned.URL.Path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.FileServer(agentWorkbenchFS).ServeHTTP(w, cloned)
}

// handleAgentProxy 把 /api/agent/* 转发给变更准备 Agent 服务。
//
// 三条不变量：
//   - 未配置下游时**显式失败**（503），不静默返回空结果；
//   - 身份来自**已认证会话**，客户端自带的身份头一律丢弃；
//   - 治理会话 Cookie 不向下游转发，避免把一个服务的凭据泄漏给另一个服务。
func (s *Server) handleAgentProxy(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(os.Getenv(agentBaseURLEnv))
	if raw == "" {
		writeError(w, http.StatusServiceUnavailable, "变更准备 Agent 未启用：需要配置 "+agentBaseURLEnv)
		return
	}

	target, err := url.Parse(raw)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		s.logger.Printf("agent proxy misconfigured: %s=%q", agentBaseURLEnv, raw)
		writeError(w, http.StatusServiceUnavailable, "变更准备 Agent 配置无效")
		return
	}

	actorID, ok := auth.ActorID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "登录状态已失效，请重新登录")
		return
	}
	organizationID, err := s.auth.ActorOrganization(actorID)
	if err != nil {
		// 成员被停用或已删除时按未授权处理，不把停用账号放行到下游。
		writeError(w, http.StatusUnauthorized, "当前成员不存在或已停用")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAgentRequestBodyBytes)

	token := strings.TrimSpace(os.Getenv(agentUpstreamToken))
	proxy := &httputil.ReverseProxy{
		Transport: agentTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// 先清掉客户端可能伪造的身份头，再写入服务端解析结果。
			for _, header := range agentIdentityHeaders {
				pr.Out.Header.Del(header)
			}
			// 下游不需要治理服务的凭据。
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-CSRF-Token")
			pr.Out.Header.Set("X-Actor-Id", actorID)
			pr.Out.Header.Set("X-Org-Id", organizationID)
			if token != "" {
				pr.Out.Header.Set("X-Agent-Upstream-Token", token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				writeError(w, http.StatusBadGateway, "变更准备服务连接中断")
				return
			}
			s.logger.Printf("agent proxy request failed: %v", err)
			writeError(w, http.StatusBadGateway, "变更准备服务暂时不可用")
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), agentRequestTimeout)
	defer cancel()
	proxy.ServeHTTP(w, r.WithContext(ctx))
}
