package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 3.1.2 白盒补充：verifyAgentTask / hasActiveAgentConfirmation 的失败关闭分支。
//
// source_constraint_test.go 已经覆盖了主要的正向与拒绝路径；这里补的是「只有真实
// 分支才走到」的边界：缺少配置、缺少身份、上游不可达、非预期状态码、响应体不可解析、
// 任务没有绑定应用，以及确认记录的各种"无效"形态。它们正是"核对不了就不放行"的
// 实现处——一旦退化，就会变成"缺配置时默认信任"或"空确认也算确认"。

// -- 配置与身份的前置校验（不发任何请求）----------------------------------------

func TestVerifyAgentTaskRequiresAgentBaseURL(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	t.Setenv(agentBaseURLEnv, "")
	t.Setenv(agentUpstreamToken, "shared-secret")

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("未配置 Agent 地址时不得判定可信")
	}
	if !strings.Contains(reason, agentBaseURLEnv) {
		t.Fatalf("拒绝原因应点名缺失的配置项，得到 %q", reason)
	}
}

func TestVerifyAgentTaskRequiresUpstreamToken(t *testing.T) {
	stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
	server, _, _ := newIdempotencyHTTPServer(t)
	t.Setenv(agentBaseURLEnv, stub.URL)
	t.Setenv(agentUpstreamToken, "")

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("缺少上游凭据时不得判定可信")
	}
	if !strings.Contains(reason, agentUpstreamToken) {
		t.Fatalf("拒绝原因应点名缺失的凭据，得到 %q", reason)
	}
}

func TestVerifyAgentTaskRequiresTrustedIdentity(t *testing.T) {
	stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	for _, tc := range []struct{ actor, org string }{{"", "org_demo"}, {"usr_developer", ""}} {
		verified, reason := server.verifyAgentTask(t.Context(), tc.actor, tc.org, agentChangeInput("task_prod_1"))
		if verified {
			t.Fatalf("缺少可信身份（actor=%q org=%q）时不得判定可信", tc.actor, tc.org)
		}
		if !strings.Contains(reason, "缺少可信身份") {
			t.Fatalf("拒绝原因应说明缺少可信身份，得到 %q", reason)
		}
	}
}

// -- 上游响应的异常形态 --------------------------------------------------------

func TestVerifyAgentTaskFailsWhenAgentUnreachable(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, "http://127.0.0.1:1") // 该端口不会有服务在听

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("上游不可达时不得判定可信")
	}
	if !strings.Contains(reason, "不可达") {
		t.Fatalf("拒绝原因应说明不可达，得到 %q", reason)
	}
}

func TestVerifyAgentTaskRejectsUnexpectedStatusCodes(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		stub, _ := newAgentTaskStub(t, status, nil)
		server, _, _ := newIdempotencyHTTPServer(t)
		configureAgent(t, stub.URL)

		verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

		if verified {
			t.Fatalf("状态码 %d 时不得判定可信", status)
		}
		if !strings.Contains(reason, "状态码") {
			t.Fatalf("拒绝原因应显示状态码，得到 %q", reason)
		}
	}
}

func TestVerifyAgentTaskTreatsMissingTaskAsUntrustedWithoutProbing(t *testing.T) {
	stub, _ := newAgentTaskStub(t, http.StatusNotFound, nil)
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("任务不存在时不得判定可信")
	}
	// 不存在 / 不属于当前成员 / 不属于当前组织：三者合并为同一句拒绝原因，避免探测。
	if !strings.Contains(reason, "不存在") || !strings.Contains(reason, "不属于") {
		t.Fatalf("三种情况应合并为同一句拒绝原因，得到 %q", reason)
	}
}

func TestVerifyAgentTaskRejectsUnparsableBody(t *testing.T) {
	server, _, _ := newIdempotencyHTTPServer(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `<html>not a task</html>`)
	}))
	t.Cleanup(stub.Close)
	configureAgent(t, stub.URL)

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("响应体不可解析时不得判定可信")
	}
	if !strings.Contains(reason, "无法解析") {
		t.Fatalf("拒绝原因应说明解析失败，得到 %q", reason)
	}
}

// -- 应用一致性：canonical ID 优先，legacy 名称严格相等，无应用直接拒绝 -------------

func TestVerifyAgentTaskRejectsTaskWithoutApplication(t *testing.T) {
	// 3.1.2 新增分支：任务既没有 canonical ID 也没有名称——不能建立可信关联。
	task := agentTaskPayload(map[string]any{"slots": map[string]any{"application_id": "", "application": ""}})
	stub, _ := newAgentTaskStub(t, http.StatusOK, task)
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("任务未绑定应用时不得判定可信")
	}
	if !strings.Contains(reason, "没有绑定应用") {
		t.Fatalf("拒绝原因应提示先选择应用，得到 %q", reason)
	}
}

func TestVerifyAgentTaskCanonicalIDWinsOverLegacyName(t *testing.T) {
	// 任务有 canonical ID 时只用 ID 比对：名称为其他应用也**不**影响判定。
	task := agentTaskPayload(map[string]any{
		"slots": map[string]any{"application_id": "app_order", "application": "some-other-name"},
	})
	stub, _ := newAgentTaskStub(t, http.StatusOK, task)
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if !verified {
		t.Fatalf("canonical ID 一致时应判定可信（名称仅作展示），得到拒绝原因 %q", reason)
	}
}

func TestVerifyAgentTaskLegacyNameRequiresExactMatch(t *testing.T) {
	task := agentTaskPayload(map[string]any{
		"slots": map[string]any{"application_id": "", "application": "order-service"},
	})
	stub, _ := newAgentTaskStub(t, http.StatusOK, task)
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	// 只做严格相等判定：不模糊匹配、不忽略大小写、不做前后缀匹配。
	for _, submitted := range []string{"order-servic", "Order-Service", "order_service", "app_order"} {
		input := agentChangeInput("task_prod_1")
		input.ApplicationID = submitted
		verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", input)
		if verified {
			t.Fatalf("遗留名称任务与 %q 不应判定为同一应用（只允许严格相等）", submitted)
		}
		if !strings.Contains(reason, "不一致") {
			t.Fatalf("拒绝原因应说明应用不一致，得到 %q", reason)
		}
	}

	input := agentChangeInput("task_prod_1")
	input.ApplicationID = "order-service"
	if verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", input); !verified {
		t.Fatalf("名称严格相等时应判定可信，得到拒绝原因 %q", reason)
	}
}

// -- 确认记录的有效性 -----------------------------------------------------------

func TestHasActiveAgentConfirmation(t *testing.T) {
	empty := ""
	for _, tc := range []struct {
		name          string
		confirmations []verifiedAgentConfirmation
		want          bool
	}{
		{name: "无确认记录", confirmations: nil, want: false},
		{name: "空列表", confirmations: []verifiedAgentConfirmation{}, want: false},
		{name: "有效确认", confirmations: []verifiedAgentConfirmation{{}}, want: true},
		{name: "失效时间为空串仍视为有效", confirmations: []verifiedAgentConfirmation{{InvalidatedAt: &empty}}, want: true},
		{name: "已失效确认", confirmations: []verifiedAgentConfirmation{{InvalidatedAt: ptrString("2026-01-01T00:00:00Z")}}, want: false},
		{name: "部分失效但仍有有效记录", confirmations: []verifiedAgentConfirmation{
			{InvalidatedAt: ptrString("2026-01-01T00:00:00Z")},
			{},
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasActiveAgentConfirmation(tc.confirmations); got != tc.want {
				t.Fatalf("hasActiveAgentConfirmation() = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

func TestVerifyAgentTaskRejectsOnlyInvalidatedConfirmations(t *testing.T) {
	task := agentTaskPayload(map[string]any{
		"confirmations": []map[string]any{{"confirmed_by": "usr_developer", "invalidated_at": "2026-01-01T00:00:00Z"}},
	})
	stub, _ := newAgentTaskStub(t, http.StatusOK, task)
	server, _, _ := newIdempotencyHTTPServer(t)
	configureAgent(t, stub.URL)

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("确认全部失效时不得判定可信")
	}
	if !strings.Contains(reason, "确认") {
		t.Fatalf("拒绝原因应指向缺少有效人工确认，得到 %q", reason)
	}
}

// ptrString 用于构造"显式给出失效时间"的确认记录。
func ptrString(value string) *string { return &value }

// -- resolveVerifiedAgentTask 与请求构造的分支 ----------------------------------

func TestResolveVerifiedAgentTaskRequiresKnownActor(t *testing.T) {
	// 没有可信身份（成员在组织里不存在）时，即使带了 agent_task_id 也必须拒绝。
	server, _, _ := newIdempotencyHTTPServer(t)
	stub, _ := newAgentTaskStub(t, http.StatusOK, agentTaskPayload(nil))
	configureAgent(t, stub.URL)

	if _, err := server.resolveVerifiedAgentTask(t.Context(), "usr_does_not_exist", agentChangeInput("task_prod_1")); err == nil {
		t.Fatal("成员不存在时必须拒绝建立可信关联")
	}
	// 未提供 agent_task_id 时不需要核对，也不应因身份未知而报错。
	input := agentChangeInput("")
	resolved, err := server.resolveVerifiedAgentTask(t.Context(), "usr_does_not_exist", input)
	if err != nil || resolved != "" {
		t.Fatalf("无关联时不应核对身份：resolved=%q err=%v", resolved, err)
	}
}

func TestVerifyAgentTaskRejectsUnconstructibleRequest(t *testing.T) {
	// 非法 base URL（无法构造请求）同样必须失败关闭，而不是跳过核对。
	server, _, _ := newIdempotencyHTTPServer(t)
	t.Setenv(agentBaseURLEnv, "http://[::1")
	t.Setenv(agentUpstreamToken, "shared-secret")

	verified, reason := server.verifyAgentTask(t.Context(), "usr_developer", "org_demo", agentChangeInput("task_prod_1"))

	if verified {
		t.Fatal("无法构造核对请求时不得判定可信")
	}
	if !strings.Contains(reason, "无法构造") {
		t.Fatalf("拒绝原因应说明请求构造失败，得到 %q", reason)
	}
}
