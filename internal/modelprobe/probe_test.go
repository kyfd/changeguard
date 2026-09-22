package modelprobe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kyfd/changeguard/internal/model"
)

// allowAll 让测试绕过 SSRF 策略，以便验证请求形状。
// httptest 监听环回地址，会被真实策略拒绝；策略本身由其他用例覆盖。
func allowAll(string) error { return nil }

// 环回、私网、链路本地、云元数据端点必须被拒绝。
// 这条不成立，企业管理员就能把服务端当成内网探针使用。
func TestValidateBaseURLBlocksInternalTargets(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1:8080",
		"http://localhost:9000",
		"https://127.0.0.1",
		"http://10.0.0.5/v1",
		"http://192.168.1.10/v1",
		"http://172.16.0.9/v1",
		"http://169.254.169.254/latest/meta-data", // 云元数据
		"http://[::1]:8080/v1",
		"http://0.0.0.0/v1",
		"http://100.64.0.1/v1", // CGNAT
		"http://metadata.internal/v1",
		"http://printer.local/v1",
	}
	for _, raw := range blocked {
		err := ValidateBaseURL(raw, false)
		if err == nil {
			t.Fatalf("ValidateBaseURL(%q) unexpectedly allowed", raw)
		}
		// 错误必须可读：只说"不允许"而不说为什么，用户无法自查。
		if !strings.Contains(err.Error(), "不被允许") {
			t.Fatalf("ValidateBaseURL(%q) error is not actionable: %v", raw, err)
		}
	}
}

// 显式开启 allowPrivate 后，私网与环回地址放行——否则纯内网部署下这个功能不可用。
// 但云元数据端点仍然拒绝：它从来不是"合法的模型网关"。
func TestValidateBaseURLAllowsPrivateOnlyWhenOptedIn(t *testing.T) {
	privateTargets := []string{
		"http://127.0.0.1:8080",
		"http://10.0.0.5/v1",
		"http://192.168.1.10/v1",
		// 不放需要真实 DNS 的名字：解析失败是另一类错误，与策略无关。
	}
	for _, raw := range privateTargets {
		if err := ValidateBaseURL(raw, true); err != nil {
			t.Fatalf("ValidateBaseURL(%q, true) unexpectedly rejected: %v", raw, err)
		}
	}
	// 元数据端点即使开了私网放行也不允许：它不是模型服务。
	if err := ValidateBaseURL("http://169.254.169.254/latest/meta-data", true); err == nil {
		t.Fatal("cloud metadata endpoint must stay blocked even with allowPrivate")
	}
	// 非 http 协议与空主机在任何模式下都拒绝。
	for _, raw := range []string{"", "ftp://10.0.0.1", "file:///etc/passwd"} {
		if err := ValidateBaseURL(raw, true); err == nil {
			t.Fatalf("ValidateBaseURL(%q, true) unexpectedly allowed", raw)
		}
	}
}

func TestAllowPrivateUpstreamReadsEnvironment(t *testing.T) {
	t.Setenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM", "")
	if AllowPrivateUpstream() {
		t.Fatal("empty value must default to closed")
	}
	t.Setenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM", "1")
	if !AllowPrivateUpstream() {
		t.Fatal("explicit 1 must open the gate")
	}
	t.Setenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM", "true")
	if !AllowPrivateUpstream() {
		t.Fatal("explicit true must open the gate")
	}
	t.Setenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM", "yes")
	if AllowPrivateUpstream() {
		t.Fatal("an unrecognised value must stay closed rather than guess")
	}
}

func TestValidateBaseURLRejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{"ftp://example.com", "file:///etc/passwd", "gopher://example.com", "//example.com/v1"} {
		if err := ValidateBaseURL(raw, false); err == nil {
			t.Fatalf("ValidateBaseURL(%q) unexpectedly allowed", raw)
		}
	}
}

func TestValidateBaseURLRequiresHost(t *testing.T) {
	for _, raw := range []string{"", "   ", "http://", "https:///v1"} {
		if err := ValidateBaseURL(raw, false); err == nil {
			t.Fatalf("ValidateBaseURL(%q) unexpectedly allowed", raw)
		}
	}
}

func TestChatCompletionsURLDerivation(t *testing.T) {
	cases := []struct {
		base     string
		provider model.ModelProviderKind
		want     string
	}{
		{"https://api.deepseek.com", model.ModelProviderOpenAI, "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/", model.ModelProviderOpenAI, "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1", model.ModelProviderOpenAI, "https://api.deepseek.com/v1/chat/completions"},
		{"https://gw.example.com/v1/chat/completions", model.ModelProviderOpenAI, "https://gw.example.com/v1/chat/completions"},
		{"https://api.anthropic.com", model.ModelProviderAnthropic, "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", model.ModelProviderAnthropic, "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1/messages", model.ModelProviderAnthropic, "https://api.anthropic.com/v1/messages"},
	}
	for _, tc := range cases {
		got, err := chatCompletionsURL(tc.base, tc.provider)
		if err != nil {
			t.Fatalf("chatCompletionsURL(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("chatCompletionsURL(%q, %s) = %q want %q", tc.base, tc.provider, got, tc.want)
		}
	}
}

// Anthropic 没有公开的模型列表端点：必须明确报错，
// 而不是发一个必然 404 的请求让用户看到一句难懂的上游错误。
func TestModelsURLRejectsAnthropic(t *testing.T) {
	if _, err := modelsURL("https://api.anthropic.com", model.ModelProviderAnthropic); err == nil {
		t.Fatal("expected an error for the Anthropic models endpoint")
	}
}

func TestModelsURLDerivation(t *testing.T) {
	got, err := modelsURL("https://api.deepseek.com", model.ModelProviderOpenAI)
	if err != nil {
		t.Fatalf("modelsURL: %v", err)
	}
	if got != "https://api.deepseek.com/v1/models" {
		t.Fatalf("modelsURL = %q", got)
	}
}

// 探测请求的形状：路径、鉴权头、以及 max_tokens 必须压到 1。
func TestPingSendsExpectedRequest(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		payload, _ := io.ReadAll(r.Body)
		gotBody = string(payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()

	// 先确认真实策略确实会拒绝这个环回测试服务器。
	if err := ValidateBaseURL(server.URL, false); err == nil {
		t.Fatal("expected the loopback test server to be blocked by the SSRF policy")
	}

	client := &Client{http: server.Client(), validate: allowAll}
	message, err := client.Ping(model.ModelConnectionTest{
		Provider: model.ModelProviderOpenAI,
		BaseURL:  server.URL,
		Model:    "test-model",
		APIKey:   "sk-test",
	})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("request path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"max_tokens":1`) {
		t.Fatalf("request body did not cap max_tokens: %s", gotBody)
	}
	if !strings.Contains(gotBody, "test-model") {
		t.Fatalf("request body did not carry the model: %s", gotBody)
	}
	if message == "" {
		t.Fatal("expected a non-empty success message")
	}
}

// Anthropic 形态必须用 x-api-key 而不是 Bearer。
func TestPingUsesAnthropicHeaders(t *testing.T) {
	var gotKey, gotVersion, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"pong"}]}`))
	}))
	defer server.Close()

	client := &Client{http: server.Client(), validate: allowAll}
	if _, err := client.Ping(model.ModelConnectionTest{
		Provider: model.ModelProviderAnthropic,
		BaseURL:  server.URL,
		Model:    "claude-3-5-haiku-latest",
		APIKey:   "sk-ant-test",
	}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotKey != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Fatal("anthropic-version header missing")
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("request path = %q", gotPath)
	}
}

// ListModels 必须解析 data[]、去重、并在空列表时报错。
func TestListModelsParsesDeduplicatesAndRejectsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"deepseek-chat","owned_by":"deepseek"},
			{"id":"deepseek-chat","owned_by":"deepseek"},
			{"id":"","owned_by":"ignored"},
			{"id":"deepseek-reasoner"}
		]}`))
	}))
	defer server.Close()

	client := &Client{http: server.Client(), validate: allowAll}
	models, err := client.ListModels(model.ModelConnectionTest{
		Provider: model.ModelProviderOpenAI, BaseURL: server.URL, APIKey: "sk-x",
	})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 deduplicated models, got %d: %+v", len(models), models)
	}
	if models[0].ID != "deepseek-chat" {
		t.Fatalf("first model = %q", models[0].ID)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer empty.Close()
	emptyClient := &Client{http: empty.Client(), validate: allowAll}
	if _, err := emptyClient.ListModels(model.ModelConnectionTest{
		Provider: model.ModelProviderOpenAI, BaseURL: empty.URL, APIKey: "sk-x",
	}); err == nil {
		t.Fatal("expected an error for an empty model list")
	}
}

// 上游 4xx 的错误信息不得包含响应体：那里面常回显 Authorization 头。
func TestPingDoesNotLeakUpstreamBody(t *testing.T) {
	err := classifyFailure(http.StatusUnauthorized)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-DO-NOT-LEAK-THIS") {
		t.Fatalf("error message leaked the upstream body: %v", err)
	}
	if !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("error message is not actionable: %v", err)
	}
}

func TestClassifyFailureCoversStatusFamilies(t *testing.T) {
	cases := map[int]string{
		http.StatusForbidden:           "凭据",
		http.StatusNotFound:            "404",
		http.StatusTooManyRequests:     "限流",
		http.StatusInternalServerError: "上游服务错误",
		http.StatusTeapot:              "非预期状态",
	}
	for status, want := range cases {
		err := classifyFailure(status)
		if err == nil {
			t.Fatalf("classifyFailure(%d) returned nil", status)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("classifyFailure(%d) = %v, want it to mention %q", status, err, want)
		}
	}
}

func TestPingRejectsMissingCredentials(t *testing.T) {
	client := New(false)
	if _, err := client.Ping(model.ModelConnectionTest{BaseURL: "https://api.deepseek.com"}); err == nil {
		t.Fatal("expected an error when the API key is missing")
	}
	if _, err := client.Ping(model.ModelConnectionTest{
		BaseURL: "https://api.deepseek.com", APIKey: "sk-x",
	}); err == nil {
		t.Fatal("expected an error when the model name is missing")
	}
}

// 不可达的公网地址：错误必须可读，且不能把 url.Error 原样抛出
// （那里面可能带上完整 URL 与查询串）。
//
// 两件事要分开看：
//   - 校验阶段（ValidateBaseURL）解析不到主机时**应该**说出主机名——那是用户刚填的，
//     不说出来用户不知道该改哪；
//   - 连接阶段（do）失败时只说"无法连接"，因为 url.Error 里带着完整 URL。
//
// 这条用例覆盖前者；后者由 do() 的实现保证。
func TestPingUnreachableHostErrorIsActionable(t *testing.T) {
	client := New(false)
	_, err := client.Ping(model.ModelConnectionTest{
		Provider: model.ModelProviderOpenAI,
		BaseURL:  "https://invalid.invalid",
		APIKey:   "sk-x",
		Model:    "m",
	})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if err.Error() == "" {
		t.Fatal("expected a non-empty error")
	}
	// 必须点出是哪个地址解析不了，用户才知道改哪里。
	if !strings.Contains(err.Error(), "invalid.invalid") {
		t.Fatalf("error did not name the address: %v", err)
	}
	// 但不能带上协议、路径或查询串。
	for _, forbidden := range []string{"https://", "http://", "?", "/v1"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked URL structure (%q): %v", forbidden, err)
		}
	}
}
