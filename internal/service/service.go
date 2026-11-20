package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kyfd/changeguard/internal/agent"
	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/conflict"
	"github.com/kyfd/changeguard/internal/experiment"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/report"
	"github.com/kyfd/changeguard/internal/store"
)

var (
	ErrInvalidState = errors.New("当前状态不允许执行该操作")
	ErrForbidden    = errors.New("没有权限执行该操作")
	ErrValidation   = errors.New("请求参数不完整")
	// ErrUntrustedAgentTask：Agent 任务关联无法核对通过，不能建立可信来源。
	ErrUntrustedAgentTask = errors.New("无法核对 Agent 任务来源")
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

func (s *Service) Dashboard() model.Dashboard { return s.store.Dashboard() }

func (s *Service) Applications() []model.Application { return s.store.Applications() }

func (s *Service) Users() []model.User { return s.store.Users() }

func (s *Service) Changes() []model.ChangeRequest { return s.store.Changes() }

func (s *Service) Change(id string) (model.ChangeRequest, error) { return s.store.Change(id) }

func (s *Service) Audits(limit int) []model.AuditEvent { return s.store.Audits(limit) }

func (s *Service) Policies() []model.RiskPolicy { return s.store.Policies() }

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

// ApplicationAccess 判断某个成员是否可以使用（view）指定应用。
//
// 供 Agent 后端在"按应用访问项目知识"前做**服务端授权核对**：应用 ID 由调用方声明，
// 声明本身不产生权限，必须回查成员是否存在、组织是否一致、以及应用授权。
// 返回 (应用, 是否允许)；不存在或跨组织时返回 false，不区分，避免探测。
func (s *Service) ApplicationAccess(applicationID, actorID string) (model.Application, bool) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.Application{}, false
	}
	application, err := s.store.Application(strings.TrimSpace(applicationID))
	if err != nil || application.OrganizationID != actor.OrganizationID {
		return model.Application{}, false
	}
	return application, s.canUseApplication(actor, application.ID, "view")
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

func (s *Service) OutcomeSignalsForChange(changeID, actorID string) ([]model.OutcomeSignal, error) {
	change, err := s.ChangeFor(changeID, actorID)
	if err != nil {
		return nil, err
	}
	return s.store.OutcomeSignalsByChange(change.OrganizationID, change.ID), nil
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

func (s *Service) Health(ctx context.Context) error { return s.store.Health(ctx) }

func (s *Service) StoreMode() string { return s.store.Mode() }

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
		WindowDays: windowDays, WindowStartedAt: windowStart, GeneratedAt: now,
		DefinitionVersion: "governance-outcomes-v3", SourceWatermark: now, Scope: scope,
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
