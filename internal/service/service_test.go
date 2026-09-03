package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

type fakeRunner struct{}

type countingRunner struct {
	targetID string
	runs     int
}

func (r *countingRunner) Run(ctx context.Context, change model.ChangeRequest) model.ExperimentReport {
	if change.ID == r.targetID {
		r.runs++
	}
	return fakeRunner{}.Run(ctx, change)
}

func (fakeRunner) Run(_ context.Context, change model.ChangeRequest) model.ExperimentReport {
	now := time.Now()
	return model.ExperimentReport{
		ID: "exp_test", Mode: "POSTGRES", Status: "PASSED", StartedAt: now,
		FinishedAt: now, DurationMS: 12, DatasetRows: 10000, LockWaitMS: 8,
		P99BeforeMS: 10, P99AfterMS: 11, RollbackVerified: true,
		ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: change.RuleSetVersion,
	}
}

type fakeAnalyzer struct{}

func (fakeAnalyzer) Analyze(_ context.Context, change model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{
		Provider: "test", Risk: change.Risk, Summary: "证据检查通过",
		Reasons: []string{"规则和演练均通过"}, Suggestions: []string{"按计划执行"},
		GeneratedAt: time.Now(),
	}
}

func TestZeroWorkersLeaveQueuedBusinessWorkUntouched(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("z", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "staging worker isolation", ApplicationID: "app_order", ChangeType: "DDL",
		SQL:         "CREATE INDEX CONCURRENTLY idx_staging ON orders(status);",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_staging;",
		Description: "verify that zero workers do not consume queued work",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.QueueExperiment(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, 0)
	time.Sleep(100 * time.Millisecond)

	unchanged, err := svc.Change(change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != model.StatusExperimentQueued {
		t.Fatalf("zero workers changed queued status to %s", unchanged.Status)
	}
	events := data.OutboxByOrganization(unchanged.OrganizationID, true, 0)
	found := false
	for _, event := range events {
		if event.AggregateID != change.ID {
			continue
		}
		found = true
		if event.Status != model.OutboxPending || event.Attempts != 0 {
			t.Fatalf("zero workers consumed event: status=%s attempts=%d", event.Status, event.Attempts)
		}
	}
	if !found {
		t.Fatal("expected queued outbox evidence")
	}
}

func TestStartupRecoveryTakesOverExpiredApplyGeneration(t *testing.T) {
	data := store.NewMemory()
	runner := &countingRunner{}
	svc := New(data, runner, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "startup lease recovery", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "CREATE INDEX CONCURRENTLY idx_recover ON orders(status);", RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_recover;",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	runner.targetID = change.ID
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.QueueExperiment(change.ID, "usr_developer"); err != nil {
		t.Fatal(err)
	}
	oldLease, err := data.ClaimOutbox("crashed-worker", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.CheckpointExperimentOutbox(oldLease.ID, "crashed-worker", oldLease.LeaseGeneration, model.OutboxStageApply, oldLease.InputSHA256); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, 1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		change, err = svc.Change(change.ID)
		if err != nil {
			t.Fatal(err)
		}
		if change.Status == model.StatusWaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if change.Status != model.StatusWaitingApproval || runner.runs != 1 {
		t.Fatalf("startup takeover did not rerun one isolated APPLY: status=%s runs=%d", change.Status, runner.runs)
	}
	events := data.OutboxByOrganization(change.OrganizationID, true, 0)
	var recovered model.OutboxEvent
	for _, event := range events {
		if event.ID == oldLease.ID {
			recovered = event
		}
	}
	if recovered.Status != model.OutboxCompleted || recovered.LeaseGeneration <= oldLease.LeaseGeneration || recovered.AttemptID != oldLease.AttemptID || recovered.ResultDigest == "" {
		t.Fatalf("unexpected recovered outbox: %+v", recovered)
	}
	if change.Experiment == nil || change.Experiment.AttemptID != recovered.AttemptID || change.Experiment.LeaseGeneration != recovered.LeaseGeneration || change.Experiment.InputSHA256 != change.ArtifactSHA256 {
		t.Fatalf("report is not bound to recovered attempt: %+v", change.Experiment)
	}
}

func TestWorkflowRequiresDifferentApprover(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("p", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, 1)

	change, err := svc.Create(model.CreateChangeInput{
		Title: "订单查询索引优化", ApplicationID: "app_order", ChangeType: "DDL",
		SQL:         "CREATE INDEX CONCURRENTLY idx_orders_status ON orders(status);",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_status;",
		Description: "降低订单列表查询耗时",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusReadyForExperiment {
		t.Fatalf("expected ready for experiment, got %s", change.Status)
	}
	if _, err = svc.QueueExperiment(change.ID, "usr_developer"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		change, err = svc.Change(change.ID)
		if err != nil {
			t.Fatal(err)
		}
		if change.Status == model.StatusWaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if change.Status != model.StatusWaitingApproval {
		t.Fatalf("experiment did not complete, got %s", change.Status)
	}
	commented, err := svc.AddComment(change.ID, "usr_reviewer", "请在低峰窗口执行并观察锁等待")
	if err != nil {
		t.Fatal(err)
	}
	if len(commented.Comments) != 1 || commented.Comments[0].AuthorID != "usr_reviewer" {
		t.Fatalf("unexpected comments: %#v", commented.Comments)
	}
	if _, err = svc.Approve(change.ID, "usr_developer", "self approve"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden self approval, got %v", err)
	}
	change, err = svc.Approve(change.ID, "usr_reviewer", "已核对证据")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusApproved {
		t.Fatalf("expected approved, got %s", change.Status)
	}
	credential, err := svc.IssuePassport(change.ID, "usr_reviewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: strings.Repeat("0", 64), Environment: change.Environment, Consumer: "tampered-ci"}, true); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("passport must reject a different artifact digest without consuming it, got %v", err)
	}
	if _, err = svc.IssuePassport(change.ID, "usr_reviewer", 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("a second active passport must not replace the first non-atomically, got %v", err)
	}
	gate, err := svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, Consumer: "test-ci"}, true)
	if err != nil || !gate.Allowed {
		t.Fatalf("CI should consume the passport: gate=%+v err=%v", gate, err)
	}
	replay, err := svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, Consumer: "test-ci"}, true)
	if err != nil || !replay.Allowed || !replay.Replayed || replay.Passport == nil || replay.Passport.ConsumedBy != "test-ci" {
		t.Fatalf("same consumer must replay first consume: gate=%+v err=%v", replay, err)
	}
	if gate.Passport == nil || replay.Passport.ConsumedAt == nil || gate.Passport.ConsumedAt == nil || !replay.Passport.ConsumedAt.Equal(*gate.Passport.ConsumedAt) {
		t.Fatalf("replay mutated consume timestamps: first=%+v replay=%+v", gate.Passport, replay.Passport)
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, Consumer: "test-ci-replay"}, true); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("consumed passport must reject a different consumer, got %v", err)
	}
	consumeAudits := 0
	for _, event := range data.AuditsByChange(change.OrganizationID, change.ID) {
		if event.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumeAudits++
		}
	}
	if consumeAudits != 1 {
		t.Fatalf("service replay must not write a second consume audit, got %d", consumeAudits)
	}
	change, err = svc.Change(change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusCompleted {
		t.Fatalf("CI consumption must atomically complete the change, got %s", change.Status)
	}
	if _, err = svc.Complete(change.ID, "usr_developer"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("manual completion must be disabled, got %v", err)
	}
}

func TestApprovedChangeCanBeEditedAndInvalidatesPassport(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("e", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	input := model.CreateChangeInput{
		Title: "订单服务安全配置", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml Diff", Source: "config/application.yaml", Language: "YAML", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "恢复上一版本配置并重新加载服务",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx", "P99 延迟", "下单成功率"}},
	}
	change, err := svc.Create(input, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil || change.Status != model.StatusWaitingApproval {
		t.Fatalf("safe config must reach approval: change=%+v err=%v", change, err)
	}
	change, err = svc.Approve(change.ID, "usr_reviewer", "配置与回滚方案已核对")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := svc.IssuePassport(change.ID, "usr_reviewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := change.ArtifactSHA256
	input.Artifacts[0].Content = "debug: false\nauth_enabled: true\ntls_verify: true\nrequest_timeout_ms: 3000"
	updated, err := svc.Update(change.ID, input, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.StatusDraft || updated.ReviewerID != "" || updated.ArtifactSHA256 == oldDigest {
		t.Fatalf("editing an approved change must reset approval and digest: %+v", updated)
	}
	passports := data.PassportsByChange(updated.OrganizationID, updated.ID)
	if len(passports) != 1 || passports[0].Status != model.PassportRevoked {
		t.Fatalf("editing an approved change must revoke its active passport: %+v", passports)
	}
	if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: oldDigest, Environment: change.Environment, Consumer: "stale-ci"}, true); !errors.Is(err, ErrPassportRevoked) {
		t.Fatalf("old passport must stay unusable after the approved change is edited, got %v", err)
	}
}
func TestUnifiedArtifactChangeCanEnterPreReleaseValidation(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title:         "订单服务配置分批发布",
		ApplicationID: "app_order",
		ChangeType:    "配置变更",
		Environment:   "生产环境",
		RepositoryURL: "https://git.example.com/commerce/order-service",
		Branch:        "release/v2.8",
		CommitSHA:     "8f2c1ab",
		Description:   "关闭调试开关并调整订单超时配置。",
		Artifacts: []model.ChangeArtifact{{
			Kind:     model.ArtifactConfig,
			Name:     "application.yaml Diff",
			Source:   "config/application.yaml",
			Language: "YAML",
			Content:  "debug: false\norder_timeout_ms: 3000",
		}},
		RollbackPlan: "恢复上一版本配置并重新加载订单服务。",
		ReleasePlan: model.ReleasePlan{
			Strategy:           "金丝雀发布",
			CanaryPercent:      10,
			ObservationMinutes: 15,
			AutoRollback:       true,
			SuccessMetrics:     []string{"HTTP 5xx", "P99 延迟", "下单成功率"},
		},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Artifacts) != 1 || change.Artifacts[0].Kind != model.ArtifactConfig {
		t.Fatalf("unexpected artifacts: %+v", change.Artifacts)
	}
	if change.ReleasePlan.Strategy != "金丝雀发布" || change.RepositoryURL == "" {
		t.Fatalf("unified release metadata was not persisted: %+v", change)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusWaitingApproval {
		t.Fatalf("expected deterministic checks to move directly to approval, got %s with findings %+v", change.Status, change.Findings)
	}
}

func TestHighRiskFindingRequiresAssignmentResolutionAndVerification(t *testing.T) {
	t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("h", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, 1)
	change, err := svc.Create(model.CreateChangeInput{
		Title: "清理历史归档表", ApplicationID: "app_order", ChangeType: "DDL",
		SQL:         "DROP TABLE order_archive_2020;",
		RollbackSQL: "CREATE TABLE order_archive_2020(id bigint primary key);",
		Description: "释放历史归档空间",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusCheckFailed {
		t.Fatalf("expected check failed, got %s", change.Status)
	}
	if len(change.Findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	findingID := change.Findings[0].ID
	dueAt := time.Now().Add(48 * time.Hour)
	change, err = svc.AssignFinding(change.ID, findingID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: dueAt})
	if err != nil {
		t.Fatal(err)
	}
	if change.Findings[0].Status != model.FindingAssigned {
		t.Fatalf("expected assigned, got %s", change.Findings[0].Status)
	}
	change, err = svc.ResolveFinding(change.ID, findingID, "usr_developer", model.ResolveFindingInput{Resolution: "已完成全量备份与恢复演练，并安排低峰窗口执行"})
	if err != nil {
		t.Fatal(err)
	}
	if change.Findings[0].Status != model.FindingResolved {
		t.Fatalf("expected resolved, got %s", change.Findings[0].Status)
	}
	change, err = svc.VerifyFinding(change.ID, findingID, "usr_reviewer", model.VerifyFindingInput{Approved: true, Comment: "备份和恢复证据已核对"})
	if err != nil {
		t.Fatal(err)
	}
	if change.Findings[0].Status != model.FindingVerified {
		t.Fatalf("expected verified, got %s", change.Findings[0].Status)
	}
	if change.Status != model.StatusReadyForExperiment || change.CheckRun == nil || change.CheckRun.Status != "PASSED" || change.CheckRun.Blocking != 0 {
		t.Fatalf("expected refreshed check run ready for experiment, got %+v", change)
	}
	if _, err = svc.QueueExperiment(change.ID, "usr_developer"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		change, err = svc.Change(change.ID)
		if err != nil {
			t.Fatal(err)
		}
		if change.Status == model.StatusWaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if change.Status != model.StatusWaitingApproval {
		t.Fatalf("real bound SQL report did not reach approval: %+v", change.Experiment)
	}
	change, err = svc.Approve(change.ID, "usr_owner", "影子演练与整改证据完整")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.IssuePassport(change.ID, "usr_owner", 0); err != nil {
		t.Fatalf("remediated SQL change should receive a passport: %v", err)
	}
}

func TestRiskPolicyLifecycleAndPermissions(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	input := model.SaveRiskPolicyInput{
		Code: "SELECT_STAR", Name: "禁止查询全部字段",
		Description: "生产查询不得直接使用星号，避免字段漂移和无效传输。",
		Pattern:     `(?i)\bSELECT\s+\*\s+FROM\b`, Severity: model.RiskMedium,
		Blocking: true, Enabled: true, Environments: []string{"生产环境"},
		ChangeTypes: []string{"DML"}, Suggestion: "显式列出需要的字段。",
	}
	if _, err := svc.CreatePolicy(input, "usr_developer"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer should not create policy, got %v", err)
	}
	policy, err := svc.CreatePolicy(input, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Version != 1 || policy.Builtin {
		t.Fatalf("unexpected created policy: %#v", policy)
	}
	result, err := svc.TestPolicies(model.TestRiskPolicyInput{
		SQL: "SELECT * FROM orders;", RollbackSQL: "SELECT id FROM orders;",
		Environment: "生产环境", ChangeType: "DML",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range result.Findings {
		if finding.Code == "SELECT_STAR" {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom policy did not participate in test: %#v", result.Findings)
	}
	toggled, err := svc.TogglePolicy(policy.ID, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	if toggled.Enabled {
		t.Fatal("expected policy to be disabled")
	}
	result, err = svc.TestPolicies(model.TestRiskPolicyInput{
		SQL: "SELECT * FROM orders;", RollbackSQL: "SELECT id FROM orders;",
		Environment: "生产环境", ChangeType: "DML",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range result.Findings {
		if finding.Code == "SELECT_STAR" {
			t.Fatalf("disabled custom policy still matched: %#v", finding)
		}
	}
}

func TestEnterpriseTenantIsolation(t *testing.T) {
	data := store.NewMemory()
	now := time.Now()
	organization := model.Organization{
		ID: "org_isolated", Name: "隔离测试企业", Slug: "isolated",
		EmailDomains: []string{"isolated.example"}, CreatedBy: "usr_isolated",
		CreatedAt: now, UpdatedAt: now,
	}
	user := model.User{
		ID: "usr_isolated", OrganizationID: organization.ID, OrganizationName: organization.Name,
		Name: "隔离用户", Email: "owner@isolated.example", Role: model.RoleOwner,
		EnterpriseAdmin: true, Active: true,
	}
	policies := model.DefaultRiskPolicies(now)
	for index := range policies {
		policies[index].ID = store.NewID("pol_")
		policies[index].OrganizationID = organization.ID
	}
	if err := data.CreateEnterprise(organization, user, model.UserCredential{UserID: user.ID}, policies, model.AuditEvent{
		OrganizationID: organization.ID, ID: "audit_isolated", ActorID: user.ID,
		ActorName: user.Name, Action: "REGISTER_ENTERPRISE", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(data, fakeRunner{}, fakeAnalyzer{})

	changes, err := svc.ChangesFor(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("new enterprise must not see demo changes: %+v", changes)
	}
	demoChange := data.Changes()[0]
	if _, err := svc.ChangeFor(demoChange.ID, user.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-tenant detail read must be forbidden, got %v", err)
	}
	applications, err := svc.ApplicationsFor(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 0 {
		t.Fatalf("new enterprise must not see demo applications: %+v", applications)
	}
	visiblePolicies, err := svc.PoliciesFor(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(visiblePolicies) != len(policies) {
		t.Fatalf("enterprise should only receive its own default policies: got %d want %d", len(visiblePolicies), len(policies))
	}
	audits, err := svc.AuditsFor(user.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, audit := range audits {
		if audit.OrganizationID != organization.ID {
			t.Fatalf("cross-tenant audit leaked: %+v", audit)
		}
	}
}

func TestEnterpriseApplicationOnboarding(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	input := model.SaveApplicationInput{
		Name: "风控中心", Owner: "安全研发组", Database: "risk_db", Schema: "public",
		Environment: "生产 / 只读", Description: "风险策略和命中记录",
	}
	if _, err := svc.CreateApplication(input, "usr_developer"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("developer must not onboard applications, got %v", err)
	}
	application, err := svc.CreateApplication(input, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	if application.OrganizationID != "org_demo" || application.ID == "" {
		t.Fatalf("application tenant metadata missing: %+v", application)
	}
	if _, err := svc.CreateApplication(input, "usr_owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate application must be rejected, got %v", err)
	}
	application, err = svc.UpdateApplication(application.ID, model.SaveApplicationInput{
		Name: "风控平台", Owner: "安全平台组", Database: "risk_db", Schema: "public",
		Environment: "预发布 / 只读", Description: "风险策略、漏洞和命中记录",
	}, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	if application.Name != "风控平台" || application.Owner != "安全平台组" {
		t.Fatalf("application update not persisted: %+v", application)
	}
}

type highRiskAnalyzer struct{}

func (highRiskAnalyzer) Analyze(_ context.Context, _ model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{
		Provider: "test", Risk: model.RiskHigh, Summary: "模型建议按高风险复核",
		GeneratedAt: time.Now(),
	}
}

func TestAgentHighRiskDoesNotEscalateGovernanceApproval(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, highRiskAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "安全配置调整", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
		RollbackPlan: "恢复上一版本配置并重新加载服务",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx", "P99 延迟"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusWaitingApproval {
		t.Fatalf("deterministic gate should reach approval, got %s", change.Status)
	}
	if change.Risk == model.RiskHigh {
		t.Fatalf("model HIGH must not escalate governance risk, got %s", change.Risk)
	}
	if change.Analysis == nil || change.Analysis.AdvisoryRisk != model.RiskHigh || change.Analysis.Risk != model.RiskHigh {
		t.Fatalf("AI HIGH should be retained only as advisory risk with legacy alias: %+v", change.Analysis)
	}
	approved, err := svc.Approve(change.ID, "usr_reviewer", "确定性门禁证据已核对")
	if err != nil {
		t.Fatalf("ordinary reviewer should approve based on governance risk, got %v", err)
	}
	if approved.Status != model.StatusApproved {
		t.Fatalf("expected approved, got %s", approved.Status)
	}
}

type lowRiskAnalyzer struct{}

func (lowRiskAnalyzer) Analyze(_ context.Context, _ model.ChangeRequest) model.AgentAnalysis {
	return model.AgentAnalysis{
		Provider: "test", Risk: model.RiskLow, Summary: "模型错误地判断为低风险",
		GeneratedAt: time.Now(),
	}
}

func TestAgentCannotDowngradeDeterministicRisk(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, lowRiskAnalyzer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx, 1)

	change, err := svc.Create(model.CreateChangeInput{
		Title: "删除历史归档表", ApplicationID: "app_order", ChangeType: "DDL",
		SQL:         "DROP TABLE order_archive_2019;",
		RollbackSQL: "CREATE TABLE order_archive_2019(id bigint primary key);",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Risk != model.RiskHigh || len(change.Findings) == 0 {
		t.Fatalf("expected deterministic high risk findings: %+v", change)
	}
	for _, finding := range change.Findings {
		if !isBlockingFinding(finding) {
			continue
		}
		change, err = svc.AssignFinding(change.ID, finding.ID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		change, err = svc.ResolveFinding(change.ID, finding.ID, "usr_developer", model.ResolveFindingInput{Resolution: "已完成备份、依赖核对和恢复演练"})
		if err != nil {
			t.Fatal(err)
		}
		change, err = svc.VerifyFinding(change.ID, finding.ID, "usr_reviewer", model.VerifyFindingInput{Approved: true, Comment: "整改证据已核对"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = svc.QueueExperiment(change.ID, "usr_developer"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		change, err = svc.Change(change.ID)
		if err != nil {
			t.Fatal(err)
		}
		if change.Status == model.StatusWaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if change.Status != model.StatusWaitingApproval {
		t.Fatalf("experiment did not complete: %s", change.Status)
	}
	if change.Risk != model.RiskHigh {
		t.Fatalf("agent must not downgrade deterministic risk, got %s", change.Risk)
	}
	if change.Analysis == nil || change.Analysis.AdvisoryRisk != model.RiskLow || change.Analysis.Risk != model.RiskLow {
		t.Fatalf("model LOW should remain an advisory value only: %+v", change.Analysis)
	}
	if _, err = svc.Approve(change.ID, "usr_reviewer", "尝试由数据库审核人放行"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("model LOW must not lower deterministic HIGH approval requirement, got %v", err)
	}
}

func TestMediumBlockingFindingCanReachExperiment(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	policy, err := svc.CreatePolicy(model.SaveRiskPolicyInput{
		Code: "SELECT_REQUIRES_REVIEW", Name: "查询变更需整改说明",
		Description: "生产环境查询类变更必须补充影响范围和执行依据。",
		Pattern:     "(?i)SELECT\\s+", Severity: model.RiskMedium, Blocking: true, Enabled: true,
		Environments: []string{"生产环境"}, ChangeTypes: []string{"DML"}, Suggestion: "补充影响说明。",
	}, "usr_owner")
	if err != nil || policy.ID == "" {
		t.Fatalf("create policy: %v", err)
	}
	change, err := svc.Create(model.CreateChangeInput{
		Title: "查询语句核查", ApplicationID: "app_order", ChangeType: "DML",
		SQL: "SELECT id FROM orders;", RollbackSQL: "SELECT 1;", Environment: "生产环境",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusCheckFailed {
		t.Fatalf("expected blocking status, got %s", change.Status)
	}
	var findingID string
	for _, finding := range change.Findings {
		if finding.Code == policy.Code {
			findingID = finding.ID
		}
	}
	if findingID == "" {
		t.Fatal("custom blocking finding missing")
	}
	change, err = svc.AssignFinding(change.ID, findingID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.ResolveFinding(change.ID, findingID, "usr_developer", model.ResolveFindingInput{Resolution: "已补充查询影响范围和验证记录"})
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.VerifyFinding(change.ID, findingID, "usr_reviewer", model.VerifyFindingInput{Approved: true, Comment: "证据完整"})
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != model.StatusReadyForExperiment {
		t.Fatalf("verified medium blocking finding should unblock workflow, got %s", change.Status)
	}
}

func TestFindingAssignmentRejectsPastDueAndInactiveOwner(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "删除旧字段", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "ALTER TABLE orders DROP COLUMN note;", RollbackSQL: "ALTER TABLE orders ADD COLUMN note varchar(255);",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	findingID := change.Findings[0].ID
	if _, err = svc.AssignFinding(change.ID, findingID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(-time.Minute)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("past due date must be rejected, got %v", err)
	}
	_, err = data.UpdateMember("org_demo", "usr_developer", func(user *model.User) error { user.Active = false; return nil }, nil, model.AuditEvent{OrganizationID: "org_demo", ID: "audit_disable", ActorID: "usr_owner", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AssignFinding(change.ID, findingID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("inactive owner must be rejected, got %v", err)
	}
}

func TestFindingOwnerMustHaveApplicationSubmitAccess(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "删除旧字段", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "ALTER TABLE orders DROP COLUMN note;", RollbackSQL: "ALTER TABLE orders ADD COLUMN note varchar(255);",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	change, err = svc.Submit(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	_, err = data.UpdateMember("org_demo", "usr_reviewer", func(user *model.User) error { return nil }, []model.ApplicationGrantInput{{ApplicationID: "app_order", CanReview: true}}, model.AuditEvent{OrganizationID: "org_demo", ID: "audit_review_grant", ActorID: "usr_owner", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = data.UpdateMember("org_demo", "usr_developer", func(user *model.User) error { return nil }, []model.ApplicationGrantInput{{ApplicationID: "app_inventory", CanSubmit: true}}, model.AuditEvent{OrganizationID: "org_demo", ID: "audit_submit_grant", ActorID: "usr_owner", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AssignFinding(change.ID, change.Findings[0].ID, "usr_reviewer", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("assignee without application submit access must be rejected, got %v", err)
	}
}

func TestChangeAuditsAreCompleteAndChronological(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	change, err := svc.Create(model.CreateChangeInput{
		Title: "索引优化", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "CREATE INDEX idx_orders_status ON orders(status);", RollbackSQL: "DROP INDEX idx_orders_status;",
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AddComment(change.ID, "usr_developer", "已补充慢查询分析结果"); err != nil {
		t.Fatal(err)
	}
	audits, err := svc.AuditsForChange(change.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) < 2 {
		t.Fatalf("expected full audit trail, got %d", len(audits))
	}
	for i, item := range audits {
		if item.ChangeID != change.ID {
			t.Fatalf("unexpected change audit: %#v", item)
		}
		if i > 0 && item.CreatedAt.Before(audits[i-1].CreatedAt) {
			t.Fatal("audit trail is not chronological")
		}
	}
}

func TestCreateRejectsPastExecutionWindow(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	_, err := svc.Create(model.CreateChangeInput{
		Title: "历史窗口", ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "CREATE INDEX idx_old_window ON orders(status);", RollbackSQL: "DROP INDEX idx_old_window;",
		PlannedAt: time.Now().Add(-time.Hour),
	}, "usr_developer")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("past execution window must be rejected, got %v", err)
	}
}

func TestRetryConcurrentWrite(t *testing.T) {
	attempts := 0
	err := retryConcurrentWrite(func() error {
		attempts++
		if attempts < 3 {
			return store.ErrConcurrentWrite
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("concurrent write should be retried: attempts=%d err=%v", attempts, err)
	}
}

func TestCreateRejectsOversizedBusinessFields(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	_, err := svc.Create(model.CreateChangeInput{
		Title: strings.Repeat("超", 121), ApplicationID: "app_order", ChangeType: "DDL",
		SQL: "CREATE INDEX idx_too_long_title ON orders(status);", RollbackSQL: "DROP INDEX idx_too_long_title;",
	}, "usr_developer")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized title must be rejected, got %v", err)
	}
}

func TestReleaseWindowFindingsCoverSameServiceAndDependencies(t *testing.T) {
	now := time.Now()
	applications := []model.Application{
		{ID: "app_order", Name: "订单服务", Dependencies: []string{"app_inventory"}},
		{ID: "app_inventory", Name: "库存服务"},
		{ID: "app_gateway", Name: "开放平台网关", Dependencies: []string{"app_order"}},
		{ID: "app_unrelated", Name: "文件中心"},
	}
	current := model.ChangeRequest{OrganizationID: "org_demo", ID: "chg_current", ApplicationID: "app_order", ApplicationName: "订单服务", PlannedAt: now.Add(4 * time.Hour), Status: model.StatusDraft}
	changes := []model.ChangeRequest{
		current,
		{OrganizationID: "org_demo", ID: "chg_same", Title: "订单服务灰度发布", ApplicationID: "app_order", ApplicationName: "订单服务", PlannedAt: current.PlannedAt.Add(-30 * time.Minute), Status: model.StatusWaitingApproval},
		{OrganizationID: "org_demo", ID: "chg_upstream", Title: "库存预占逻辑发布", ApplicationID: "app_inventory", ApplicationName: "库存服务", PlannedAt: current.PlannedAt.Add(40 * time.Minute), Status: model.StatusApproved},
		{OrganizationID: "org_demo", ID: "chg_downstream", Title: "网关路由发布", ApplicationID: "app_gateway", ApplicationName: "开放平台网关", PlannedAt: current.PlannedAt.Add(-50 * time.Minute), Status: model.StatusReadyForExperiment},
		{OrganizationID: "org_demo", ID: "chg_terminal", Title: "已完成订单发布", ApplicationID: "app_order", PlannedAt: current.PlannedAt, Status: model.StatusCompleted},
		{OrganizationID: "org_demo", ID: "chg_unrelated", Title: "文件扫描发布", ApplicationID: "app_unrelated", PlannedAt: current.PlannedAt, Status: model.StatusApproved},
		{OrganizationID: "org_demo", ID: "chg_far", Title: "远期订单发布", ApplicationID: "app_order", PlannedAt: current.PlannedAt.Add(2 * time.Hour), Status: model.StatusApproved},
	}

	findings, codes := releaseWindowFindings(current, applications, changes, model.DefaultRiskPolicies(now))
	if len(findings) != 3 {
		t.Fatalf("expected 3 window findings, got %d: %+v", len(findings), findings)
	}
	if len(codes) != 2 {
		t.Fatalf("expected 2 matched policy codes, got %+v", codes)
	}
	var sameService, dependency int
	for _, finding := range findings {
		switch finding.Code {
		case "CHANGE_WINDOW_CONFLICT":
			sameService++
			if !finding.Blocking || finding.Severity != model.RiskHigh {
				t.Fatalf("same-service conflict must block: %+v", finding)
			}
		case "DEPENDENCY_WINDOW_OVERLAP":
			dependency++
			if finding.Blocking || finding.Severity != model.RiskMedium {
				t.Fatalf("dependency overlap must only warn: %+v", finding)
			}
		}
	}
	if sameService != 1 || dependency != 2 {
		t.Fatalf("unexpected conflict distribution: same=%d dependency=%d", sameService, dependency)
	}
}

func TestSubmitBlocksCollidingServiceWindow(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	baseInput := model.CreateChangeInput{
		ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		RepositoryURL: "https://git.example.com/commerce/order-service", Branch: "release/test", CommitSHA: "abc1234",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "feature_enabled: true\nrequest_timeout_ms: 3000"}},
		RollbackPlan: "切回上一版本静态资源并清理 CDN 缓存。",
		ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"前端错误率", "保存成功率"}},
		Description:  "验证同一服务发布窗口互斥。",
	}
	firstInput := baseInput
	firstInput.Title = "运营台第一批发布"
	firstInput.PlannedAt = time.Now().Add(72 * time.Hour)
	first, err := svc.Create(firstInput, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	first, err = svc.Submit(first.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != model.StatusWaitingApproval {
		t.Fatalf("first release should pass deterministic checks, got %s: %+v", first.Status, first.Findings)
	}

	secondInput := baseInput
	secondInput.Title = "运营台第二批发布"
	secondInput.PlannedAt = first.PlannedAt.Add(30 * time.Minute)
	second, err := svc.Create(secondInput, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	second, err = svc.Submit(second.ID, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != model.StatusCheckFailed {
		t.Fatalf("colliding release must be blocked, got %s: %+v", second.Status, second.Findings)
	}
	found := false
	for _, finding := range second.Findings {
		if finding.Code == "CHANGE_WINDOW_CONFLICT" && finding.Blocking {
			found = true
		}
	}
	if !found {
		t.Fatalf("same-service window finding missing: %+v", second.Findings)
	}
}
func TestApplicationDependenciesUseTenantIDsAndRejectCycles(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	base := model.SaveApplicationInput{
		Name: "订单服务", Owner: "刘丰熙", Kind: "后端服务", Runtime: "Go 1.26",
		RepositoryURL: "https://git.example.com/commerce/order-service", Tier: "核心",
		Lifecycle: "生产运行", Database: "commerce", Schema: "public",
		Environment: "生产 / Kubernetes", Description: "订单创建、状态流转与幂等控制",
	}
	base.Dependencies = []string{"missing_service"}
	if _, err := svc.UpdateApplication("app_order", base, "usr_owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown dependency must be rejected, got %v", err)
	}
	base.Dependencies = []string{"app_order"}
	if _, err := svc.UpdateApplication("app_order", base, "usr_owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("self dependency must be rejected, got %v", err)
	}
	cycle := base
	cycle.Name = "库存服务"
	cycle.Owner = "陈嘉"
	cycle.RepositoryURL = "https://git.example.com/commerce/inventory-service"
	cycle.Database = "inventory"
	cycle.Dependencies = []string{"app_order"}
	if _, err := svc.UpdateApplication("app_inventory", cycle, "usr_owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("dependency cycle must be rejected, got %v", err)
	}
	base.Dependencies = []string{"app_inventory", "app_payment"}
	updated, err := svc.UpdateApplication("app_order", base, "usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Dependencies) != 2 || updated.Dependencies[0] != "app_inventory" {
		t.Fatalf("dependency IDs were not persisted: %+v", updated.Dependencies)
	}
}

func TestCreateRejectsUnsupportedV1Artifact(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	_, err := svc.Create(model.CreateChangeInput{
		Title: "代码发布", ApplicationID: "app_order", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactCode, Name: "service.diff", Content: "+panic(\"boom\")"}},
		RollbackPlan: "回退上一版本镜像。",
	}, "usr_developer")
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "DATABASE、CONFIG、KUBERNETES") {
		t.Fatalf("unsupported artifact kind must fail explicitly, got %v", err)
	}
}

func TestCreateRejectsDatabaseArtifactWithoutExecutableSQL(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	_, err := svc.Create(model.CreateChangeInput{
		Title: "数据库制品绕过尝试", ApplicationID: "app_order", Environment: "生产环境",
		Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactDatabase, Name: "migration.sql", Content: "CREATE INDEX idx_orders_id ON orders(id);"}},
		RollbackPlan: "删除索引。",
	}, "usr_developer")
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "可执行 SQL") {
		t.Fatalf("database artifacts must not bypass shadow execution, got %v", err)
	}
}

func TestNonSQLRemediationCanApproveIssueAndConsumePassport(t *testing.T) {
	cases := []struct {
		name       string
		changeType string
		artifact   model.ChangeArtifact
	}{
		{name: "config", changeType: "配置变更", artifact: model.ChangeArtifact{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "api_key: actual-secret\ndebug: false"}},
		{name: "kubernetes", changeType: "Kubernetes 变更", artifact: model.ChangeArtifact{Kind: model.ArtifactKubernetes, Name: "deployment.yaml", Content: "apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n        - name: api\n          image: api:latest\n          securityContext:\n            privileged: true"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("DBGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("r", 32))
			data := store.NewMemory()
			svc := New(data, fakeRunner{}, fakeAnalyzer{})
			change, err := svc.Create(model.CreateChangeInput{
				Title: "整改闭环 " + testCase.name, ApplicationID: "app_order", ChangeType: testCase.changeType, Environment: "生产环境",
				Artifacts: []model.ChangeArtifact{testCase.artifact}, RollbackPlan: "恢复上一稳定版本配置。",
				ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"错误率", "P99 延迟"}},
			}, "usr_developer")
			if err != nil {
				t.Fatal(err)
			}
			change, err = svc.Submit(change.ID, "usr_developer")
			if err != nil || change.Status != model.StatusCheckFailed {
				t.Fatalf("unsafe artifact must be blocked: status=%s err=%v findings=%+v", change.Status, err, change.Findings)
			}
			for _, finding := range append([]model.Finding(nil), change.Findings...) {
				if !isBlockingFinding(finding) {
					continue
				}
				change, err = svc.AssignFinding(change.ID, finding.ID, "usr_owner", model.AssignFindingInput{OwnerID: "usr_developer", DueAt: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				change, err = svc.ResolveFinding(change.ID, finding.ID, "usr_developer", model.ResolveFindingInput{Resolution: "已替换为安全配置并补充复核证据"})
				if err != nil {
					t.Fatal(err)
				}
				change, err = svc.VerifyFinding(change.ID, finding.ID, "usr_owner", model.VerifyFindingInput{Approved: true, Comment: "整改证据已核对"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if change.Status != model.StatusWaitingApproval || change.CheckRun == nil || change.CheckRun.Status != "PASSED" || change.CheckRun.Blocking != 0 {
				t.Fatalf("remediation must refresh check run and enter approval: %+v", change)
			}
			change, err = svc.Approve(change.ID, "usr_owner", "确定性检查与整改证据完整")
			if err != nil {
				t.Fatal(err)
			}
			credential, err := svc.IssuePassport(change.ID, "usr_owner", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.VerifyGate(model.GateRequest{Token: credential.Token, ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment, Consumer: "ci-" + testCase.name}, true); err != nil {
				t.Fatal(err)
			}
			completed, err := svc.Change(change.ID)
			if err != nil || completed.Status != model.StatusCompleted {
				t.Fatalf("CI consume must complete remediated change: %+v err=%v", completed, err)
			}
		})
	}
}
