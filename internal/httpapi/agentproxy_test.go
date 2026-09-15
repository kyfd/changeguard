package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

// 变更准备代理的安全性质。这里的断言都对应"能被利用的失败模式"，
// 不是样式或文案问题：
//
//  1. 下游没配就显式失败，不能静默返回空结果；
//  2. 组织范围只能来自服务端解析，调用方自带的头必须被丢弃；
//  3. 治理会话 Cookie 不向下游转发；
//  4. 下游共享密钥不能被调用方覆盖。

const testActorID = "usr_developer"

func newAgentTestServer(t *testing.T, baseURL string) http.Handler {
	t.Helper()
	t.Setenv(agentBaseURLEnv, baseURL)
	data := store.NewMemory()
	logger := log.New(io.Discard, "", 0)
	authManager := auth.New(auth.Config{Mode: "disabled"}, data, logger)
	return New(service.New(data, nil, nil), authManager, logger).routes()
}

// recordingUpstream 记录收到的请求头，用来验证代理到底转发了什么。
func recordingUpstream(t *testing.T) (*httptest.Server, *http.Header) {
	t.Helper()
	captured := &http.Header{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*captured = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream, captured
}

func TestAgentAssetsAreEmbedded(t *testing.T) {
	for _, name := range []string{"web/agent/index.html", "web/agent/styles.css", "web/agent/app.js"} {
		if _, err := webAssets.ReadFile(name); err != nil {
			t.Fatalf("material confirmation workbench must be embedded in the binary: %v", err)
		}
	}
}

func TestAgentWorkbenchIsServedFromTheSameOrigin(t *testing.T) {
	t.Setenv(agentBaseURLEnv, "")
	handler := newAgentTestServer(t, "")

	for _, path := range []string{"/agent/", "/agent/app.js", "/agent/styles.css"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/agent/", nil))
	page := recorder.Body.String()
	for _, marker := range []string{"变更准备", "证据与检查", "身份来自控制台会话"} {
		if !strings.Contains(page, marker) {
			t.Fatalf("/agent/ must serve the workbench: missing %q", marker)
		}
	}
	// 目录路径回退成控制台首页是一个真实发生过的退化，这里把它钉住。
	if strings.Contains(page, `id="authGate"`) {
		t.Fatal("/agent/ must serve the workbench, not the console shell")
	}
	// 子路径也必须走工作台处理器，而不是根静态处理器。
	assets := httptest.NewRecorder()
	handler.ServeHTTP(assets, httptest.NewRequest(http.MethodGet, "/agent/app.js", nil))
	if strings.Contains(assets.Body.String(), `id="authGate"`) {
		t.Fatal("/agent/app.js must be served as a script, not fall back to the console")
	}
}

func TestAgentWorkbenchNeverDeclaresItsOwnIdentity(t *testing.T) {
	content, err := webAssets.ReadFile("web/agent/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, forbidden := range []string{"X-Actor-Id", "X-Org-Id", "X-Actor-ID"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("the workbench must not declare identity; found %q", forbidden)
		}
	}
	if !strings.Contains(script, "X-CSRF-Token") {
		t.Fatal("write requests must carry the CSRF token like every other governance API")
	}
}

func TestAgentWorkbenchKeepsMisleadingWordingOut(t *testing.T) {
	content, err := webAssets.ReadFile("web/agent/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, marker := range []string{
		"草案已生成 · 待人工确认",
		"它不是审批结论，不会自动提交变更，也不代表可以在生产执行",
		"检查失败 —— 不得视为通过",
		"失败不等于没有问题",
		"本地编辑未经验证",
		"本地编辑只用于审阅，不会回传服务端",
		"这只能说未命中已知规则，不代表不存在风险",
		"不参与放行判定",
		"风险等级与阻断项由确定性扫描产生",
		"本服务不触发",
		"DEMO_ONLY",
		"NOT_RUN",
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("misleading-wording guard is missing: %q", marker)
		}
	}
}

func TestAgentProxyFailsClosedWhenNotConfigured(t *testing.T) {
	handler := newAgentTestServer(t, "")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), agentBaseURLEnv) {
		t.Fatal("the refusal must name the missing configuration")
	}
}

func TestAgentProxyRejectsUnresolvedActor(t *testing.T) {
	// 直接调用处理器并传入没有 actor 的上下文：这一支必须自己兜住，
	// 不能依赖上游中间件——中间件在演示模式下会给匿名请求兜一个默认成员。
	logger := log.New(io.Discard, "", 0)
	server := &Server{auth: auth.New(auth.Config{Mode: "disabled"}, store.NewMemory(), logger), logger: logger}
	t.Setenv(agentBaseURLEnv, "http://127.0.0.1:1")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil).WithContext(context.Background())
	server.handleAgentProxy(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestAgentProxyForwardsWithServerResolvedIdentity(t *testing.T) {
	upstream, captured := recordingUpstream(t)
	handler := newAgentTestServer(t, upstream.URL)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)
	request.Header.Set("X-Actor-ID", testActorID)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := captured.Get("X-Actor-Id"); got != testActorID {
		t.Fatalf("downstream actor=%q want=%q", got, testActorID)
	}
	if got := captured.Get("X-Org-Id"); got != "org_demo" {
		t.Fatalf("downstream organization=%q want=%q", got, "org_demo")
	}
}

func TestAgentProxyDiscardsCallerSuppliedIdentity(t *testing.T) {
	upstream, captured := recordingUpstream(t)
	handler := newAgentTestServer(t, upstream.URL)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)
	request.Header.Set("X-Actor-ID", testActorID)
	// 调用方试图自行声明组织和共享密钥。
	request.Header.Set("X-Org-Id", "org_attacker")
	request.Header.Set("X-Agent-Upstream-Token", "forged-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if got := captured.Get("X-Org-Id"); got == "org_attacker" {
		t.Fatal("caller-supplied organization must be discarded, not forwarded")
	}
	if got := captured.Get("X-Org-Id"); got != "org_demo" {
		t.Fatalf("organization must come from the store, got %q", got)
	}
	// 未配置共享密钥时，伪造的令牌不允许被透传。
	if got := captured.Get("X-Agent-Upstream-Token"); got != "" {
		t.Fatalf("forged upstream token must not be forwarded, got %q", got)
	}
}

func TestAgentProxySendsSharedSecretAndStripsGovernanceCredentials(t *testing.T) {
	upstream, captured := recordingUpstream(t)
	handler := newAgentTestServer(t, upstream.URL)
	t.Setenv(agentUpstreamToken, "shared-secret-for-agent-service")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)
	request.Header.Set("X-Actor-ID", testActorID)
	request.AddCookie(&http.Cookie{Name: "dbguard_session", Value: "governance-session-value"})
	request.Header.Set("Authorization", "Bearer governance-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if got := captured.Get("X-Agent-Upstream-Token"); got != "shared-secret-for-agent-service" {
		t.Fatalf("upstream token=%q", got)
	}
	if got := captured.Get("Cookie"); got != "" {
		t.Fatalf("governance session cookie must not be forwarded downstream, got %q", got)
	}
	if got := captured.Get("Authorization"); got != "" {
		t.Fatalf("governance authorization must not be forwarded downstream, got %q", got)
	}
}

func TestAgentProxyReportsUnreachableUpstream(t *testing.T) {
	handler := newAgentTestServer(t, "http://127.0.0.1:1")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)
	request.Header.Set("X-Actor-ID", testActorID)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusBadGateway)
	}
}

func TestAgentProxyRejectsInvalidBaseURL(t *testing.T) {
	handler := newAgentTestServer(t, "not-a-url")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/agent/healthz", nil)
	request.Header.Set("X-Actor-ID", testActorID)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusServiceUnavailable)
	}
}
