package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liufengxi/dbguard/internal/agent"
	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/checker"
	"github.com/liufengxi/dbguard/internal/conflict"
	"github.com/liufengxi/dbguard/internal/experiment"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/report"
	"github.com/liufengxi/dbguard/internal/store"
)

var (
	ErrInvalidState = errors.New("当前状态不允许执行该操作")
	ErrForbidden    = errors.New("没有权限执行该操作")
	ErrValidation   = errors.New("请求参数不完整")
)

type Event struct {
	OrganizationID string             `json:"organization_id"`
	Type           string             `json:"type"`
	ChangeID       string             `json:"change_id"`
	Status         model.ChangeStatus `json:"status"`
	Message        string             `json:"message"`
	At             time.Time          `json:"at"`
}

type Service struct {
	store          *store.Store
	runner         experiment.Runner
	analyzer       agent.Analyzer
	queue          chan string
	subMu          sync.RWMutex
	subs           map[chan Event]struct{}
	passportSigner *changegate.Signer
	passportTTL    time.Duration
	evidenceTools  *EvidenceQueryRegistry
}

func New(data *store.Store, runner experiment.Runner, analyzer agent.Analyzer) *Service {
	signer, ttl := passportSignerFromEnvironment()
	return &Service{store: data, runner: runner, analyzer: analyzer, queue: make(chan string, 64), subs: make(map[chan Event]struct{}), passportSigner: signer, passportTTL: ttl, evidenceTools: defaultEvidenceQueryRegistry(data)}
}

func (s *Service) Start(ctx context.Context, workers int) {
	if workers <= 0 {
		return
	}
	_ = s.store.EnsureExperimentOutbox()
	for i := 0; i < workers; i++ {
		go s.worker(ctx, fmt.Sprintf("worker-%02d-%s", i+1, store.NewID("")))
	}
	select {
	case s.queue <- "startup":
	default:
	}
}

func (s *Service) Dashboard() model.Dashboard                    { return s.store.Dashboard() }
func (s *Service) Applications() []model.Application             { return s.store.Applications() }
func (s *Service) Users() []model.User                           { return s.store.Users() }
func (s *Service) Changes() []model.ChangeRequest                { return s.store.Changes() }
func (s *Service) Change(id string) (model.ChangeRequest, error) { return s.store.Change(id) }
func (s *Service) Audits(limit int) []model.AuditEvent           { return s.store.Audits(limit) }
func (s *Service) Policies() []model.RiskPolicy                  { return s.store.Policies() }

func (s *Service) DashboardFor(actorID string) (model.Dashboard, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.Dashboard{}, err
	}
	changes := s.store.ChangesByOrganization(actor.OrganizationID)
	visible := make([]model.ChangeRequest, 0, len(changes))
	for _, change := range changes {
		if s.canUseApplication(actor, change.ApplicationID, "view") {
			visible = append(visible, change)
		}
	}
	return dashboardForChanges(visible), nil
}

func (s *Service) GovernanceOutcomesFor(actorID string, windowDays int, now time.Time) (model.GovernanceOutcomeSummary, error) {
	if windowDays < 1 || windowDays > 365 {
		return model.GovernanceOutcomeSummary{}, ErrValidation
	}
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.GovernanceOutcomeSummary{}, err
	}
	changes, err := s.ChangesFor(actorID)
	if err != nil {
		return model.GovernanceOutcomeSummary{}, err
	}
	allowed := make(map[string]bool, len(changes))
	for _, change := range changes {
		allowed[change.ID] = true
	}
	events := s.store.IntegrationEvents(actor.OrganizationID, 0)
	visibleEvents := make([]model.IntegrationEvent, 0, len(events))
	for _, event := range events {
		if allowed[event.ChangeID] {
			visibleEvents = append(visibleEvents, event)
		}
	}
	signals := s.store.OutcomeSignals(actor.OrganizationID, 0)
	visibleSignals := make([]model.OutcomeSignal, 0, len(signals))
	for _, signal := range signals {
		if allowed[signal.ChangeID] {
			visibleSignals = append(visibleSignals, signal)
		}
	}
	return governanceOutcomesForEvidence(changes, visibleEvents, visibleSignals, windowDays, now, "actor_application_access"), nil
}

func (s *Service) GlobalGovernanceOutcomes(windowDays int, now time.Time) model.GovernanceOutcomeSummary {
	if windowDays < 1 || windowDays > 365 {
		windowDays = 30
	}
	return governanceOutcomesForEvidence(s.store.Changes(), s.store.IntegrationEvents("", 0), s.store.OutcomeSignals("", 0), windowDays, now, "global_operations")
}

func (s *Service) ApplicationsFor(actorID string) ([]model.Application, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	applications := s.store.ApplicationsByOrganization(actor.OrganizationID)
	visible := make([]model.Application, 0, len(applications))
	for _, application := range applications {
		if s.canUseApplication(actor, application.ID, "view") {
			visible = append(visible, application)
		}
	}
	return visible, nil
}

func (s *Service) UsersFor(actorID string) ([]model.User, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	return s.store.UsersByOrganization(actor.OrganizationID), nil
}

func (s *Service) ChangesFor(actorID string) ([]model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	changes := s.store.ChangesByOrganization(actor.OrganizationID)
	visible := make([]model.ChangeRequest, 0, len(changes))
	for _, change := range changes {
		if s.canUseApplication(actor, change.ApplicationID, "view") {
			visible = append(visible, change)
		}
	}
	return visible, nil
}

func (s *Service) ChangeFor(id, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "view") {
		return model.ChangeRequest{}, ErrForbidden
	}
	return change, nil
}

func (s *Service) AuditsFor(actorID string, limit int) ([]model.AuditEvent, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	audits := s.store.AuditsByOrganization(actor.OrganizationID, 0)
	if actor.EnterpriseAdmin || actor.Role == model.RoleOwner || !s.store.HasApplicationGrants(actor.OrganizationID) {
		if limit > 0 && len(audits) > limit {
			audits = audits[:limit]
		}
		return audits, nil
	}
	allowedChanges := make(map[string]bool)
	for _, change := range s.store.ChangesByOrganization(actor.OrganizationID) {
		if s.canUseApplication(actor, change.ApplicationID, "view") {
			allowedChanges[change.ID] = true
		}
	}
	visible := make([]model.AuditEvent, 0, len(audits))
	for _, event := range audits {
		if event.ChangeID != "" && allowedChanges[event.ChangeID] {
			visible = append(visible, event)
		}
		if limit > 0 && len(visible) >= limit {
			break
		}
	}
	return visible, nil
}

func (s *Service) AuditsForChange(changeID, actorID string) ([]model.AuditEvent, error) {
	change, err := s.ChangeFor(changeID, actorID)
	if err != nil {
		return nil, err
	}
	return s.store.AuditsByChange(change.OrganizationID, change.ID), nil
}

// AuditMonthlyReport 渲染指定自然月的审计月报 HTML。可见范围与 AuditsFor
// 完全一致（组织隔离 + 应用授权过滤），月度归档给合规部门用。
func (s *Service) AuditMonthlyReport(actorID string, month time.Time) ([]byte, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	events, err := s.AuditsFor(actorID, 0)
	if err != nil {
		return nil, err
	}
	return report.MonthlyAudit(report.MonthlyAuditInput{
		OrganizationName: actor.OrganizationName,
		GeneratedBy:      actor.Name,
		GeneratedRole:    actor.Role,
		GeneratedAt:      time.Now().UTC(),
		Month:            month,
		Audits:           events,
	})
}

// GovernanceTrendsFor 按月聚合当前用户可见变更的治理趋势。
// 可见范围与变更列表一致（组织隔离 + 应用授权过滤）。
func (s *Service) GovernanceTrendsFor(actorID string, months int) ([]model.GovernanceTrendMonth, error) {
	changes, err := s.ChangesFor(actorID)
	if err != nil {
		return nil, err
	}
	return governanceTrends(changes, months, time.Now().UTC()), nil
}

func (s *Service) ConflictRadarFor(actorID string, from, to time.Time) (model.ConflictRadar, error) {
	changes, err := s.ChangesFor(actorID)
	if err != nil {
		return model.ConflictRadar{}, err
	}
	applications, err := s.ApplicationsFor(actorID)
	if err != nil {
		return model.ConflictRadar{}, err
	}
	return conflict.Detect(changes, applications, from, to), nil
}

func (s *Service) IntegrationEventsFor(actorID string, limit int) ([]model.IntegrationEvent, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	return s.store.IntegrationEvents(actor.OrganizationID, limit), nil
}

func (s *Service) RecordIntegrationEvent(event model.IntegrationEvent) (model.IntegrationEvent, bool, error) {
	event.Provider = strings.ToUpper(strings.TrimSpace(event.Provider))
	event.Status = strings.ToUpper(strings.TrimSpace(event.Status))
	event.EventType = strings.ToUpper(strings.TrimSpace(event.EventType))
	event.OrganizationID = strings.TrimSpace(event.OrganizationID)
	if event.OrganizationID == "" || (event.Provider != "GITLAB" && event.Provider != "JENKINS") {
		return model.IntegrationEvent{}, false, ErrValidation
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	} else {
		event.ReceivedAt = event.ReceivedAt.UTC()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = event.ReceivedAt
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if event.ID == "" {
		event.ID = store.NewID("int")
	}
	if event.ChangeID != "" {
		change, err := s.store.Change(event.ChangeID)
		if err != nil || change.OrganizationID != event.OrganizationID {
			event.ChangeID = ""
		}
	}
	if event.ChangeID == "" && event.CommitSHA != "" {
		for _, change := range s.store.ChangesByOrganization(event.OrganizationID) {
			if strings.EqualFold(strings.TrimSpace(change.CommitSHA), strings.TrimSpace(event.CommitSHA)) {
				event.ChangeID = change.ID
				break
			}
		}
	}
	if event.ExternalID == "" {
		event.ExternalID = strings.Join([]string{
			event.Provider, event.Project, event.Pipeline, event.Status, event.CommitSHA,
		}, ":")
	}
	if event.Detail == "" {
		if event.ChangeID == "" {
			event.Detail = "已接收流水线事件，但未关联到变更单"
		} else {
			event.Detail = "流水线事件已关联到变更单 " + event.ChangeID
		}
	}
	action := "GITLAB_PIPELINE_RECEIVED"
	if event.Provider == "JENKINS" {
		action = "JENKINS_BUILD_RECEIVED"
	}
	audit := model.AuditEvent{
		OrganizationID: event.OrganizationID,
		ID:             store.NewID("aud"),
		ChangeID:       event.ChangeID,
		ActorID:        "integration_" + strings.ToLower(event.Provider),
		ActorName:      event.Provider + " CI",
		Action:         action,
		Detail:         event.Detail,
		CreatedAt:      event.ReceivedAt,
	}
	recorded, created, err := s.store.RecordIntegrationEvent(event, audit)
	return recorded, created, err
}

func (s *Service) PoliciesFor(actorID string) ([]model.RiskPolicy, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	return s.store.PoliciesByOrganization(actor.OrganizationID), nil
}

func (s *Service) ActorFor(actorID string) (model.User, error) { return s.activeActor(actorID) }

func (s *Service) activeActor(actorID string) (model.User, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active {
		return model.User{}, ErrForbidden
	}
	return actor, nil
}

func (s *Service) CreateApplication(input model.SaveApplicationInput, actorID string) (model.Application, error) {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return model.Application{}, ErrForbidden
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Database), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Schema), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Owner), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) ||
		!validRuneLength(strings.TrimSpace(input.Environment), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.Description), 0, 2000) {
		return model.Application{}, fmt.Errorf("%w：应用字段为空或长度超出限制", ErrValidation)
	}
	dependencies := normalizeStringList(input.Dependencies)
	if err := s.validateApplicationDependencies(actor.OrganizationID, "", dependencies); err != nil {
		return model.Application{}, err
	}
	application := model.Application{
		OrganizationID: actor.OrganizationID, ID: store.NewID("app_"),
		Name: strings.TrimSpace(input.Name), Owner: strings.TrimSpace(input.Owner),
		Kind: defaultString(input.Kind, "后端服务"), Runtime: defaultString(input.Runtime, "Go"),
		RepositoryURL: strings.TrimSpace(input.RepositoryURL), Tier: defaultString(input.Tier, "重要"),
		Lifecycle: defaultString(input.Lifecycle, "生产运行"),
		Database:  strings.TrimSpace(input.Database), Schema: strings.TrimSpace(input.Schema),
		Environment: strings.TrimSpace(input.Environment), Dependencies: dependencies,
		Tags: normalizeStringList(input.Tags), Description: strings.TrimSpace(input.Description),
	}
	if application.Environment == "" {
		application.Environment = "生产 / 只读"
	}
	err = s.store.CreateApplication(application, audit(actor, "", "CREATE_APPLICATION", "纳管应用 "+application.Name))
	if errors.Is(err, store.ErrConflict) {
		return model.Application{}, fmt.Errorf("%w：应用名称或数据库 Schema 已存在", ErrValidation)
	}
	return application, err
}

func (s *Service) UpdateApplication(id string, input model.SaveApplicationInput, actorID string) (model.Application, error) {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return model.Application{}, ErrForbidden
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Database), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Schema), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Owner), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) ||
		!validRuneLength(strings.TrimSpace(input.Environment), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.Description), 0, 2000) {
		return model.Application{}, fmt.Errorf("%w：应用字段为空或长度超出限制", ErrValidation)
	}
	dependencies := normalizeStringList(input.Dependencies)
	if err := s.validateApplicationDependencies(actor.OrganizationID, id, dependencies); err != nil {
		return model.Application{}, err
	}
	updated, err := s.store.UpdateApplication(actor.OrganizationID, id, func(application *model.Application) error {
		application.Name = strings.TrimSpace(input.Name)
		application.Owner = strings.TrimSpace(input.Owner)
		application.Kind = defaultString(input.Kind, application.Kind)
		application.Runtime = defaultString(input.Runtime, application.Runtime)
		application.RepositoryURL = strings.TrimSpace(input.RepositoryURL)
		application.Tier = defaultString(input.Tier, application.Tier)
		application.Lifecycle = defaultString(input.Lifecycle, application.Lifecycle)
		application.Database = strings.TrimSpace(input.Database)
		application.Schema = strings.TrimSpace(input.Schema)
		application.Environment = strings.TrimSpace(input.Environment)
		application.Dependencies = dependencies
		application.Tags = normalizeStringList(input.Tags)
		application.Description = strings.TrimSpace(input.Description)
		if application.Environment == "" {
			application.Environment = "生产 / 只读"
		}
		return nil
	}, audit(actor, "", "UPDATE_APPLICATION", "更新纳管应用 "+strings.TrimSpace(input.Name)))
	if errors.Is(err, store.ErrConflict) {
		return model.Application{}, fmt.Errorf("%w：应用名称或数据库 Schema 已存在", ErrValidation)
	}
	return updated, err
}

func (s *Service) Create(input model.CreateChangeInput, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	app, err := s.store.Application(input.ApplicationID)
	if err != nil || app.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, app.ID, "submit") {
		return model.ChangeRequest{}, ErrValidation
	}
	title := strings.TrimSpace(input.Title)
	sqlText := strings.TrimSpace(input.SQL)
	rollbackText := strings.TrimSpace(input.RollbackSQL)
	rollbackPlan := strings.TrimSpace(input.RollbackPlan)
	if changegate.Redact(input.SQL) != input.SQL || changegate.Redact(input.RollbackSQL) != input.RollbackSQL {
		return model.ChangeRequest{}, fmt.Errorf("%w：SQL 或回滚 SQL 包含疑似凭据，禁止脱敏后执行；请改用受管密钥引用", ErrValidation)
	}
	artifacts, err := normalizeArtifacts(input.Artifacts, input.SQL)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	environment := defaultString(input.Environment, "生产环境")
	changeType := strings.TrimSpace(input.ChangeType)
	if changeType == "" {
		changeType = deriveChangeType(artifacts)
	}
	sqlSHA256 := changegate.SHA256(input.SQL)
	rollbackSHA256 := changegate.SHA256(input.RollbackSQL)
	artifactSHA256 := changegate.ChangeDigest(environment, changeType, artifacts, sqlSHA256, rollbackSHA256, input.RollbackPlan)
	if hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：DATABASE 制品必须同时提供可执行 SQL，不能绕过 PostgreSQL 影子演练", ErrValidation)
	}
	if !validRuneLength(title, 1, 120) || !validRuneLength(strings.TrimSpace(input.Environment), 0, 64) ||
		!validRuneLength(strings.TrimSpace(input.ChangeType), 0, 64) || !validRuneLength(strings.TrimSpace(input.Description), 0, 4000) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) || !validRuneLength(strings.TrimSpace(input.Branch), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.CommitSHA), 0, 120) || len(sqlText) > 1<<20 || len(rollbackText) > 1<<20 ||
		len(rollbackPlan) > 8000 || artifactBytes(artifacts) > 2<<20 || (sqlText == "" && len(artifacts) == 0) ||
		(hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "") {
		return model.ChangeRequest{}, fmt.Errorf("%w：标题、环境、仓库信息或变更证据不符合要求", ErrValidation)
	}
	now := time.Now()
	if input.PlannedAt.IsZero() {
		input.PlannedAt = now.Add(24 * time.Hour)
	} else if !input.PlannedAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：计划执行时间必须晚于当前时间", ErrValidation)
	}
	change := model.ChangeRequest{
		OrganizationID: actor.OrganizationID, ID: store.NewID("chg_"), Title: strings.TrimSpace(input.Title),
		ApplicationID: app.ID, ApplicationName: app.Name,
		Environment:   environment,
		ChangeType:    changeType,
		RepositoryURL: strings.TrimSpace(input.RepositoryURL), Branch: strings.TrimSpace(input.Branch), CommitSHA: strings.TrimSpace(input.CommitSHA),
		ArtifactSHA256: artifactSHA256, SQLSHA256: sqlSHA256, RollbackSHA256: rollbackSHA256,
		Artifacts: artifacts,
		SQL:       changegate.Redact(sqlText), RollbackSQL: changegate.Redact(rollbackText),
		RollbackPlan: changegate.Redact(rollbackPlan), ReleasePlan: normalizeReleasePlan(input.ReleasePlan),
		Description: changegate.Redact(strings.TrimSpace(input.Description)),
		SubmitterID: actor.ID, SubmitterName: actor.Name,
		Status: model.StatusDraft, Risk: model.RiskUnknown,
		PlannedAt: input.PlannedAt, CreatedAt: now, UpdatedAt: now, Version: 1,
		Timeline: []model.TimelineEntry{timeline(model.StatusDraft, "创建统一变更单", fmt.Sprintf("已登记 %d 类变更证据、回滚方案和计划窗口", len(artifacts)), actor.Name)},
	}
	if err := s.store.CreateChange(change, audit(actor, change.ID, "CREATE", "创建研发发布变更单")); err != nil {
		return model.ChangeRequest{}, err
	}
	s.publish(change, "变更单已创建")
	return change, nil
}

func (s *Service) Update(id string, input model.CreateChangeInput, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || change.SubmitterID != actor.ID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusDraft && change.Status != model.StatusCheckFailed && change.Status != model.StatusRejected && change.Status != model.StatusApproved {
		return model.ChangeRequest{}, ErrInvalidState
	}
	app, err := s.store.Application(input.ApplicationID)
	if err != nil || app.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, app.ID, "submit") {
		return model.ChangeRequest{}, ErrValidation
	}
	title := strings.TrimSpace(input.Title)
	sqlText := strings.TrimSpace(input.SQL)
	rollbackText := strings.TrimSpace(input.RollbackSQL)
	rollbackPlan := strings.TrimSpace(input.RollbackPlan)
	if changegate.Redact(input.SQL) != input.SQL || changegate.Redact(input.RollbackSQL) != input.RollbackSQL {
		return model.ChangeRequest{}, fmt.Errorf("%w：SQL 或回滚 SQL 包含疑似凭据，禁止脱敏后执行；请改用受管密钥引用", ErrValidation)
	}
	artifacts, err := normalizeArtifacts(input.Artifacts, input.SQL)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	environment := defaultString(input.Environment, change.Environment)
	changeType := strings.TrimSpace(input.ChangeType)
	if changeType == "" {
		changeType = deriveChangeType(artifacts)
	}
	sqlSHA256 := changegate.SHA256(input.SQL)
	rollbackSHA256 := changegate.SHA256(input.RollbackSQL)
	artifactSHA256 := changegate.ChangeDigest(environment, changeType, artifacts, sqlSHA256, rollbackSHA256, input.RollbackPlan)
	if hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：DATABASE 制品必须同时提供可执行 SQL，不能绕过 PostgreSQL 影子演练", ErrValidation)
	}
	if !validRuneLength(title, 1, 120) || !validRuneLength(strings.TrimSpace(input.Environment), 0, 64) ||
		!validRuneLength(strings.TrimSpace(input.ChangeType), 0, 64) || !validRuneLength(strings.TrimSpace(input.Description), 0, 4000) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) || !validRuneLength(strings.TrimSpace(input.Branch), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.CommitSHA), 0, 120) || len(sqlText) > 1<<20 || len(rollbackText) > 1<<20 ||
		len(rollbackPlan) > 8000 || artifactBytes(artifacts) > 2<<20 || (sqlText == "" && len(artifacts) == 0) ||
		(hasArtifactKind(artifacts, model.ArtifactDatabase) && sqlText == "") {
		return model.ChangeRequest{}, fmt.Errorf("%w：标题、环境、仓库信息或变更证据不符合要求", ErrValidation)
	}
	now := time.Now()
	if input.PlannedAt.IsZero() {
		input.PlannedAt = change.PlannedAt
	}
	if !input.PlannedAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：计划执行时间必须晚于当前时间", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Title = strings.TrimSpace(input.Title)
		item.ApplicationID = app.ID
		item.ApplicationName = app.Name
		item.Environment = environment
		item.ChangeType = changeType
		item.RepositoryURL = strings.TrimSpace(input.RepositoryURL)
		item.Branch = strings.TrimSpace(input.Branch)
		item.CommitSHA = strings.TrimSpace(input.CommitSHA)
		item.ArtifactSHA256 = artifactSHA256
		item.SQLSHA256 = sqlSHA256
		item.RollbackSHA256 = rollbackSHA256
		item.RuleSetVersion = ""
		item.Artifacts = artifacts
		item.SQL = changegate.Redact(sqlText)
		item.RollbackSQL = changegate.Redact(rollbackText)
		item.RollbackPlan = changegate.Redact(rollbackPlan)
		item.ReleasePlan = normalizeReleasePlan(input.ReleasePlan)
		item.Description = changegate.Redact(strings.TrimSpace(input.Description))
		item.PlannedAt = input.PlannedAt
		item.Status = model.StatusDraft
		item.Risk = model.RiskUnknown
		item.Findings = nil
		item.CheckRun = nil
		item.Experiment = nil
		item.Analysis = nil
		item.ReviewerID = ""
		item.ReviewerName = ""
		item.ReviewComment = ""
		item.Timeline = append(item.Timeline, timeline(model.StatusDraft, "更新变更方案", "SQL、配置、Kubernetes 或回滚方案已修改，历史记录保留", actor.Name))
		return nil
	}, audit(actor, id, "UPDATE", "修改变更内容并重置待检查状态"))
	if err == nil {
		_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "变更内容修改，撤销旧通行证"))
		s.publish(updated, "变更方案已更新")
	}
	return updated, err
}

func (s *Service) Submit(id, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || change.SubmitterID != actor.ID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusDraft && change.Status != model.StatusCheckFailed {
		return model.ChangeRequest{}, ErrInvalidState
	}
	_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "变更重新提交检查，撤销旧通行证"))
	policies := s.store.PoliciesByOrganization(actor.OrganizationID)
	ruleSetVersion := changegate.RuleSetVersion(policies)
	result := checker.CheckReleaseWithPolicies(checker.ReleaseInput{SQL: change.SQL, RollbackSQL: change.RollbackSQL, RollbackPlan: change.RollbackPlan, Artifacts: change.Artifacts, ReleasePlan: change.ReleasePlan}, checker.Context{Environment: change.Environment, ChangeType: change.ChangeType}, policies)
	windowFindings, windowPolicyCodes := releaseWindowFindings(
		change,
		s.store.ApplicationsByOrganization(actor.OrganizationID),
		s.store.ChangesByOrganization(actor.OrganizationID),
		policies,
	)
	if len(windowFindings) > 0 {
		result.Findings = removeBaselineFindings(result.Findings)
		result.Findings = append(result.Findings, windowFindings...)
		result.MatchedPolicyCodes = appendUniqueStrings(result.MatchedPolicyCodes, windowPolicyCodes...)
		for _, finding := range windowFindings {
			result.Risk = maxRiskLevel(result.Risk, finding.Severity)
		}
	}
	_ = s.store.RecordPolicyHitsForOrganization(actor.OrganizationID, result.MatchedPolicyCodes)
	next := model.StatusReadyForExperiment
	title := "规则检查通过"
	detail := fmt.Sprintf("识别 %d 类变更证据和 %d 条 SQL，命中 %d 项规则证据", len(change.Artifacts), len(result.Statements), len(result.Findings))
	if hasBlockingFinding(result.Findings) {
		next = model.StatusCheckFailed
		title = "规则检查阻断"
		detail = "发现发布窗口冲突、配置泄密、Kubernetes 安全基线、危险 SQL 或缺失回滚等阻断项"
	} else if strings.TrimSpace(change.SQL) == "" {
		next = model.StatusWaitingApproval
		title = "确定性静态门禁通过，等待审批"
		detail = "配置与 Kubernetes 制品已完成真实规则检查；未伪造运行时演练结果"
	}
	blockingCount := 0
	for _, finding := range result.Findings {
		if isBlockingFinding(finding) && finding.Status != model.FindingVerified {
			blockingCount++
		}
	}
	checkStatus := "PASSED"
	if blockingCount > 0 {
		checkStatus = "BLOCKED"
	}
	checkRun := &model.CheckRun{ID: store.NewID("check_"), ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: ruleSetVersion, Status: checkStatus, Findings: len(result.Findings), Blocking: blockingCount, CheckedAt: time.Now()}
	// AI reference analysis after deterministic checks (never decides release).
	analysisInput := change
	analysisInput.Findings = result.Findings
	analysisInput.CheckRun = checkRun
	analysisInput.Risk = result.Risk
	analysisInput.RuleSetVersion = ruleSetVersion
	analysis := s.analyzer.Analyze(context.Background(), analysisInput)
	normalizeAdvisoryRisk(&analysis)
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Findings = result.Findings
		item.CheckRun = checkRun
		item.Risk = result.Risk
		item.RuleSetVersion = ruleSetVersion
		item.Experiment = nil
		item.Analysis = &analysis
		item.Status = next
		item.Timeline = append(item.Timeline, timeline(next, title, detail, actor.Name))
		if analysis.Summary != "" {
			item.Timeline = append(item.Timeline, timeline(next, "生成 AI 参考意见", "模型仅输出参考结论，不参与放行决策；provider="+analysis.Provider, "系统"))
		}
		return nil
	}, auditChangeVersion(audit(actor, id, "SUBMIT_CHECK", detail), change))
	if err == nil {
		s.publish(updated, detail)
	}
	return updated, err
}

func normalizeAdvisoryRisk(analysis *model.AgentAnalysis) {
	if analysis == nil {
		return
	}
	if analysis.AdvisoryRisk == "" {
		analysis.AdvisoryRisk = analysis.Risk
	}
	// Preserve the legacy JSON field while making its advisory semantics explicit.
	analysis.Risk = analysis.AdvisoryRisk
}

func (s *Service) QueueExperiment(id, actorID string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusReadyForExperiment {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if strings.TrimSpace(change.SQL) == "" {
		return model.ChangeRequest{}, fmt.Errorf("%w：非 SQL 变更已由确定性静态门禁直接进入审批，无需演练", ErrInvalidState)
	}
	if change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, ErrForbidden
	}
	updated, err := s.store.UpdateChangeWithOutbox(id, func(item *model.ChangeRequest) error {
		item.Status = model.StatusExperimentQueued
		item.Timeline = append(item.Timeline, timeline(model.StatusExperimentQueued, "进入演练队列", "可靠 Worker 将执行影子库变更、回滚与指标采集", actor.Name))
		return nil
	}, model.OutboxEvent{
		OrganizationID: actor.OrganizationID, AggregateType: "change", AggregateID: id,
		EventType: "experiment.requested", Status: model.OutboxPending, MaxAttempts: 5, InputSHA256: change.ArtifactSHA256,
	}, audit(actor, id, "QUEUE_EXPERIMENT", "提交影子库演练任务并写入事务 Outbox"))
	if err != nil {
		return model.ChangeRequest{}, err
	}
	select {
	case s.queue <- id:
	default:
	}
	s.publish(updated, "已进入影子演练队列")
	return updated, nil
}

func (s *Service) AddComment(id, actorID, content string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "view") {
		return model.ChangeRequest{}, ErrForbidden
	}
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：评论内容应为 1 到 1000 个字符", ErrValidation)
	}
	comment := model.ChangeComment{
		ID: store.NewID("cmt_"), AuthorID: actor.ID, AuthorName: actor.Name,
		AuthorRole: actor.Role, Content: content, CreatedAt: time.Now(),
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Comments = append(item.Comments, comment)
		return nil
	}, audit(actor, id, "COMMENT", "添加协作评论"))
	if err == nil {
		s.publish(updated, "变更单新增协作评论")
	}
	return updated, err
}

func (s *Service) CreatePolicy(input model.SaveRiskPolicyInput, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	input.Code = strings.ToUpper(strings.TrimSpace(input.Code))
	if err := validatePolicyInput(input, false); err != nil {
		return model.RiskPolicy{}, err
	}
	if _, err := s.store.PolicyByCodeForOrganization(actor.OrganizationID, input.Code); err == nil {
		return model.RiskPolicy{}, fmt.Errorf("%w：规则编码已存在", ErrValidation)
	}
	now := time.Now()
	policy := model.RiskPolicy{
		OrganizationID: actor.OrganizationID, ID: store.NewID("pol_"), Code: input.Code, Name: strings.TrimSpace(input.Name),
		Description: strings.TrimSpace(input.Description), Pattern: strings.TrimSpace(input.Pattern),
		Severity: input.Severity, Blocking: input.Blocking, Enabled: input.Enabled, Builtin: false,
		Environments: normalizeStringList(input.Environments), ChangeTypes: normalizeStringList(input.ChangeTypes),
		ArtifactKinds: normalizeStringList(input.ArtifactKinds),
		Suggestion:    strings.TrimSpace(input.Suggestion), Version: 1, CreatedAt: now, UpdatedAt: now, UpdatedBy: actor.Name,
	}
	detail := fmt.Sprintf("创建自定义风险规则 %s（%s）", policy.Name, policy.Code)
	if err := s.store.CreatePolicy(policy, audit(actor, "", "CREATE_POLICY", detail)); err != nil {
		return model.RiskPolicy{}, err
	}
	return policy, nil
}

func (s *Service) UpdatePolicy(id string, input model.SaveRiskPolicyInput, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	current, err := s.store.PolicyForOrganization(actor.OrganizationID, id)
	if err != nil {
		return model.RiskPolicy{}, err
	}
	input.Code = current.Code
	if current.Builtin {
		input.Pattern = current.Pattern
	}
	if err := validatePolicyInput(input, current.Builtin); err != nil {
		return model.RiskPolicy{}, err
	}
	detail := fmt.Sprintf("更新风险规则 %s（%s）", strings.TrimSpace(input.Name), current.Code)
	return s.store.UpdatePolicy(id, func(policy *model.RiskPolicy) error {
		policy.Name = strings.TrimSpace(input.Name)
		policy.Description = strings.TrimSpace(input.Description)
		policy.Pattern = strings.TrimSpace(input.Pattern)
		policy.Severity = input.Severity
		policy.Blocking = input.Blocking
		policy.Enabled = input.Enabled
		policy.Environments = normalizeStringList(input.Environments)
		policy.ChangeTypes = normalizeStringList(input.ChangeTypes)
		policy.ArtifactKinds = normalizeStringList(input.ArtifactKinds)
		policy.Suggestion = strings.TrimSpace(input.Suggestion)
		policy.UpdatedBy = actor.Name
		return nil
	}, audit(actor, "", "UPDATE_POLICY", detail))
}

func (s *Service) TogglePolicy(id, actorID string) (model.RiskPolicy, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || actor.Role != "技术负责人" {
		return model.RiskPolicy{}, ErrForbidden
	}
	current, err := s.store.PolicyForOrganization(actor.OrganizationID, id)
	if err != nil {
		return model.RiskPolicy{}, err
	}
	action := "启用"
	if current.Enabled {
		action = "停用"
	}
	detail := fmt.Sprintf("%s风险规则 %s（%s）", action, current.Name, current.Code)
	return s.store.UpdatePolicy(id, func(policy *model.RiskPolicy) error {
		policy.Enabled = !policy.Enabled
		policy.UpdatedBy = actor.Name
		return nil
	}, audit(actor, "", "TOGGLE_POLICY", detail))
}

func (s *Service) TestPolicies(input model.TestRiskPolicyInput, actorID string) (checker.Result, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return checker.Result{}, ErrForbidden
	}
	if strings.TrimSpace(input.SQL) == "" && strings.TrimSpace(input.Content) == "" {
		return checker.Result{}, fmt.Errorf("%w：请输入需要试跑的 SQL 或变更制品内容", ErrValidation)
	}
	if strings.TrimSpace(input.Content) != "" {
		kind := model.ArtifactKind(strings.ToUpper(strings.TrimSpace(input.ArtifactKind)))
		result := checker.CheckReleaseWithPolicies(checker.ReleaseInput{
			Artifacts:    []model.ChangeArtifact{{ID: "artifact_test", Kind: kind, Name: "规则试跑制品", Content: input.Content}},
			RollbackPlan: "规则试跑使用的模拟回滚方案",
			ReleasePlan:  model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, AutoRollback: true, SuccessMetrics: []string{"错误率", "P99 延迟"}},
		}, checker.Context{Environment: defaultString(input.Environment, "生产环境"), ChangeType: defaultString(input.ChangeType, "混合发布")}, s.store.PoliciesByOrganization(actor.OrganizationID))
		return result, nil
	}
	result := checker.CheckWithPolicies(
		input.SQL,
		input.RollbackSQL,
		checker.Context{Environment: defaultString(input.Environment, "生产环境"), ChangeType: defaultString(input.ChangeType, "DDL")},
		s.store.PoliciesByOrganization(actor.OrganizationID),
	)
	return result, nil
}

func validatePolicyInput(input model.SaveRiskPolicyInput, builtin bool) error {
	codePattern := regexp.MustCompile("^[A-Z][A-Z0-9_]{2,63}$")
	if !codePattern.MatchString(input.Code) {
		return fmt.Errorf("%w：规则编码只能包含大写字母、数字和下划线", ErrValidation)
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Description), 1, 1000) ||
		!validRuneLength(strings.TrimSpace(input.Suggestion), 1, 1000) ||
		!validRuneLength(strings.TrimSpace(input.Pattern), 0, 500) {
		return fmt.Errorf("%w：规则名称、说明、表达式或整改建议长度不符合要求", ErrValidation)
	}
	if input.Severity != model.RiskLow && input.Severity != model.RiskMedium && input.Severity != model.RiskHigh {
		return fmt.Errorf("%w：风险等级不正确", ErrValidation)
	}
	if !builtin && strings.TrimSpace(input.Pattern) == "" {
		return fmt.Errorf("%w：自定义规则必须填写正则表达式", ErrValidation)
	}
	if strings.TrimSpace(input.Pattern) != "" {
		if _, err := regexp.Compile(input.Pattern); err != nil {
			return fmt.Errorf("%w：正则表达式无效：%v", ErrValidation, err)
		}
	}
	return nil
}

func normalizeStringList(items []string) []string {
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (s *Service) AssignFinding(changeID, findingID, actorID string, input model.AssignFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	owner, err := s.store.User(input.OwnerID)
	if err != nil || !owner.Active {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险负责人不存在或已停用", ErrValidation)
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || owner.OrganizationID != actor.OrganizationID ||
		!s.canUseApplication(actor, change.ApplicationID, "review") ||
		!s.canUseApplication(owner, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if findingIndex(change.Findings, findingID) < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	now := time.Now()
	if input.DueAt.IsZero() {
		input.DueAt = now.Add(72 * time.Hour)
	}
	if !input.DueAt.After(now) {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改期限必须晚于当前时间", ErrValidation)
	}
	detail := fmt.Sprintf("风险项 %s 已派给 %s，要求于 %s 前完成整改", findingID, owner.Name, input.DueAt.Format("2006-01-02 15:04"))
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		index := findingIndex(item.Findings, findingID)
		if index < 0 {
			return ErrValidation
		}
		finding := &item.Findings[index]
		finding.Status = model.FindingAssigned
		finding.OwnerID = owner.ID
		finding.OwnerName = owner.Name
		finding.DueAt = &input.DueAt
		finding.UpdatedAt = now
		item.Timeline = append(item.Timeline, timeline(item.Status, "风险整改派单", detail, actor.Name))
		return nil
	}, audit(actor, changeID, "ASSIGN_FINDING", detail))
	if err == nil {
		s.publish(updated, "风险项已派单")
	}
	return updated, err
}

func (s *Service) ResolveFinding(changeID, findingID, actorID string, input model.ResolveFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "submit") {
		return model.ChangeRequest{}, ErrForbidden
	}
	index := findingIndex(change.Findings, findingID)
	if index < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	finding := change.Findings[index]
	if finding.OwnerID == "" {
		if change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
			return model.ChangeRequest{}, ErrForbidden
		}
	} else if finding.OwnerID != actor.ID && change.SubmitterID != actor.ID && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, ErrForbidden
	}
	if finding.Status == model.FindingVerified {
		return model.ChangeRequest{}, ErrInvalidState
	}
	resolution := strings.TrimSpace(input.Resolution)
	if len([]rune(resolution)) < 5 || len([]rune(resolution)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改说明应为 5 到 1000 个字符", ErrValidation)
	}
	now := time.Now()
	detail := fmt.Sprintf("风险项 %s 已提交整改，等待审核人复核", findingID)
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		idx := findingIndex(item.Findings, findingID)
		if idx < 0 {
			return ErrValidation
		}
		target := &item.Findings[idx]
		target.Status = model.FindingResolved
		if target.OwnerID == "" {
			target.OwnerID, target.OwnerName = actor.ID, actor.Name
		}
		target.Resolution = resolution
		target.ResolvedAt = &now
		target.VerifiedAt = nil
		target.VerifiedByID, target.VerifiedByName, target.VerificationComment = "", "", ""
		target.UpdatedAt = now
		item.Timeline = append(item.Timeline, timeline(item.Status, "提交风险整改", detail, actor.Name))
		return nil
	}, audit(actor, changeID, "RESOLVE_FINDING", detail))
	if err == nil {
		s.publish(updated, "风险项已提交整改")
	}
	return updated, err
}

func (s *Service) VerifyFinding(changeID, findingID, actorID string, input model.VerifyFindingInput) (model.ChangeRequest, error) {
	actor, err := s.store.User(actorID)
	if err != nil || !actor.Active || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(changeID)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.Status == model.StatusApproved || change.Status == model.StatusCompleted {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	index := findingIndex(change.Findings, findingID)
	if index < 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：风险项不存在", ErrValidation)
	}
	finding := change.Findings[index]
	if finding.Status != model.FindingResolved {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if finding.OwnerID == actor.ID {
		return model.ChangeRequest{}, fmt.Errorf("%w：整改人与复核人必须分离", ErrForbidden)
	}
	comment := strings.TrimSpace(input.Comment)
	if len([]rune(comment)) > 1000 {
		return model.ChangeRequest{}, fmt.Errorf("%w：复核意见不能超过 1000 个字符", ErrValidation)
	}
	if comment == "" {
		if input.Approved {
			comment = "整改证据已核对，复核通过"
		} else {
			return model.ChangeRequest{}, fmt.Errorf("%w：退回整改时必须填写原因", ErrValidation)
		}
	}
	now := time.Now()
	actionTitle := "风险复核通过"
	actionCode := "VERIFY_FINDING"
	if !input.Approved {
		actionTitle, actionCode = "风险整改退回", "REJECT_FINDING"
	}
	updated, err := s.store.UpdateChange(changeID, func(item *model.ChangeRequest) error {
		idx := findingIndex(item.Findings, findingID)
		if idx < 0 {
			return ErrValidation
		}
		target := &item.Findings[idx]
		target.VerifiedByID, target.VerifiedByName = actor.ID, actor.Name
		target.VerificationComment = comment
		target.UpdatedAt = now
		if input.Approved {
			target.Status = model.FindingVerified
			target.VerifiedAt = &now
		} else {
			target.Status = model.FindingAssigned
			target.VerifiedAt = nil
		}
		item.Timeline = append(item.Timeline, timeline(item.Status, actionTitle, comment, actor.Name))
		if input.Approved && item.Status == model.StatusCheckFailed && allBlockingFindingsVerified(item.Findings) {
			if item.CheckRun == nil {
				item.CheckRun = &model.CheckRun{ID: store.NewID("check_"), ArtifactSHA256: item.ArtifactSHA256, RuleSetVersion: item.RuleSetVersion}
			}
			item.CheckRun.ArtifactSHA256 = item.ArtifactSHA256
			item.CheckRun.RuleSetVersion = item.RuleSetVersion
			item.CheckRun.Status = "PASSED"
			item.CheckRun.Blocking = 0
			item.CheckRun.Findings = len(item.Findings)
			item.CheckRun.CheckedAt = now
			if strings.TrimSpace(item.SQL) != "" {
				item.Status = model.StatusReadyForExperiment
				item.Timeline = append(item.Timeline, timeline(model.StatusReadyForExperiment, "阻断风险已闭环", "所有阻断风险项均完成整改与独立复核，可以进入 PostgreSQL 影子演练", actor.Name))
			} else {
				item.Status = model.StatusWaitingApproval
				item.Timeline = append(item.Timeline, timeline(model.StatusWaitingApproval, "阻断风险已闭环", "所有阻断风险项均完成整改与独立复核，静态门禁通过，等待独立审批", actor.Name))
			}
		}
		return nil
	}, audit(actor, changeID, actionCode, comment))
	if err == nil {
		s.publish(updated, actionTitle)
	}
	return updated, err
}
func (s *Service) Approve(id, actorID, comment string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusWaitingApproval {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if actor.ID == change.SubmitterID || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if strings.TrimSpace(change.SQL) != "" {
		if !trustedSQLExperiment(change.Experiment, change) {
			return model.ChangeRequest{}, fmt.Errorf("%w：SQL 变更必须通过真实 PostgreSQL 影子演练", ErrInvalidState)
		}
	} else if change.CheckRun == nil || change.CheckRun.Status != "PASSED" || change.CheckRun.Blocking != 0 {
		return model.ChangeRequest{}, fmt.Errorf("%w：静态门禁尚未通过", ErrInvalidState)
	}
	if !allBlockingFindingsVerified(change.Findings) && hasBlockingFinding(change.Findings) {
		return model.ChangeRequest{}, fmt.Errorf("%w：仍有未复核的高风险项", ErrInvalidState)
	}
	if change.Risk == model.RiskHigh && actor.Role != "技术负责人" {
		return model.ChangeRequest{}, fmt.Errorf("%w：高风险变更需要技术负责人审批", ErrForbidden)
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		comment = "证据已核对，同意按计划窗口执行"
	}
	if !validRuneLength(comment, 1, 2000) {
		return model.ChangeRequest{}, fmt.Errorf("%w：审批意见不能超过 2000 个字符", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Status = model.StatusApproved
		item.ReviewerID = actor.ID
		item.ReviewerName = actor.Name
		item.ReviewComment = comment
		item.Timeline = append(item.Timeline, timeline(model.StatusApproved, "审批通过", comment, actor.Name))
		return nil
	}, auditChangeVersion(audit(actor, id, "APPROVE", comment), change))
	if err == nil {
		s.publish(updated, "变更审批通过")
	}
	return updated, err
}

func (s *Service) Reject(id, actorID, comment string) (model.ChangeRequest, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.ChangeRequest{}, ErrForbidden
	}
	change, err := s.store.Change(id)
	if err != nil {
		return model.ChangeRequest{}, err
	}
	if change.OrganizationID != actor.OrganizationID || !s.canUseApplication(actor, change.ApplicationID, "review") {
		return model.ChangeRequest{}, ErrForbidden
	}
	if change.Status != model.StatusWaitingApproval {
		return model.ChangeRequest{}, ErrInvalidState
	}
	if actor.ID == change.SubmitterID || (actor.Role != "数据库审核人" && actor.Role != "技术负责人") {
		return model.ChangeRequest{}, ErrForbidden
	}
	comment = strings.TrimSpace(comment)
	if !validRuneLength(comment, 1, 2000) {
		return model.ChangeRequest{}, fmt.Errorf("%w：驳回原因应为 1 到 2000 个字符", ErrValidation)
	}
	updated, err := s.store.UpdateChange(id, func(item *model.ChangeRequest) error {
		item.Status = model.StatusRejected
		item.ReviewerID = actor.ID
		item.ReviewerName = actor.Name
		item.ReviewComment = comment
		item.Timeline = append(item.Timeline, timeline(model.StatusRejected, "审批驳回", comment, actor.Name))
		return nil
	}, audit(actor, id, "REJECT", comment))
	if err == nil {
		_ = s.store.RevokePassportsByChange(actor.OrganizationID, id, actor.ID, time.Now().UTC(), audit(actor, id, "PASSPORTS_REVOKED", "审批驳回，撤销旧通行证"))
		s.publish(updated, "变更已驳回")
	}
	return updated, err
}

func (s *Service) Complete(id, actorID string) (model.ChangeRequest, error) {
	return model.ChangeRequest{}, fmt.Errorf("%w：生产变更只能由 CI 消费一次性通行证后自动完成", ErrInvalidState)
}

func (s *Service) Subscribe() (<-chan Event, func()) {
	channel := make(chan Event, 8)
	s.subMu.Lock()
	s.subs[channel] = struct{}{}
	s.subMu.Unlock()
	return channel, func() {
		s.subMu.Lock()
		if _, ok := s.subs[channel]; ok {
			delete(s.subs, channel)
			close(channel)
		}
		s.subMu.Unlock()
	}
}

func (s *Service) worker(ctx context.Context, workerID string) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.queue:
		case <-ticker.C:
		}
		for s.processOutboxEvent(ctx, workerID) {
		}
	}
}

func (s *Service) processOutboxEvent(ctx context.Context, workerID string) bool {
	const lease = 2 * time.Minute
	event, err := s.store.ClaimOutbox(workerID, lease)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		return false
	}
	processingCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-processingCtx.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				if renewErr := s.store.RenewOutbox(event.ID, workerID, event.LeaseGeneration, lease); renewErr != nil {
					renewDone <- renewErr
					cancel()
					return
				}
			}
		}
	}()
	var processErr error
	switch event.EventType {
	case "experiment.requested":
		processErr = s.runExperiment(processingCtx, event, workerID)
	default:
		processErr = fmt.Errorf("unsupported outbox event %s", event.EventType)
	}
	cancel()
	if renewErr := <-renewDone; processErr == nil && renewErr != nil {
		processErr = fmt.Errorf("续租异步任务失败: %w", renewErr)
	}
	if processErr != nil {
		_ = s.store.FailOutbox(event.ID, workerID, event.LeaseGeneration, processErr)
	} else {
		_ = s.store.CompleteOutbox(event.ID, workerID, event.LeaseGeneration)
	}
	return true
}

func retryConcurrentWrite(action func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = action()
		if !errors.Is(err, store.ErrConcurrentWrite) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return err
}

func (s *Service) runExperiment(ctx context.Context, event model.OutboxEvent, workerID string) error {
	change, err := s.store.Change(event.AggregateID)
	if err != nil {
		return err
	}
	if event.InputSHA256 == "" || event.InputSHA256 != change.ArtifactSHA256 {
		return fmt.Errorf("演练输入摘要与当前制品不一致")
	}
	if event.ResultDigest != "" {
		// FINALIZE and the business report were committed atomically. A restart
		// only needs to complete the outbox envelope; it must not rerun APPLY.
		return nil
	}
	if change.Status != model.StatusExperimentQueued && change.Status != model.StatusExperimentRunning {
		return nil
	}
	if err := s.store.CheckpointExperimentOutbox(event.ID, workerID, event.LeaseGeneration, model.OutboxStagePrepare, event.InputSHA256); err != nil {
		return err
	}
	systemActor := model.User{ID: "system_worker", OrganizationID: change.OrganizationID, Name: "演练 Worker", Role: "系统", Active: true}
	running, err := s.store.UpdateChange(event.AggregateID, func(item *model.ChangeRequest) error {
		if item.ArtifactSHA256 != event.InputSHA256 {
			return fmt.Errorf("演练输入摘要与当前制品不一致")
		}
		if item.Status != model.StatusExperimentQueued && item.Status != model.StatusExperimentRunning {
			return ErrInvalidState
		}
		if item.Status == model.StatusExperimentQueued {
			item.Status = model.StatusExperimentRunning
			item.Timeline = append(item.Timeline, timeline(model.StatusExperimentRunning, "开始预发布验证", "执行单体隔离影子事务；中断后由新 lease generation 从 APPLY 重跑", systemActor.Name))
		}
		return nil
	}, auditExperiment(audit(systemActor, event.AggregateID, "EXPERIMENT_START", fmt.Sprintf("Worker 开始执行 attempt=%s generation=%d", event.AttemptID, event.LeaseGeneration)), event.ID, event.AttemptID))
	if err != nil {
		return err
	}
	s.publish(running, "预发布验证进行中")
	if strings.TrimSpace(change.SQL) == "" {
		return fmt.Errorf("非 SQL 变更不执行数据库影子演练")
	}
	if err := s.store.CheckpointExperimentOutbox(event.ID, workerID, event.LeaseGeneration, model.OutboxStageApply, event.InputSHA256); err != nil {
		return err
	}
	report := s.runner.Run(ctx, change)
	enrichReleaseReport(&report, change)
	report.AttemptID = event.AttemptID
	report.LeaseGeneration = event.LeaseGeneration
	report.InputSHA256 = event.InputSHA256
	if err := ctx.Err(); err != nil {
		return err
	}
	latest, err := s.store.Change(event.AggregateID)
	if err != nil {
		return err
	}
	if latest.ArtifactSHA256 != event.InputSHA256 {
		return fmt.Errorf("演练完成时输入摘要已变化")
	}
	latest.Experiment = &report
	analysis := s.analyzer.Analyze(ctx, latest)
	normalizeAdvisoryRisk(&analysis)
	if err := ctx.Err(); err != nil {
		return err
	}
	next := model.StatusWaitingApproval
	title := "预发布验证完成，等待审批"
	detail := "规则、数据库演练、制品检查、发布策略和智能分析证据已汇总"
	if !trustedSQLExperiment(&report, change) {
		next = model.StatusCheckFailed
		title = "预发布验证失败"
		detail = defaultString(report.ExecutionError, "验证未通过，请修正后重新提交")
	}
	digestInput, err := json.Marshal(report)
	if err != nil {
		return err
	}
	resultDigest := changegate.SHA256(string(digestInput))
	report.ResultDigest = resultDigest
	updated, err := s.store.FinalizeExperimentOutbox(event.ID, workerID, event.LeaseGeneration, event.AttemptID, event.InputSHA256, resultDigest, func(item *model.ChangeRequest) error {
		if item.ArtifactSHA256 != event.InputSHA256 {
			return fmt.Errorf("演练完成时输入摘要已变化")
		}
		item.Experiment = &report
		item.Analysis = &analysis
		item.Status = next
		item.Timeline = append(item.Timeline, timeline(next, title, detail, systemActor.Name))
		return nil
	}, auditExperiment(audit(systemActor, event.AggregateID, "EXPERIMENT_FINISH", fmt.Sprintf("%s；attempt=%s generation=%d", detail, event.AttemptID, event.LeaseGeneration)), event.ID, event.AttemptID))
	if err != nil {
		return err
	}
	s.publish(updated, detail)
	return nil
}

func releaseValidationReport(change model.ChangeRequest) model.ExperimentReport {
	now := time.Now()
	return model.ExperimentReport{
		ID: store.NewID("validation_"), Kind: "STATIC_GATE", Mode: "DETERMINISTIC",
		Status: "NOT_RUN", StartedAt: now, FinishedAt: now,
		ExecutionError: "非 SQL 变更仅执行确定性静态门禁，不伪造运行时演练结果",
	}
}

func enrichReleaseReport(report *model.ExperimentReport, change model.ChangeRequest) {
	if report.ID == "" {
		report.ID = store.NewID("validation_")
	}
	if report.Kind == "" {
		if strings.TrimSpace(change.SQL) != "" {
			report.Kind = "DATABASE_AND_RELEASE_VALIDATION"
		} else {
			report.Kind = "RELEASE_VALIDATION"
		}
	}
	if report.Mode == "" {
		report.Mode = "DETERMINISTIC"
	}
	report.Strategy = change.ReleasePlan.Strategy
	report.CanaryPercent = change.ReleasePlan.CanaryPercent
	report.ObservationMinutes = change.ReleasePlan.ObservationMinutes
	report.ChecksTotal = len(change.Artifacts) + 3
	if report.Status == "PASSED" && strings.EqualFold(report.Mode, "POSTGRES") {
		report.ChecksPassed = report.ChecksTotal
	} else {
		report.ChecksPassed = 0
	}
	now := time.Now()
	kinds := make([]string, 0, len(change.Artifacts))
	seen := make(map[model.ArtifactKind]bool)
	for _, artifact := range change.Artifacts {
		if !seen[artifact.Kind] {
			seen[artifact.Kind] = true
			kinds = append(kinds, string(artifact.Kind))
		}
	}
	report.Evidence = append(report.Evidence,
		model.Evidence{ID: store.NewID("ev_artifact_"), Kind: "artifact", Title: "变更制品清单", Value: fmt.Sprintf("%d 项：%s", len(change.Artifacts), strings.Join(kinds, " / ")), Source: "统一变更单", ObservedAt: now},
		model.Evidence{ID: store.NewID("ev_release_"), Kind: "release", Title: "发布策略", Value: fmt.Sprintf("%s，灰度 %d%%，观察 %d 分钟", change.ReleasePlan.Strategy, change.ReleasePlan.CanaryPercent, change.ReleasePlan.ObservationMinutes), Source: "发布计划", ObservedAt: now},
		model.Evidence{ID: store.NewID("ev_metric_"), Kind: "metric", Title: "成功判定指标", Value: strings.Join(change.ReleasePlan.SuccessMetrics, " / "), Source: "发布计划", ObservedAt: now},
	)
}

func (s *Service) publish(change model.ChangeRequest, message string) {
	event := Event{OrganizationID: change.OrganizationID, Type: "change.updated", ChangeID: change.ID, Status: change.Status, Message: message, At: time.Now()}
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for channel := range s.subs {
		select {
		case channel <- event:
		default:
		}
	}
}

func isBlockingFinding(item model.Finding) bool {
	legacyBlocking := map[string]bool{
		"EMPTY_SQL": true, "MISSING_ROLLBACK": true, "UPDATE_WITHOUT_WHERE": true,
		"DELETE_WITHOUT_WHERE": true, "DROP_TABLE": true, "TRUNCATE": true,
		"ADD_NOT_NULL_WITHOUT_DEFAULT": true,
		"FK_WITHOUT_NOT_VALID":         true, "HEAVY_DDL_REWRITE": true,
	}
	return item.Blocking || legacyBlocking[item.Code]
}

func hasBlockingFinding(findings []model.Finding) bool {
	for _, item := range findings {
		if isBlockingFinding(item) && item.Status != model.FindingVerified {
			return true
		}
	}
	return false
}

func findingIndex(findings []model.Finding, id string) int {
	for index := range findings {
		if findings[index].ID == id {
			return index
		}
	}
	return -1
}

func allBlockingFindingsVerified(findings []model.Finding) bool {
	hasBlocking := false
	for _, finding := range findings {
		if !isBlockingFinding(finding) {
			continue
		}
		hasBlocking = true
		if finding.Status != model.FindingVerified {
			return false
		}
	}
	return hasBlocking
}

func maxRiskLevel(left, right model.RiskLevel) model.RiskLevel {
	rank := map[model.RiskLevel]int{
		model.RiskUnknown: 0, model.RiskLow: 1, model.RiskMedium: 2, model.RiskHigh: 3,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

const releaseWindowProtection = 90 * time.Minute

func releaseWindowFindings(current model.ChangeRequest, applications []model.Application, changes []model.ChangeRequest, policies []model.RiskPolicy) ([]model.Finding, []string) {
	if current.PlannedAt.IsZero() {
		return nil, nil
	}
	policyByCode := make(map[string]model.RiskPolicy, 2)
	for _, policy := range policies {
		if policy.Enabled && (policy.Code == "CHANGE_WINDOW_CONFLICT" || policy.Code == "DEPENDENCY_WINDOW_OVERLAP") {
			policyByCode[policy.Code] = policy
		}
	}
	if len(policyByCode) == 0 {
		return nil, nil
	}

	applicationByID := make(map[string]model.Application, len(applications))
	for _, application := range applications {
		applicationByID[application.ID] = application
	}
	currentApplication, ok := applicationByID[current.ApplicationID]
	if !ok {
		return nil, nil
	}
	upstream := make(map[string]bool, len(currentApplication.Dependencies))
	for _, dependencyID := range currentApplication.Dependencies {
		upstream[dependencyID] = true
	}
	downstream := make(map[string]bool)
	for _, application := range applications {
		for _, dependencyID := range application.Dependencies {
			if dependencyID == current.ApplicationID {
				downstream[application.ID] = true
				break
			}
		}
	}

	now := time.Now()
	findings := make([]model.Finding, 0)
	policyCodes := make([]string, 0, 2)
	for _, other := range changes {
		if other.ID == current.ID || other.OrganizationID != current.OrganizationID || other.PlannedAt.IsZero() ||
			other.Status == model.StatusRejected || other.Status == model.StatusCompleted {
			continue
		}
		delta := current.PlannedAt.Sub(other.PlannedAt)
		if delta < 0 {
			delta = -delta
		}
		if delta > releaseWindowProtection {
			continue
		}

		code := ""
		relation := ""
		if other.ApplicationID == current.ApplicationID {
			code = "CHANGE_WINDOW_CONFLICT"
			relation = "同一服务"
		} else if upstream[other.ApplicationID] {
			code = "DEPENDENCY_WINDOW_OVERLAP"
			relation = "直接上游"
		} else if downstream[other.ApplicationID] {
			code = "DEPENDENCY_WINDOW_OVERLAP"
			relation = "直接下游"
		}
		policy, enabled := policyByCode[code]
		if code == "" || !enabled {
			continue
		}
		otherApplication := applicationByID[other.ApplicationID]
		otherName := other.ApplicationName
		if otherName == "" {
			otherName = otherApplication.Name
		}
		detail := fmt.Sprintf("%s“%s”在 90 分钟保护窗口内已有变更“%s”（%s）。", relation, otherName, other.Title, other.Status)
		evidence := fmt.Sprintf("change_id=%s; planned_at=%s; interval=%s", other.ID, other.PlannedAt.Format(time.RFC3339), formatWindowInterval(delta))
		findings = append(findings, model.Finding{
			ID:          "window_" + strings.ToLower(policy.Code) + "_" + other.ID,
			Code:        policy.Code,
			Severity:    policy.Severity,
			Title:       policy.Name,
			Detail:      detail,
			Evidence:    evidence,
			Suggestion:  policy.Suggestion,
			Blocking:    policy.Blocking,
			RuleVersion: policy.Version,
			Status:      model.FindingOpen,
			UpdatedAt:   now,
		})
		policyCodes = appendUniqueStrings(policyCodes, policy.Code)
	}
	return findings, policyCodes
}

func formatWindowInterval(delta time.Duration) string {
	minutes := int(delta.Round(time.Minute) / time.Minute)
	if minutes < 1 {
		return "不足 1 分钟"
	}
	return fmt.Sprintf("%d 分钟", minutes)
}

func removeBaselineFindings(findings []model.Finding) []model.Finding {
	filtered := findings[:0]
	for _, finding := range findings {
		if finding.Code == "BASELINE_PASS" || finding.Code == "RELEASE_BASELINE_PASS" {
			continue
		}
		filtered = append(filtered, finding)
	}
	return filtered
}

func appendUniqueStrings(items []string, values ...string) []string {
	seen := make(map[string]bool, len(items)+len(values))
	result := make([]string, 0, len(items)+len(values))
	for _, item := range append(items, values...) {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}

func timeline(status model.ChangeStatus, title, detail, actor string) model.TimelineEntry {
	return model.TimelineEntry{ID: store.NewID("tl_"), Status: status, Title: title, Detail: detail, Actor: actor, CreatedAt: time.Now()}
}

func audit(actor model.User, changeID, action, detail string) model.AuditEvent {
	actorType, authMethod := "USER", "SESSION"
	if strings.HasPrefix(strings.ToLower(actor.ID), "ci:") {
		actorType, authMethod = "CI", "BEARER_PASSPORT"
	} else if strings.HasPrefix(strings.ToLower(actor.ID), "system") || strings.HasPrefix(strings.ToLower(actor.ID), "integration") {
		actorType, authMethod = "SYSTEM", "INTERNAL"
	}
	return model.AuditEvent{
		OrganizationID: actor.OrganizationID, ID: store.NewID("audit_"), ChangeID: changeID,
		ActorID: actor.ID, ActorName: actor.Name, ActorType: actorType, AuthMethod: authMethod,
		Action: action, ResourceType: "CHANGE", ResourceID: changeID, Result: "SUCCESS",
		Detail: detail, CreatedAt: time.Now().UTC(),
	}
}

func auditChangeVersion(event model.AuditEvent, before model.ChangeRequest) model.AuditEvent {
	event.ResourceVersionBefore = before.Version
	event.ResourceVersionAfter = before.Version + 1
	event.RequestDigest = before.ArtifactSHA256
	return event
}

func auditPassport(event model.AuditEvent, passportID string) model.AuditEvent {
	event.PassportID = passportID
	return event
}

func auditExperiment(event model.AuditEvent, relatedEventID, attemptID string) model.AuditEvent {
	event.RelatedEventID = relatedEventID
	event.AttemptID = attemptID
	return event
}

func normalizeArtifacts(items []model.ChangeArtifact, sqlText string) ([]model.ChangeArtifact, error) {
	allowed := map[model.ArtifactKind]bool{
		model.ArtifactConfig: true, model.ArtifactKubernetes: true, model.ArtifactDatabase: true,
	}
	result := make([]model.ChangeArtifact, 0, len(items)+1)
	hasDatabase := false
	for index, item := range items {
		if !allowed[item.Kind] {
			return nil, fmt.Errorf("%w：第 %d 个制品类型 %q 不在 v1 支持范围（仅支持 DATABASE、CONFIG、KUBERNETES）", ErrValidation, index+1, item.Kind)
		}
		if strings.TrimSpace(item.Content) == "" {
			return nil, fmt.Errorf("%w：第 %d 个制品内容不能为空", ErrValidation, index+1)
		}
		if item.ID == "" {
			item.ID = store.NewID("artifact_")
		}
		item.Name = defaultString(item.Name, string(item.Kind)+" 变更证据")
		item = changegate.PrepareArtifact(item)
		if item.Kind == model.ArtifactDatabase {
			hasDatabase = true
		}
		result = append(result, item)
	}
	if strings.TrimSpace(sqlText) != "" && !hasDatabase {
		item := model.ChangeArtifact{ID: store.NewID("artifact_"), Kind: model.ArtifactDatabase, Name: "数据库 SQL", Source: "变更单", Language: "SQL", Content: sqlText}
		result = append(result, changegate.PrepareArtifact(item))
	}
	return result, nil
}

func deriveChangeType(items []model.ChangeArtifact) string {
	kinds := make(map[model.ArtifactKind]struct{}, 3)
	for _, item := range items {
		kinds[item.Kind] = struct{}{}
	}
	if len(kinds) > 1 {
		return "联合变更"
	}
	if _, ok := kinds[model.ArtifactDatabase]; ok {
		return "数据库变更"
	}
	if _, ok := kinds[model.ArtifactConfig]; ok {
		return "配置变更"
	}
	if _, ok := kinds[model.ArtifactKubernetes]; ok {
		return "Kubernetes 变更"
	}
	return "生产变更"
}

func artifactBytes(items []model.ChangeArtifact) int {
	total := 0
	for _, item := range items {
		total += len(item.Name) + len(item.Source) + len(item.Language) + len(item.Content)
	}
	return total
}

func hasArtifactKind(items []model.ChangeArtifact, kind model.ArtifactKind) bool {
	for _, item := range items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func normalizeReleasePlan(plan model.ReleasePlan) model.ReleasePlan {
	plan.Strategy = defaultString(plan.Strategy, "全量发布")
	if plan.CanaryPercent < 0 {
		plan.CanaryPercent = 0
	}
	if plan.CanaryPercent > 100 {
		plan.CanaryPercent = 100
	}
	if plan.ObservationMinutes <= 0 {
		plan.ObservationMinutes = 15
	}
	if plan.ObservationMinutes > 1440 {
		plan.ObservationMinutes = 1440
	}
	plan.SuccessMetrics = normalizeStringList(plan.SuccessMetrics)
	return plan
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func (s *Service) validateApplicationDependencies(organizationID, applicationID string, dependencies []string) error {
	applications := s.store.ApplicationsByOrganization(organizationID)
	applicationByID := make(map[string]model.Application, len(applications))
	for _, application := range applications {
		applicationByID[application.ID] = application
	}
	for _, dependencyID := range dependencies {
		if dependencyID == applicationID && applicationID != "" {
			return fmt.Errorf("%w：服务不能依赖自身", ErrValidation)
		}
		if _, ok := applicationByID[dependencyID]; !ok {
			return fmt.Errorf("%w：依赖服务 %s 不存在或不属于当前企业", ErrValidation, dependencyID)
		}
	}
	if applicationID == "" {
		return nil
	}

	graph := make(map[string][]string, len(applications))
	for _, application := range applications {
		graph[application.ID] = append([]string(nil), application.Dependencies...)
	}
	graph[applicationID] = append([]string(nil), dependencies...)
	state := make(map[string]uint8, len(graph))
	var hasCycle func(string) bool
	hasCycle = func(node string) bool {
		if state[node] == 1 {
			return true
		}
		if state[node] == 2 {
			return false
		}
		state[node] = 1
		for _, dependencyID := range graph[node] {
			if hasCycle(dependencyID) {
				return true
			}
		}
		state[node] = 2
		return false
	}
	if hasCycle(applicationID) {
		return fmt.Errorf("%w：依赖关系形成循环，请调整上下游服务", ErrValidation)
	}
	return nil
}

func (s *Service) canUseApplication(actor model.User, applicationID, capability string) bool {
	if actor.EnterpriseAdmin || actor.Role == model.RoleOwner {
		return true
	}
	if !s.store.HasApplicationGrants(actor.OrganizationID) {
		return true
	}
	for _, grant := range s.store.ApplicationGrantsByUser(actor.OrganizationID, actor.ID) {
		if grant.ApplicationID != applicationID {
			continue
		}
		switch capability {
		case "submit":
			return grant.CanSubmit
		case "review":
			return grant.CanReview
		default:
			return grant.CanSubmit || grant.CanReview
		}
	}
	return false
}

func (s *Service) OperationsFor(actorID string) (model.OperationsSummary, []model.OutboxEvent, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.OperationsSummary{}, nil, err
	}
	if !actor.EnterpriseAdmin && actor.Role != model.RoleOwner {
		return model.OperationsSummary{}, nil, ErrForbidden
	}
	return s.store.OperationsSummary(actor.OrganizationID), s.store.OutboxByOrganization(actor.OrganizationID, false, 100), nil
}

func (s *Service) RetryOutbox(id, actorID string) error {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return ErrForbidden
	}
	if err := s.store.RetryOutbox(actor.OrganizationID, id, model.AuditEvent{
		ID: store.NewID("audit_"), ActorID: actor.ID, ActorName: actor.Name,
		Action: "RETRY_OUTBOX", Detail: "人工重试死信事件 " + id, CreatedAt: time.Now(),
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return fmt.Errorf("%w：当前事件不是死信状态", ErrInvalidState)
	}
	select {
	case s.queue <- id:
	default:
	}
	return nil
}

func (s *Service) Health(ctx context.Context) error { return s.store.Health(ctx) }
func (s *Service) StoreMode() string                { return s.store.Mode() }

func (s *Service) GlobalOperations() model.OperationsSummary { return s.store.OperationsSummary("") }

func dashboardForChanges(changes []model.ChangeRequest) model.Dashboard {
	dashboard := model.Dashboard{RiskDistribution: map[model.RiskLevel]int{model.RiskLow: 0, model.RiskMedium: 0, model.RiskHigh: 0, model.RiskUnknown: 0}}
	var experimentCount, experimentPass int
	var durationTotal int64
	for _, change := range changes {
		dashboard.RiskDistribution[change.Risk]++
		if change.Risk == model.RiskHigh {
			dashboard.HighRiskCount++
		}
		switch change.Status {
		case model.StatusDraft, model.StatusChecking, model.StatusReadyForExperiment, model.StatusExperimentQueued, model.StatusExperimentRunning, model.StatusWaitingApproval:
			dashboard.PendingCount++
		}
		if change.Status == model.StatusWaitingApproval {
			dashboard.PendingApprovals = append(dashboard.PendingApprovals, change)
		}
		if change.Experiment != nil {
			experimentCount++
			durationTotal += change.Experiment.DurationMS
			if change.Experiment.Status == "PASSED" {
				experimentPass++
			}
		}
	}
	if len(changes) > 6 {
		dashboard.RecentChanges = changes[:6]
	} else {
		dashboard.RecentChanges = changes
	}
	if experimentCount > 0 {
		dashboard.ExperimentPassRate = float64(experimentPass) / float64(experimentCount) * 100
		dashboard.AverageExperimentSec = float64(durationTotal) / float64(experimentCount) / 1000
	}
	return dashboard
}

func governanceOutcomesForEvidence(changes []model.ChangeRequest, integrationEvents []model.IntegrationEvent, outcomeSignals []model.OutcomeSignal, windowDays int, now time.Time, scope string) model.GovernanceOutcomeSummary {
	now = now.UTC()
	windowStart := now.Add(-time.Duration(windowDays) * 24 * time.Hour)
	result := model.GovernanceOutcomeSummary{
		WindowDays: windowDays, WindowStartedAt: windowStart, GeneratedAt: now, Scope: scope,
	}
	eligibleChanges := make(map[string]bool)
	var checkRuns, artifactEvidence, rollbackPlans, successMetrics int
	var progressiveDelivery, autoRollback int
	var decisionLeadTotal time.Duration
	var experimentPassed int
	var blockingVerified int
	var dueFindings, onTimeClosures int

	for _, change := range changes {
		createdAt := change.CreatedAt.UTC()
		if createdAt.Before(windowStart) || createdAt.After(now) {
			continue
		}
		result.TotalChanges++
		eligibleChanges[change.ID] = true
		production := isProductionEnvironment(change.Environment)
		if production {
			result.ProductionChanges++
			if isProgressiveDelivery(change.ReleasePlan.Strategy) {
				progressiveDelivery++
			}
			if change.ReleasePlan.AutoRollback {
				autoRollback++
			}
		}
		if change.Status == model.StatusCompleted {
			result.CompletedChanges++
		}
		switch change.Status {
		case model.StatusApproved, model.StatusCompleted:
			result.AcceptedDecisions++
		case model.StatusRejected:
			result.RejectedDecisions++
		}
		if change.Risk == model.RiskHigh {
			result.HighRiskChanges++
		}
		if change.CheckRun != nil {
			checkRuns++
		}
		if len(change.Artifacts) > 0 && strings.TrimSpace(change.ArtifactSHA256) != "" {
			artifactEvidence++
		}
		if strings.TrimSpace(change.RollbackPlan) != "" || strings.TrimSpace(change.RollbackSQL) != "" {
			rollbackPlans++
		}
		if len(change.ReleasePlan.SuccessMetrics) > 0 {
			successMetrics++
		}
		if decisionAt, ok := changeDecisionAt(change); ok && !decisionAt.Before(createdAt) {
			result.Flow.DecisionLeadTimeSampleCount++
			decisionLeadTotal += decisionAt.Sub(createdAt)
		}
		if change.Experiment != nil {
			result.Flow.ExperimentSampleCount++
			if change.Experiment.Status == "PASSED" {
				experimentPassed++
			}
		}
		for _, finding := range change.Findings {
			result.TotalFindings++
			if finding.Status == model.FindingVerified {
				result.VerifiedFindings++
			}
			if finding.Blocking {
				result.BlockingFindings++
				if finding.Status == model.FindingVerified {
					blockingVerified++
				} else {
					result.OpenBlockingFindings++
				}
			}
			if finding.DueAt != nil {
				dueFindings++
				if finding.VerifiedAt != nil && !finding.VerifiedAt.After(*finding.DueAt) {
					onTimeClosures++
				}
				if finding.Status != model.FindingVerified && finding.DueAt.Before(now) {
					result.OverdueFindings++
				}
			}
		}
	}

	latestDeploymentOutcomes := make(map[string]model.IntegrationEvent)
	for _, event := range integrationEvents {
		eventTime := integrationEventTime(event)
		if !eligibleChanges[event.ChangeID] || eventTime.Before(windowStart) || eventTime.After(now) {
			continue
		}
		if _, terminal := deploymentOutcome(event.Status); !terminal {
			continue
		}
		current, exists := latestDeploymentOutcomes[event.ChangeID]
		currentTime := integrationEventTime(current)
		if !exists || eventTime.After(currentTime) || (eventTime.Equal(currentTime) && event.ReceivedAt.After(current.ReceivedAt)) {
			latestDeploymentOutcomes[event.ChangeID] = event
		}
	}
	for _, event := range latestDeploymentOutcomes {
		failed, _ := deploymentOutcome(event.Status)
		result.Flow.DeploymentOutcomeSampleCount++
		if failed {
			result.Flow.FailedDeployments++
		} else {
			result.Flow.SuccessfulDeployments++
		}
	}

	latestRollbacks := make(map[string]model.OutcomeSignal)
	incidentEvents := make(map[string][]model.OutcomeSignal)
	latestSLIComparisons := make(map[string]model.OutcomeSignal)
	remediationAt := make(map[string]time.Time)
	for _, signal := range outcomeSignals {
		if !eligibleChanges[signal.ChangeID] || signal.OccurredAt.After(now) {
			continue
		}
		inWindow := !signal.OccurredAt.Before(windowStart)
		switch signal.Kind {
		case model.OutcomeSignalRollback:
			if !inWindow {
				continue
			}
			rememberEarliest(remediationAt, signal.ChangeID, signal.OccurredAt)
			if _, terminal := rollbackOutcome(signal.Status); !terminal {
				continue
			}
			key := signal.Source + "\x00" + signal.ChangeID + "\x00" + signal.OperationID
			current, exists := latestRollbacks[key]
			if !exists || signal.OccurredAt.After(current.OccurredAt) {
				latestRollbacks[key] = signal
			}
		case model.OutcomeSignalIncident:
			key := signal.Source + "\x00" + signal.ChangeID + "\x00" + signal.IncidentID
			incidentEvents[key] = append(incidentEvents[key], signal)
			if inWindow {
				rememberEarliest(remediationAt, signal.ChangeID, signal.OccurredAt)
			}
		case model.OutcomeSignalBusinessSLI:
			if !inWindow || signal.BaselineValue == nil || signal.ObservedValue == nil {
				continue
			}
			deployment, exists := latestDeploymentOutcomes[signal.ChangeID]
			failed, terminal := deploymentOutcome(deployment.Status)
			if !exists || !terminal || failed || !businessSLIBracketsDeployment(signal, integrationEventTime(deployment)) {
				continue
			}
			key := signal.Source + "\x00" + signal.ChangeID + "\x00" + strings.ToLower(signal.MetricName)
			current, exists := latestSLIComparisons[key]
			if !exists || signal.OccurredAt.After(current.OccurredAt) {
				latestSLIComparisons[key] = signal
			}
		}
	}
	for _, signal := range latestRollbacks {
		failed, _ := rollbackOutcome(signal.Status)
		result.Operations.RollbackOutcomeSampleCount++
		if failed {
			result.Operations.FailedRollbacks++
		} else {
			result.Operations.SuccessfulRollbacks++
		}
	}
	var incidentResolutionTotal time.Duration
	for _, events := range incidentEvents {
		sort.Slice(events, func(i, j int) bool { return events[i].OccurredAt.Before(events[j].OccurredAt) })
		observedInWindow := false
		openStartedAt := time.Time{}
		latestStatus := ""
		for _, event := range events {
			if !event.OccurredAt.Before(windowStart) {
				observedInWindow = true
			}
			latestStatus = event.Status
			if isResolvedIncident(event.Status) {
				if !openStartedAt.IsZero() && !event.OccurredAt.Before(windowStart) && !event.OccurredAt.Before(openStartedAt) {
					result.Operations.IncidentResolutionSampleCount++
					incidentResolutionTotal += event.OccurredAt.Sub(openStartedAt)
				}
				openStartedAt = time.Time{}
				continue
			}
			if openStartedAt.IsZero() {
				openStartedAt = event.OccurredAt
			}
		}
		if !observedInWindow {
			continue
		}
		result.Operations.LinkedIncidentCount++
		if isResolvedIncident(latestStatus) {
			result.Operations.ResolvedIncidents++
		} else {
			result.Operations.OpenIncidents++
		}
	}
	for _, signal := range latestSLIComparisons {
		result.Business.SLISampleCount++
		switch businessSLIOutcome(signal) {
		case 1:
			result.Business.ImprovedSLIs++
		case -1:
			result.Business.DegradedSLIs++
		default:
			result.Business.StableSLIs++
		}
		if signal.ObjectiveValue != nil {
			result.Business.ObjectiveSampleCount++
			if businessObjectiveMet(signal) {
				result.Business.ObjectivesMet++
			}
		}
	}
	for changeID, deployment := range latestDeploymentOutcomes {
		failed, _ := deploymentOutcome(deployment.Status)
		if failed {
			continue
		}
		result.Operations.PostReleaseSampleCount++
		if observedAt, exists := remediationAt[changeID]; exists && !observedAt.Before(integrationEventTime(deployment).Add(-5*time.Minute)) {
			result.Operations.RemediationRequiredDeployments++
		}
	}

	result.ControlCoverage = model.GovernanceControlCoverage{
		CheckRunPercent:            percentage(checkRuns, result.TotalChanges),
		ArtifactEvidencePercent:    percentage(artifactEvidence, result.TotalChanges),
		RollbackPlanPercent:        percentage(rollbackPlans, result.TotalChanges),
		SuccessMetricsPercent:      percentage(successMetrics, result.TotalChanges),
		ProgressiveDeliveryPercent: percentage(progressiveDelivery, result.ProductionChanges),
		AutoRollbackPercent:        percentage(autoRollback, result.ProductionChanges),
	}
	if result.Flow.DecisionLeadTimeSampleCount > 0 {
		result.Flow.AverageDecisionLeadMinutes = roundTwo(decisionLeadTotal.Minutes() / float64(result.Flow.DecisionLeadTimeSampleCount))
	}
	result.Flow.ExperimentPassRatePercent = percentage(experimentPassed, result.Flow.ExperimentSampleCount)
	result.Flow.DeploymentFailureRate = percentage(result.Flow.FailedDeployments, result.Flow.DeploymentOutcomeSampleCount)
	result.Flow.BlockingFindingClosureRate = percentage(blockingVerified, result.BlockingFindings)
	result.Flow.DueFindingSampleCount = dueFindings
	result.Flow.OnTimeFindingClosureRate = percentage(onTimeClosures, dueFindings)
	result.Operations.RollbackSuccessRatePercent = percentage(result.Operations.SuccessfulRollbacks, result.Operations.RollbackOutcomeSampleCount)
	if result.Operations.IncidentResolutionSampleCount > 0 {
		result.Operations.AverageIncidentResolutionMinutes = roundTwo(incidentResolutionTotal.Minutes() / float64(result.Operations.IncidentResolutionSampleCount))
	}
	result.Operations.PostReleaseRemediationRate = percentage(result.Operations.RemediationRequiredDeployments, result.Operations.PostReleaseSampleCount)
	result.Business.ObjectiveAttainmentRatePercent = percentage(result.Business.ObjectivesMet, result.Business.ObjectiveSampleCount)
	result.OutcomeDataQuality = model.GovernanceOutcomeDataQuality{
		ReleaseOutcomeObservable:  result.Flow.DeploymentOutcomeSampleCount > 0,
		RollbackOutcomeObservable: result.Operations.RollbackOutcomeSampleCount > 0,
		IncidentLinkageObservable: result.Operations.LinkedIncidentCount > 0,
		BusinessSLIObservable:     result.Business.SLISampleCount > 0,
		MissingSignals: outcomeMissingSignals(
			result.Flow.DeploymentOutcomeSampleCount,
			result.Operations.RollbackOutcomeSampleCount,
			result.Operations.LinkedIncidentCount,
			result.Business.SLISampleCount,
		),
	}
	return result
}

func deploymentOutcome(value string) (failed bool, terminal bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SUCCESS", "SUCCEEDED", "PASSED":
		return false, true
	case "FAILURE", "FAILED", "ABORTED", "UNSTABLE", "CANCELED", "CANCELLED":
		return true, true
	default:
		return false, false
	}
}

func integrationEventTime(event model.IntegrationEvent) time.Time {
	if !event.OccurredAt.IsZero() {
		return event.OccurredAt.UTC()
	}
	return event.ReceivedAt.UTC()
}

func rollbackOutcome(value string) (failed bool, terminal bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SUCCEEDED":
		return false, true
	case "FAILED", "CANCELED", "CANCELLED":
		return true, true
	default:
		return false, false
	}
}

func isResolvedIncident(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	return value == "RESOLVED" || value == "CLOSED"
}

func rememberEarliest(values map[string]time.Time, key string, value time.Time) {
	current, exists := values[key]
	if !exists || value.Before(current) {
		values[key] = value
	}
}

func businessSLIOutcome(signal model.OutcomeSignal) int {
	if signal.BaselineValue == nil || signal.ObservedValue == nil {
		return 0
	}
	tolerance := 0.0
	if signal.Tolerance != nil {
		tolerance = *signal.Tolerance
	}
	difference := *signal.ObservedValue - *signal.BaselineValue
	if math.Abs(difference) <= tolerance {
		return 0
	}
	if signal.MetricDirection == model.MetricLowerIsBetter {
		difference = -difference
	}
	if difference > 0 {
		return 1
	}
	return -1
}

func businessObjectiveMet(signal model.OutcomeSignal) bool {
	if signal.ObservedValue == nil || signal.ObjectiveValue == nil {
		return false
	}
	if signal.MetricDirection == model.MetricLowerIsBetter {
		return *signal.ObservedValue <= *signal.ObjectiveValue
	}
	return *signal.ObservedValue >= *signal.ObjectiveValue
}

func businessSLIBracketsDeployment(signal model.OutcomeSignal, deployedAt time.Time) bool {
	if signal.BaselineWindowEnd == nil || signal.ObservationWindowStart == nil || deployedAt.IsZero() {
		return false
	}
	return !signal.BaselineWindowEnd.After(deployedAt.Add(5*time.Minute)) &&
		!signal.ObservationWindowStart.Before(deployedAt.Add(-5*time.Minute))
}

func outcomeMissingSignals(deploymentSamples, rollbackSamples, incidentSamples, businessSamples int) []string {
	missing := make([]string, 0, 4)
	if deploymentSamples == 0 {
		missing = append(missing, "linked terminal deployment outcome")
	}
	if rollbackSamples == 0 {
		missing = append(missing, "terminal rollback execution outcome")
	}
	if incidentSamples == 0 {
		missing = append(missing, "incident-to-change linkage")
	}
	if businessSamples == 0 {
		missing = append(missing, "pre/post deployment business SLI comparison")
	}
	return missing
}

func changeDecisionAt(change model.ChangeRequest) (time.Time, bool) {
	var decision time.Time
	for _, item := range change.Timeline {
		if item.Status != model.StatusApproved && item.Status != model.StatusRejected {
			continue
		}
		if decision.IsZero() || item.CreatedAt.Before(decision) {
			decision = item.CreatedAt.UTC()
		}
	}
	return decision, !decision.IsZero()
}

func isProductionEnvironment(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(normalized, "生产") {
		return true
	}
	for _, token := range strings.FieldsFunc(normalized, func(current rune) bool {
		return current < 'a' || current > 'z'
	}) {
		if token == "production" || token == "prod" {
			return true
		}
	}
	return false
}

func isProgressiveDelivery(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	return normalized != "" && !strings.Contains(normalized, "全量") && !strings.Contains(normalized, "full")
}

func percentage(numerator, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	return roundTwo(float64(numerator) / float64(denominator) * 100)
}

func roundTwo(value float64) float64 {
	return math.Round(value*100) / 100
}

func validRuneLength(value string, minLength, maxLength int) bool {
	length := len([]rune(value))
	return length >= minLength && length <= maxLength
}
