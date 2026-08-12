package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/agent"
	"github.com/liufengxi/dbguard/internal/experiment"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

func newAssistantTestService(t *testing.T) *Service {
	t.Helper()
	data := store.NewMemory()
	svc := New(data, experiment.NewFromEnvironment(), agent.NewFromEnvironment())
	return svc
}

// assistantDemoChange builds a WAITING_APPROVAL change on org_demo with a
// blocking finding so the assistant must report "cannot release".
func assistantDemoChange(t *testing.T, svc *Service) model.ChangeRequest {
	t.Helper()
	actor, err := svc.activeActor("usr_developer")
	if err != nil {
		t.Fatalf("activeActor: %v", err)
	}
	application, err := svc.store.Application("app_order")
	if err != nil {
		t.Fatalf("application: %v", err)
	}
	change := model.ChangeRequest{
		OrganizationID: actor.OrganizationID, ID: "chg_assist_test", Title: "助手测试变更",
		ApplicationID: application.ID, ApplicationName: application.Name,
		Environment: "生产环境", ChangeType: "DDL",
		SQL: "ALTER TABLE orders ADD COLUMN note varchar(255);", RollbackSQL: "ALTER TABLE orders DROP COLUMN IF EXISTS note;",
		SubmitterID: actor.ID, SubmitterName: actor.Name,
		Status: model.StatusWaitingApproval, Risk: model.RiskHigh,
		Findings: []model.Finding{
			{ID: "finding_assist_lock", Code: "DDL_LOCK_IMPACT", Severity: model.RiskHigh, Title: "线上 DDL 未使用 CONCURRENTLY", Suggestion: "使用维护窗口执行。", Blocking: true, Status: model.FindingOpen},
			{ID: "finding_assist_meta", Code: "DDL_FULL_SCAN", Severity: model.RiskMedium, Title: "可能触发全表扫描", Suggestion: "评估索引方案。", Blocking: false, Status: model.FindingResolved},
		},
		CreatedAt: mustTime("2026-08-01T10:00:00Z"), UpdatedAt: mustTime("2026-08-01T10:00:00Z"),
	}
	if err := svc.store.CreateChange(change, audit(actor, change.ID, "CREATE", "测试")); err != nil {
		t.Fatalf("CreateChange: %v", err)
	}
	return change
}

func TestAskChangeAssistantBlocksOnFindings(t *testing.T) {
	svc := newAssistantTestService(t)
	change := assistantDemoChange(t, svc)
	message, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_developer", AskChangeAssistantInput{Question: "为什么不能审批？"})
	if err != nil {
		t.Fatalf("AskChangeAssistant: %v", err)
	}
	if !strings.Contains(message.Answer, "不能放行") {
		t.Fatalf("expected blocking verdict, got: %s", message.Answer)
	}
	if !strings.Contains(message.Answer, "DDL_LOCK_IMPACT") {
		t.Fatalf("expected blocking code in answer, got: %s", message.Answer)
	}
	foundCitation := false
	for _, citation := range message.Citations {
		if citation.Kind == "rule_finding" && citation.ID == "finding_assist_lock" {
			foundCitation = true
		}
	}
	if !foundCitation {
		t.Fatalf("expected blocking finding citation, got %+v", message.Citations)
	}
	if len(message.Trace) == 0 {
		t.Fatalf("expected tool trace")
	}
}

func TestAskChangeAssistantRejectsCrossTenant(t *testing.T) {
	svc := newAssistantTestService(t)
	change := assistantDemoChange(t, svc)
	// A user from a different organization must not see this change.
	other := model.User{ID: "usr_other_org", OrganizationID: "org_other", Name: "别人", Role: "后端开发", Email: "other@other.com", Active: true}
	if _, err := svc.store.CreateSSOUser(other, model.UserCredential{}, "", model.AuditEvent{}); err != nil {
		t.Fatalf("CreateSSOUser: %v", err)
	}
	_, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_other_org", AskChangeAssistantInput{Question: "能审批吗？"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestAskChangeAssistantConversationContinuation(t *testing.T) {
	svc := newAssistantTestService(t)
	change := assistantDemoChange(t, svc)
	first, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_developer", AskChangeAssistantInput{Question: "为什么卡住了？"})
	if err != nil {
		t.Fatalf("first ask: %v", err)
	}
	second, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_developer", AskChangeAssistantInput{Question: "那怎么修？", ConversationID: first.ConversationID})
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	if second.ConversationID != first.ConversationID {
		t.Fatalf("expected same conversation, got %s vs %s", second.ConversationID, first.ConversationID)
	}
	summary, err := svc.AgentConversationFor(change.ID, first.ConversationID, "usr_developer")
	if err != nil {
		t.Fatalf("AgentConversationFor: %v", err)
	}
	if len(summary.Messages) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(summary.Messages))
	}
}

func TestAskChangeAssistantIncludesTransactionHints(t *testing.T) {
	svc := newAssistantTestService(t)
	actor, err := svc.activeActor("usr_developer")
	if err != nil {
		t.Fatalf("activeActor: %v", err)
	}
	change := model.ChangeRequest{
		OrganizationID: actor.OrganizationID, ID: "chg_assist_tx", Title: "事务优化助手测试",
		ApplicationID: "app_order", ApplicationName: "订单中心",
		Environment: "生产环境", ChangeType: "DML",
		SQL:         "UPDATE orders SET archive_flag=1 WHERE created_at < NOW() - INTERVAL '180 days';",
		RollbackSQL: "UPDATE orders SET archive_flag=0 WHERE archive_flag=1;",
		SubmitterID: actor.ID, SubmitterName: actor.Name,
		Status: model.StatusCheckFailed, Risk: model.RiskMedium,
		Findings: []model.Finding{
			{ID: "finding_assist_unbatched", Code: "UNBATCHED_LARGE_DML", Severity: model.RiskMedium, Title: "大批量 DML 缺少分批边界", Suggestion: "按主键分批。", Blocking: false, Status: model.FindingOpen},
			{ID: "finding_assist_timeout", Code: "MISSING_LOCK_TIMEOUT", Severity: model.RiskMedium, Title: "未声明锁超时", Suggestion: "设置 lock_timeout。", Blocking: false, Status: model.FindingOpen},
		},
		CreatedAt: mustTime("2026-08-01T10:00:00Z"), UpdatedAt: mustTime("2026-08-01T10:00:00Z"),
	}
	if err := svc.store.CreateChange(change, audit(actor, change.ID, "CREATE", "测试")); err != nil {
		t.Fatalf("CreateChange: %v", err)
	}
	message, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_developer", AskChangeAssistantInput{Question: "怎么修事务问题？"})
	if err != nil {
		t.Fatalf("AskChangeAssistant: %v", err)
	}
	if !strings.Contains(message.Answer, "事务优化建议") {
		t.Fatalf("expected transaction remediation section, got: %s", message.Answer)
	}
	if !strings.Contains(message.Answer, "分批") && !strings.Contains(message.Answer, "lock_timeout") {
		t.Fatalf("expected concrete TX hints, got: %s", message.Answer)
	}
}

func TestAskChangeAssistantEmptyQuestionRejected(t *testing.T) {
	svc := newAssistantTestService(t)
	change := assistantDemoChange(t, svc)
	_, err := svc.AskChangeAssistant(context.Background(), change.ID, "usr_developer", AskChangeAssistantInput{Question: "   "})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
