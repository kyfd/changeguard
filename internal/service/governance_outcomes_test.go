package service

import (
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestGovernanceOutcomesUsesAuditableWorkflowEvidence(t *testing.T) {
	now := time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC)
	firstCreated := now.Add(-2 * time.Hour)
	firstDue := now.Add(time.Hour)
	firstVerified := now.Add(-30 * time.Minute)
	secondCreated := now.Add(-3 * time.Hour)
	secondDue := now.Add(-time.Hour)
	changes := []model.ChangeRequest{
		{
			ID: "chg_accepted", Environment: "生产环境", CreatedAt: firstCreated, Status: model.StatusCompleted, Risk: model.RiskHigh,
			ArtifactSHA256: "artifact", Artifacts: []model.ChangeArtifact{{ID: "a1", Kind: model.ArtifactCode}},
			RollbackPlan: "restore image", ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", AutoRollback: true, SuccessMetrics: []string{"order success"}},
			CheckRun: &model.CheckRun{ID: "check_1"}, Experiment: &model.ExperimentReport{Status: "PASSED"},
			Timeline: []model.TimelineEntry{{Status: model.StatusApproved, CreatedAt: firstCreated.Add(time.Hour)}},
			Findings: []model.Finding{{Blocking: true, Status: model.FindingVerified, DueAt: &firstDue, VerifiedAt: &firstVerified}},
		},
		{
			ID: "chg_rejected", Environment: "production", CreatedAt: secondCreated, Status: model.StatusRejected, Risk: model.RiskMedium,
			ReleasePlan: model.ReleasePlan{Strategy: "全量发布"}, Experiment: &model.ExperimentReport{Status: "FAILED"},
			Timeline: []model.TimelineEntry{{Status: model.StatusRejected, CreatedAt: secondCreated.Add(2 * time.Hour)}},
			Findings: []model.Finding{{Blocking: true, Status: model.FindingAssigned, DueAt: &secondDue}},
		},
		{ID: "chg_old", Environment: "生产环境", CreatedAt: now.Add(-31 * 24 * time.Hour), Status: model.StatusCompleted},
	}
	events := []model.IntegrationEvent{
		{ChangeID: "chg_accepted", Status: "RUNNING", ReceivedAt: now.Add(-90 * time.Minute)},
		{ChangeID: "chg_accepted", Status: "SUCCESS", ReceivedAt: now.Add(-20 * time.Minute)},
		{ChangeID: "chg_rejected", Status: "FAILURE", ReceivedAt: now.Add(-10 * time.Minute)},
		{ChangeID: "chg_old", Status: "FAILURE", ReceivedAt: now.Add(-5 * time.Minute)},
	}

	result := governanceOutcomesForEvidence(changes, events, nil, 30, now, "test")
	if result.TotalChanges != 2 || result.ProductionChanges != 2 || result.CompletedChanges != 1 || result.AcceptedDecisions != 1 || result.RejectedDecisions != 1 {
		t.Fatalf("unexpected change outcomes: %+v", result)
	}
	if result.HighRiskChanges != 1 || result.BlockingFindings != 2 || result.OpenBlockingFindings != 1 || result.VerifiedFindings != 1 || result.OverdueFindings != 1 {
		t.Fatalf("unexpected finding outcomes: %+v", result)
	}
	coverage := result.ControlCoverage
	if coverage.CheckRunPercent != 50 || coverage.ArtifactEvidencePercent != 50 || coverage.RollbackPlanPercent != 50 || coverage.SuccessMetricsPercent != 50 || coverage.ProgressiveDeliveryPercent != 50 || coverage.AutoRollbackPercent != 50 {
		t.Fatalf("unexpected control coverage: %+v", coverage)
	}
	flow := result.Flow
	if flow.DecisionLeadTimeSampleCount != 2 || flow.AverageDecisionLeadMinutes != 90 || flow.ExperimentPassRatePercent != 50 || flow.DeploymentOutcomeSampleCount != 2 || flow.SuccessfulDeployments != 1 || flow.FailedDeployments != 1 || flow.DeploymentFailureRate != 50 || flow.BlockingFindingClosureRate != 50 || flow.OnTimeFindingClosureRate != 50 {
		t.Fatalf("unexpected flow metrics: %+v", flow)
	}
	if !result.OutcomeDataQuality.ReleaseOutcomeObservable || len(result.OutcomeDataQuality.MissingSignals) != 3 {
		t.Fatalf("business outcome limitations must remain explicit: %+v", result.OutcomeDataQuality)
	}
}

func TestGovernanceOutcomesAvoidsFalseProductionMatches(t *testing.T) {
	now := time.Now().UTC()
	changes := []model.ChangeRequest{{Environment: "preproduction", CreatedAt: now.Add(-time.Hour)}}
	result := governanceOutcomesForEvidence(changes, nil, nil, 30, now, "test")
	if result.ProductionChanges != 0 {
		t.Fatalf("preproduction must not be counted as production: %+v", result)
	}
	if result.OutcomeDataQuality.ReleaseOutcomeObservable || len(result.OutcomeDataQuality.MissingSignals) != 4 {
		t.Fatalf("missing deployment samples must remain explicit: %+v", result.OutcomeDataQuality)
	}
}

func TestDeploymentOutcomeUsesLatestTerminalState(t *testing.T) {
	now := time.Now().UTC()
	changes := []model.ChangeRequest{{ID: "chg_1", Environment: "prod", CreatedAt: now.Add(-time.Hour)}}
	events := []model.IntegrationEvent{
		{ChangeID: "chg_1", Status: "FAILURE", ReceivedAt: now.Add(-40 * time.Minute)},
		{ChangeID: "chg_1", Status: "RUNNING", ReceivedAt: now.Add(-30 * time.Minute)},
		{ChangeID: "chg_1", Status: "SUCCESS", ReceivedAt: now.Add(-20 * time.Minute)},
	}
	result := governanceOutcomesForEvidence(changes, events, nil, 30, now, "test")
	if result.Flow.DeploymentOutcomeSampleCount != 1 || result.Flow.SuccessfulDeployments != 1 || result.Flow.FailedDeployments != 0 {
		t.Fatalf("latest terminal deployment state was not selected: %+v", result.Flow)
	}
}

func TestGovernanceOutcomesUsesExternalOperationsEvidence(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	change := model.ChangeRequest{ID: "chg_release", Environment: "production", CreatedAt: now.Add(-5 * time.Hour), Status: model.StatusCompleted}
	deployments := []model.IntegrationEvent{{ChangeID: change.ID, Status: "SUCCESS", ReceivedAt: now.Add(-4 * time.Hour)}}
	baseline, baselineEnd := now.Add(-7*time.Hour), now.Add(-6*time.Hour)
	observation, observationEnd := now.Add(-2*time.Hour), now.Add(-90*time.Minute)
	baselineValue, observedValue, objectiveValue, tolerance := 2.0, 1.0, 1.2, 0.05
	signals := []model.OutcomeSignal{
		{Source: "SERVICENOW", Kind: model.OutcomeSignalIncident, Status: "OPEN", ChangeID: change.ID, IncidentID: "INC-42", OccurredAt: now.Add(-210 * time.Minute)},
		{Source: "SERVICENOW", Kind: model.OutcomeSignalIncident, Status: "ACKNOWLEDGED", ChangeID: change.ID, IncidentID: "INC-42", OccurredAt: now.Add(-180 * time.Minute)},
		{Source: "SERVICENOW", Kind: model.OutcomeSignalIncident, Status: "RESOLVED", ChangeID: change.ID, IncidentID: "INC-42", OccurredAt: now.Add(-120 * time.Minute)},
		{Source: "ARGOCD", Kind: model.OutcomeSignalRollback, Status: "STARTED", ChangeID: change.ID, OperationID: "rollback-42", OccurredAt: now.Add(-225 * time.Minute)},
		{Source: "ARGOCD", Kind: model.OutcomeSignalRollback, Status: "SUCCEEDED", ChangeID: change.ID, OperationID: "rollback-42", OccurredAt: now.Add(-200 * time.Minute)},
		{
			Source: "PROMETHEUS", Kind: model.OutcomeSignalBusinessSLI, Status: "OBSERVED", ChangeID: change.ID,
			MetricName: "checkout_error_rate", MetricUnit: "percent", MetricDirection: model.MetricLowerIsBetter,
			BaselineValue: &baselineValue, ObservedValue: &observedValue, ObjectiveValue: &objectiveValue, Tolerance: &tolerance,
			BaselineWindowStart: &baseline, BaselineWindowEnd: &baselineEnd,
			ObservationWindowStart: &observation, ObservationWindowEnd: &observationEnd,
			OccurredAt: now.Add(-time.Hour),
		},
	}

	result := governanceOutcomesForEvidence([]model.ChangeRequest{change}, deployments, signals, 30, now, "test")
	operations := result.Operations
	if operations.RollbackOutcomeSampleCount != 1 || operations.SuccessfulRollbacks != 1 || operations.FailedRollbacks != 0 || operations.RollbackSuccessRatePercent != 100 {
		t.Fatalf("unexpected rollback outcomes: %+v", operations)
	}
	if operations.LinkedIncidentCount != 1 || operations.OpenIncidents != 0 || operations.ResolvedIncidents != 1 || operations.IncidentResolutionSampleCount != 1 || operations.AverageIncidentResolutionMinutes != 90 {
		t.Fatalf("unexpected incident outcomes: %+v", operations)
	}
	if operations.PostReleaseSampleCount != 1 || operations.RemediationRequiredDeployments != 1 || operations.PostReleaseRemediationRate != 100 {
		t.Fatalf("unexpected post-release outcomes: %+v", operations)
	}
	if result.Business.SLISampleCount != 1 || result.Business.ImprovedSLIs != 1 || result.Business.ObjectiveSampleCount != 1 || result.Business.ObjectivesMet != 1 || result.Business.ObjectiveAttainmentRatePercent != 100 {
		t.Fatalf("unexpected business SLI outcomes: %+v", result.Business)
	}
	quality := result.OutcomeDataQuality
	if !quality.ReleaseOutcomeObservable || !quality.RollbackOutcomeObservable || !quality.IncidentLinkageObservable || !quality.BusinessSLIObservable || len(quality.MissingSignals) != 0 {
		t.Fatalf("all external outcome evidence should be observable: %+v", quality)
	}
}

func TestGovernanceOutcomesDoesNotTreatStartedRollbackAsTerminalOutcome(t *testing.T) {
	now := time.Now().UTC()
	change := model.ChangeRequest{ID: "chg_started", Environment: "prod", CreatedAt: now.Add(-2 * time.Hour)}
	deployments := []model.IntegrationEvent{{ChangeID: change.ID, Status: "SUCCESS", ReceivedAt: now.Add(-90 * time.Minute)}}
	signals := []model.OutcomeSignal{{Source: "ARGOCD", Kind: model.OutcomeSignalRollback, Status: "STARTED", ChangeID: change.ID, OperationID: "rollback-started", OccurredAt: now.Add(-time.Hour)}}
	result := governanceOutcomesForEvidence([]model.ChangeRequest{change}, deployments, signals, 30, now, "test")
	if result.Operations.RollbackOutcomeSampleCount != 0 || result.OutcomeDataQuality.RollbackOutcomeObservable {
		t.Fatalf("started rollback must not be reported as a terminal outcome: %+v", result)
	}
	if result.Operations.RemediationRequiredDeployments != 1 {
		t.Fatalf("an explicit rollback start still proves post-release remediation: %+v", result.Operations)
	}
}

func TestGovernanceOutcomesHandlesIncidentReopenAsSeparateResolutionEpisode(t *testing.T) {
	now := time.Now().UTC()
	change := model.ChangeRequest{ID: "chg_incident", Environment: "prod", CreatedAt: now.Add(-6 * time.Hour)}
	signals := []model.OutcomeSignal{
		{Source: "PAGERDUTY", Kind: model.OutcomeSignalIncident, Status: "OPEN", ChangeID: change.ID, IncidentID: "INC-7", OccurredAt: now.Add(-5 * time.Hour)},
		{Source: "PAGERDUTY", Kind: model.OutcomeSignalIncident, Status: "RESOLVED", ChangeID: change.ID, IncidentID: "INC-7", OccurredAt: now.Add(-4 * time.Hour)},
		{Source: "PAGERDUTY", Kind: model.OutcomeSignalIncident, Status: "TRIGGERED", ChangeID: change.ID, IncidentID: "INC-7", OccurredAt: now.Add(-3 * time.Hour)},
	}
	result := governanceOutcomesForEvidence([]model.ChangeRequest{change}, nil, signals, 30, now, "test")
	operations := result.Operations
	if operations.LinkedIncidentCount != 1 || operations.OpenIncidents != 1 || operations.ResolvedIncidents != 0 || operations.IncidentResolutionSampleCount != 1 || operations.AverageIncidentResolutionMinutes != 60 {
		t.Fatalf("unexpected reopened incident metrics: %+v", operations)
	}
}

func TestGovernanceOutcomesUsesLatestReleaseBracketedSLIOnly(t *testing.T) {
	now := time.Now().UTC()
	change := model.ChangeRequest{ID: "chg_sli", Environment: "prod", CreatedAt: now.Add(-6 * time.Hour)}
	deployedAt := now.Add(-3 * time.Hour)
	deployments := []model.IntegrationEvent{{ChangeID: change.ID, Status: "SUCCESS", OccurredAt: deployedAt, ReceivedAt: deployedAt.Add(time.Minute)}}
	baselineStart, baselineEnd := now.Add(-5*time.Hour), now.Add(-4*time.Hour)
	observationStart, observationEnd := now.Add(-2*time.Hour), now.Add(-90*time.Minute)
	baseline, firstObserved, latestObserved, objective := 100.0, 90.0, 105.0, 100.0
	signals := []model.OutcomeSignal{
		{
			Source: "PROMETHEUS", Kind: model.OutcomeSignalBusinessSLI, Status: "OBSERVED", ChangeID: change.ID,
			MetricName: "orders_per_minute", MetricUnit: "count", MetricDirection: model.MetricHigherIsBetter,
			BaselineValue: &baseline, ObservedValue: &firstObserved, ObjectiveValue: &objective,
			BaselineWindowStart: &baselineStart, BaselineWindowEnd: &baselineEnd,
			ObservationWindowStart: &observationStart, ObservationWindowEnd: &observationEnd,
			OccurredAt: now.Add(-time.Hour),
		},
		{
			Source: "PROMETHEUS", Kind: model.OutcomeSignalBusinessSLI, Status: "OBSERVED", ChangeID: change.ID,
			MetricName: "orders_per_minute", MetricUnit: "count", MetricDirection: model.MetricHigherIsBetter,
			BaselineValue: &baseline, ObservedValue: &latestObserved, ObjectiveValue: &objective,
			BaselineWindowStart: &baselineStart, BaselineWindowEnd: &baselineEnd,
			ObservationWindowStart: &observationStart, ObservationWindowEnd: &observationEnd,
			OccurredAt: now.Add(-30 * time.Minute),
		},
	}
	result := governanceOutcomesForEvidence([]model.ChangeRequest{change}, deployments, signals, 30, now, "test")
	if result.Business.SLISampleCount != 1 || result.Business.ImprovedSLIs != 1 || result.Business.DegradedSLIs != 0 || result.Business.ObjectivesMet != 1 {
		t.Fatalf("only the latest comparison for a change/source/metric should count: %+v", result.Business)
	}

	withoutDeployment := governanceOutcomesForEvidence([]model.ChangeRequest{change}, nil, signals, 30, now, "test")
	if withoutDeployment.Business.SLISampleCount != 0 || withoutDeployment.OutcomeDataQuality.BusinessSLIObservable {
		t.Fatalf("SLI evidence without a successful linked release must remain non-observable: %+v", withoutDeployment)
	}
}
