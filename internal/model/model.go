package model

import (
	"encoding/json"
	"time"
)

type RiskLevel string

const (
	RiskUnknown RiskLevel = "UNKNOWN"
	RiskLow     RiskLevel = "LOW"
	RiskMedium  RiskLevel = "MEDIUM"
	RiskHigh    RiskLevel = "HIGH"
)

type FindingStatus string

const (
	FindingOpen     FindingStatus = "OPEN"
	FindingAssigned FindingStatus = "ASSIGNED"
	FindingResolved FindingStatus = "RESOLVED"
	FindingVerified FindingStatus = "VERIFIED"
)

type ChangeStatus string

const (
	StatusDraft              ChangeStatus = "DRAFT"
	StatusChecking           ChangeStatus = "CHECKING"
	StatusCheckFailed        ChangeStatus = "CHECK_FAILED"
	StatusReadyForExperiment ChangeStatus = "READY_FOR_EXPERIMENT"
	StatusExperimentQueued   ChangeStatus = "EXPERIMENT_QUEUED"
	StatusExperimentRunning  ChangeStatus = "EXPERIMENT_RUNNING"
	StatusWaitingApproval    ChangeStatus = "WAITING_APPROVAL"
	StatusApproved           ChangeStatus = "APPROVED"
	StatusRejected           ChangeStatus = "REJECTED"
	StatusCompleted          ChangeStatus = "COMPLETED"
)

type Application struct {
	OrganizationID string   `json:"organization_id"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Owner          string   `json:"owner"`
	Kind           string   `json:"kind"`
	Runtime        string   `json:"runtime"`
	RepositoryURL  string   `json:"repository_url,omitempty"`
	Tier           string   `json:"tier"`
	Lifecycle      string   `json:"lifecycle"`
	Database       string   `json:"database"`
	Schema         string   `json:"schema"`
	Environment    string   `json:"environment"`
	Dependencies   []string `json:"dependencies,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Description    string   `json:"description"`
}

type SaveApplicationInput struct {
	Name          string   `json:"name"`
	Owner         string   `json:"owner"`
	Kind          string   `json:"kind"`
	Runtime       string   `json:"runtime"`
	RepositoryURL string   `json:"repository_url"`
	Tier          string   `json:"tier"`
	Lifecycle     string   `json:"lifecycle"`
	Database      string   `json:"database"`
	Schema        string   `json:"schema"`
	Environment   string   `json:"environment"`
	Dependencies  []string `json:"dependencies"`
	Tags          []string `json:"tags"`
	Description   string   `json:"description"`
}

type ArtifactKind string

const (
	ArtifactCode       ArtifactKind = "CODE"
	ArtifactConfig     ArtifactKind = "CONFIG"
	ArtifactKubernetes ArtifactKind = "KUBERNETES"
	ArtifactAPI        ArtifactKind = "API"
	ArtifactDatabase   ArtifactKind = "DATABASE"
)

type ChangeArtifact struct {
	ID            string       `json:"id"`
	Kind          ArtifactKind `json:"kind"`
	Name          string       `json:"name"`
	Source        string       `json:"source,omitempty"`
	Language      string       `json:"language,omitempty"`
	Content       string       `json:"content"`
	ContentSHA256 string       `json:"content_sha256"`
}

type ReleasePlan struct {
	Strategy           string   `json:"strategy"`
	CanaryPercent      int      `json:"canary_percent,omitempty"`
	ObservationMinutes int      `json:"observation_minutes"`
	AutoRollback       bool     `json:"auto_rollback"`
	SuccessMetrics     []string `json:"success_metrics,omitempty"`
}

type User struct {
	ID               string     `json:"id"`
	OrganizationID   string     `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	Name             string     `json:"name"`
	Email            string     `json:"email,omitempty"`
	Role             string     `json:"role"`
	EnterpriseAdmin  bool       `json:"enterprise_admin"`
	Active           bool       `json:"active"`
	IdentityProvider string     `json:"identity_provider,omitempty"`
	Subject          string     `json:"-"`
	LastLoginAt      *time.Time `json:"last_login_at,omitempty"`
}

type Finding struct {
	ID                  string        `json:"id"`
	Code                string        `json:"code"`
	Severity            RiskLevel     `json:"severity"`
	Title               string        `json:"title"`
	Detail              string        `json:"detail"`
	Evidence            string        `json:"evidence"`
	Suggestion          string        `json:"suggestion"`
	Blocking            bool          `json:"blocking"`
	RuleVersion         int           `json:"rule_version"`
	Status              FindingStatus `json:"status"`
	OwnerID             string        `json:"owner_id,omitempty"`
	OwnerName           string        `json:"owner_name,omitempty"`
	DueAt               *time.Time    `json:"due_at,omitempty"`
	Resolution          string        `json:"resolution,omitempty"`
	ResolvedAt          *time.Time    `json:"resolved_at,omitempty"`
	VerifiedByID        string        `json:"verified_by_id,omitempty"`
	VerifiedByName      string        `json:"verified_by_name,omitempty"`
	VerificationComment string        `json:"verification_comment,omitempty"`
	VerifiedAt          *time.Time    `json:"verified_at,omitempty"`
	UpdatedAt           time.Time     `json:"updated_at"`
}

type Evidence struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Value      string    `json:"value"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

type ExperimentReport struct {
	ID                 string     `json:"id"`
	Kind               string     `json:"kind,omitempty"`
	Mode               string     `json:"mode"`
	Status             string     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         time.Time  `json:"finished_at"`
	DurationMS         int64      `json:"duration_ms"`
	DatasetRows        int64      `json:"dataset_rows"`
	LockWaitMS         int64      `json:"lock_wait_ms"`
	P99BeforeMS        float64    `json:"p99_before_ms"`
	P99AfterMS         float64    `json:"p99_after_ms"`
	FailedTransactions int        `json:"failed_transactions"`
	RollbackVerified   bool       `json:"rollback_verified"`
	ChecksTotal        int        `json:"checks_total,omitempty"`
	ChecksPassed       int        `json:"checks_passed,omitempty"`
	Strategy           string     `json:"strategy,omitempty"`
	CanaryPercent      int        `json:"canary_percent,omitempty"`
	ObservationMinutes int        `json:"observation_minutes,omitempty"`
	ExecutionError     string     `json:"execution_error,omitempty"`
	ArtifactSHA256     string     `json:"artifact_sha256,omitempty"`
	RuleSetVersion     string     `json:"rule_set_version,omitempty"`
	Evidence           []Evidence `json:"evidence"`
}

// AgentToolCallRecord is the auditable summary of a single allow-listed tool
// invocation made by the agent runtime. Arguments and results are compacted by
// the runtime before persistence so the change record does not become a covert
// data store for prompts, credentials, or large tool responses.
type AgentToolCallRecord struct {
	Name          string `json:"name"`
	Args          string `json:"args,omitempty"`
	ResultSummary string `json:"result_summary,omitempty"`
	Error         string `json:"error,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
}

type AgentAnalysis struct {
	Provider           string                `json:"provider"`
	Model              string                `json:"model,omitempty"`
	Risk               RiskLevel             `json:"risk"`
	Summary            string                `json:"summary"`
	Reasons            []string              `json:"reasons"`
	Suggestions        []string              `json:"suggestions"`
	EvidenceIDs        []string              `json:"evidence_ids"`
	Steps              int                   `json:"steps"`
	ToolCalls          int                   `json:"tool_calls"`
	Tokens             int                   `json:"tokens,omitempty"`
	TraceID            string                `json:"trace_id,omitempty"`
	InjectionSuspected bool                  `json:"injection_suspected,omitempty"`
	ToolCallLog        []AgentToolCallRecord `json:"tool_call_log,omitempty"`
	GeneratedAt        time.Time             `json:"generated_at"`
}

type TimelineEntry struct {
	ID        string       `json:"id"`
	Status    ChangeStatus `json:"status"`
	Title     string       `json:"title"`
	Detail    string       `json:"detail"`
	Actor     string       `json:"actor"`
	CreatedAt time.Time    `json:"created_at"`
}

type ChangeRequest struct {
	OrganizationID  string            `json:"organization_id"`
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	ApplicationID   string            `json:"application_id"`
	ApplicationName string            `json:"application_name"`
	Environment     string            `json:"environment"`
	ChangeType      string            `json:"change_type"`
	RepositoryURL   string            `json:"repository_url,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	CommitSHA       string            `json:"commit_sha,omitempty"`
	ArtifactSHA256  string            `json:"artifact_sha256"`
	SQLSHA256       string            `json:"sql_sha256,omitempty"`
	RollbackSHA256  string            `json:"rollback_sha256,omitempty"`
	RuleSetVersion  string            `json:"rule_set_version,omitempty"`
	Artifacts       []ChangeArtifact  `json:"artifacts,omitempty"`
	SQL             string            `json:"sql"`
	RollbackSQL     string            `json:"rollback_sql"`
	RollbackPlan    string            `json:"rollback_plan,omitempty"`
	ReleasePlan     ReleasePlan       `json:"release_plan"`
	Description     string            `json:"description"`
	SubmitterID     string            `json:"submitter_id"`
	SubmitterName   string            `json:"submitter_name"`
	ReviewerID      string            `json:"reviewer_id,omitempty"`
	ReviewerName    string            `json:"reviewer_name,omitempty"`
	ReviewComment   string            `json:"review_comment,omitempty"`
	Status          ChangeStatus      `json:"status"`
	Risk            RiskLevel         `json:"risk"`
	PlannedAt       time.Time         `json:"planned_at"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Version         int               `json:"version"`
	Findings        []Finding         `json:"findings"`
	CheckRun        *CheckRun         `json:"check_run,omitempty"`
	Experiment      *ExperimentReport `json:"experiment,omitempty"`
	Analysis        *AgentAnalysis    `json:"analysis,omitempty"`
	Timeline        []TimelineEntry   `json:"timeline"`
	Comments        []ChangeComment   `json:"comments"`
}

type ChangeComment struct {
	ID         string    `json:"id"`
	AuthorID   string    `json:"author_id"`
	AuthorName string    `json:"author_name"`
	AuthorRole string    `json:"author_role"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

type AuditEvent struct {
	OrganizationID string    `json:"organization_id"`
	ID             string    `json:"id"`
	ChangeID       string    `json:"change_id,omitempty"`
	ActorID        string    `json:"actor_id"`
	ActorName      string    `json:"actor_name"`
	Action         string    `json:"action"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"created_at"`
}

type ConflictReason struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Resource string `json:"resource,omitempty"`
}

type ConflictChangeSummary struct {
	ID              string       `json:"id"`
	Title           string       `json:"title"`
	ApplicationID   string       `json:"application_id"`
	ApplicationName string       `json:"application_name"`
	Environment     string       `json:"environment"`
	Status          ChangeStatus `json:"status"`
	Risk            RiskLevel    `json:"risk"`
	PlannedAt       time.Time    `json:"planned_at"`
	WindowEnd       time.Time    `json:"window_end"`
}

type ChangeConflict struct {
	ID             string                `json:"id"`
	Severity       RiskLevel             `json:"severity"`
	Score          int                   `json:"score"`
	ChangeA        ConflictChangeSummary `json:"change_a"`
	ChangeB        ConflictChangeSummary `json:"change_b"`
	OverlapStart   time.Time             `json:"overlap_start"`
	OverlapEnd     time.Time             `json:"overlap_end"`
	OverlapMinutes int                   `json:"overlap_minutes"`
	Reasons        []ConflictReason      `json:"reasons"`
	Recommendation string                `json:"recommendation"`
}

type ConflictRadar struct {
	GeneratedAt          time.Time               `json:"generated_at"`
	WindowStart          time.Time               `json:"window_start"`
	WindowEnd            time.Time               `json:"window_end"`
	PlannedChanges       int                     `json:"planned_changes"`
	ConflictCount        int                     `json:"conflict_count"`
	HighRiskCount        int                     `json:"high_risk_count"`
	AffectedApplications int                     `json:"affected_applications"`
	SeverityDistribution map[RiskLevel]int       `json:"severity_distribution"`
	Changes              []ConflictChangeSummary `json:"changes"`
	Conflicts            []ChangeConflict        `json:"conflicts"`
}

type IntegrationEvent struct {
	OrganizationID string    `json:"organization_id"`
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	ExternalID     string    `json:"external_id"`
	EventType      string    `json:"event_type"`
	Status         string    `json:"status"`
	ChangeID       string    `json:"change_id,omitempty"`
	Project        string    `json:"project,omitempty"`
	Pipeline       string    `json:"pipeline,omitempty"`
	CommitSHA      string    `json:"commit_sha,omitempty"`
	ExternalURL    string    `json:"external_url,omitempty"`
	Detail         string    `json:"detail"`
	OccurredAt     time.Time `json:"occurred_at"`
	ReceivedAt     time.Time `json:"received_at"`
}

type IntegrationProviderStatus struct {
	Provider        string     `json:"provider"`
	Configured      bool       `json:"configured"`
	Authentication  string     `json:"authentication"`
	Endpoint        string     `json:"endpoint"`
	SupportedEvents []string   `json:"supported_events"`
	LastReceivedAt  *time.Time `json:"last_received_at,omitempty"`
}

type IntegrationStatus struct {
	GitLab     IntegrationProviderStatus `json:"gitlab"`
	Jenkins    IntegrationProviderStatus `json:"jenkins"`
	Operations IntegrationProviderStatus `json:"operations"`
}

type OutcomeSignalKind string

const (
	OutcomeSignalIncident    OutcomeSignalKind = "INCIDENT"
	OutcomeSignalRollback    OutcomeSignalKind = "ROLLBACK"
	OutcomeSignalBusinessSLI OutcomeSignalKind = "BUSINESS_SLI"
)

type OutcomeMetricDirection string

const (
	MetricHigherIsBetter OutcomeMetricDirection = "HIGHER_IS_BETTER"
	MetricLowerIsBetter  OutcomeMetricDirection = "LOWER_IS_BETTER"
)

// OutcomeSignal is evidence received from an external operations system after
// a release. It deliberately stores only linkage, state, and bounded metrics;
// alert payloads, log bodies, credentials, and customer data do not belong in
// the change record.
type OutcomeSignal struct {
	OrganizationID         string                 `json:"organization_id"`
	ID                     string                 `json:"id"`
	ExternalID             string                 `json:"external_id"`
	Source                 string                 `json:"source"`
	Kind                   OutcomeSignalKind      `json:"kind"`
	Status                 string                 `json:"status"`
	ChangeID               string                 `json:"change_id"`
	IncidentID             string                 `json:"incident_id,omitempty"`
	OperationID            string                 `json:"operation_id,omitempty"`
	Severity               string                 `json:"severity,omitempty"`
	MetricName             string                 `json:"metric_name,omitempty"`
	MetricUnit             string                 `json:"metric_unit,omitempty"`
	MetricDirection        OutcomeMetricDirection `json:"metric_direction,omitempty"`
	BaselineValue          *float64               `json:"baseline_value,omitempty"`
	ObservedValue          *float64               `json:"observed_value,omitempty"`
	ObjectiveValue         *float64               `json:"objective_value,omitempty"`
	Tolerance              *float64               `json:"tolerance,omitempty"`
	BaselineWindowStart    *time.Time             `json:"baseline_window_start,omitempty"`
	BaselineWindowEnd      *time.Time             `json:"baseline_window_end,omitempty"`
	ObservationWindowStart *time.Time             `json:"observation_window_start,omitempty"`
	ObservationWindowEnd   *time.Time             `json:"observation_window_end,omitempty"`
	ExternalURL            string                 `json:"external_url,omitempty"`
	Detail                 string                 `json:"detail,omitempty"`
	OccurredAt             time.Time              `json:"occurred_at"`
	ReceivedAt             time.Time              `json:"received_at"`
}

type CreateChangeInput struct {
	Title         string           `json:"title"`
	ApplicationID string           `json:"application_id"`
	Environment   string           `json:"environment"`
	ChangeType    string           `json:"change_type"`
	RepositoryURL string           `json:"repository_url"`
	Branch        string           `json:"branch"`
	CommitSHA     string           `json:"commit_sha"`
	Artifacts     []ChangeArtifact `json:"artifacts"`
	SQL           string           `json:"sql"`
	RollbackSQL   string           `json:"rollback_sql"`
	RollbackPlan  string           `json:"rollback_plan"`
	ReleasePlan   ReleasePlan      `json:"release_plan"`
	Description   string           `json:"description"`
	PlannedAt     time.Time        `json:"planned_at"`
}

type AssignFindingInput struct {
	OwnerID string    `json:"owner_id"`
	DueAt   time.Time `json:"due_at"`
}

type ResolveFindingInput struct {
	Resolution string `json:"resolution"`
}

type VerifyFindingInput struct {
	Approved bool   `json:"approved"`
	Comment  string `json:"comment"`
}

type Dashboard struct {
	PendingCount         int               `json:"pending_count"`
	ExperimentPassRate   float64           `json:"experiment_pass_rate"`
	HighRiskCount        int               `json:"high_risk_count"`
	AverageExperimentSec float64           `json:"average_experiment_sec"`
	RiskDistribution     map[RiskLevel]int `json:"risk_distribution"`
	RecentChanges        []ChangeRequest   `json:"recent_changes"`
	PendingApprovals     []ChangeRequest   `json:"pending_approvals"`
}

type GovernanceControlCoverage struct {
	CheckRunPercent            float64 `json:"check_run_percent"`
	ArtifactEvidencePercent    float64 `json:"artifact_evidence_percent"`
	RollbackPlanPercent        float64 `json:"rollback_plan_percent"`
	SuccessMetricsPercent      float64 `json:"success_metrics_percent"`
	ProgressiveDeliveryPercent float64 `json:"progressive_delivery_percent"`
	AutoRollbackPercent        float64 `json:"auto_rollback_percent"`
}

type GovernanceFlowMetrics struct {
	DecisionLeadTimeSampleCount  int     `json:"decision_lead_time_sample_count"`
	AverageDecisionLeadMinutes   float64 `json:"average_decision_lead_minutes"`
	ExperimentSampleCount        int     `json:"experiment_sample_count"`
	ExperimentPassRatePercent    float64 `json:"experiment_pass_rate_percent"`
	DeploymentOutcomeSampleCount int     `json:"deployment_outcome_sample_count"`
	SuccessfulDeployments        int     `json:"successful_deployments"`
	FailedDeployments            int     `json:"failed_deployments"`
	DeploymentFailureRate        float64 `json:"deployment_failure_rate_percent"`
	BlockingFindingClosureRate   float64 `json:"blocking_finding_closure_rate_percent"`
	DueFindingSampleCount        int     `json:"due_finding_sample_count"`
	OnTimeFindingClosureRate     float64 `json:"on_time_finding_closure_rate_percent"`
}

type GovernanceOperationalMetrics struct {
	RollbackOutcomeSampleCount       int     `json:"rollback_outcome_sample_count"`
	SuccessfulRollbacks              int     `json:"successful_rollbacks"`
	FailedRollbacks                  int     `json:"failed_rollbacks"`
	RollbackSuccessRatePercent       float64 `json:"rollback_success_rate_percent"`
	LinkedIncidentCount              int     `json:"linked_incident_count"`
	OpenIncidents                    int     `json:"open_incidents"`
	ResolvedIncidents                int     `json:"resolved_incidents"`
	IncidentResolutionSampleCount    int     `json:"incident_resolution_sample_count"`
	AverageIncidentResolutionMinutes float64 `json:"average_incident_resolution_minutes"`
	PostReleaseSampleCount           int     `json:"post_release_sample_count"`
	RemediationRequiredDeployments   int     `json:"remediation_required_deployments"`
	PostReleaseRemediationRate       float64 `json:"post_release_remediation_rate_percent"`
}

type GovernanceBusinessMetrics struct {
	SLISampleCount                 int     `json:"sli_sample_count"`
	ImprovedSLIs                   int     `json:"improved_slis"`
	StableSLIs                     int     `json:"stable_slis"`
	DegradedSLIs                   int     `json:"degraded_slis"`
	ObjectiveSampleCount           int     `json:"objective_sample_count"`
	ObjectivesMet                  int     `json:"objectives_met"`
	ObjectiveAttainmentRatePercent float64 `json:"objective_attainment_rate_percent"`
}

type GovernanceOutcomeDataQuality struct {
	ReleaseOutcomeObservable  bool     `json:"release_outcome_observable"`
	RollbackOutcomeObservable bool     `json:"rollback_outcome_observable"`
	IncidentLinkageObservable bool     `json:"incident_linkage_observable"`
	BusinessSLIObservable     bool     `json:"business_sli_observable"`
	MissingSignals            []string `json:"missing_signals"`
}

type GovernanceOutcomeSummary struct {
	WindowDays           int                          `json:"window_days"`
	WindowStartedAt      time.Time                    `json:"window_started_at"`
	GeneratedAt          time.Time                    `json:"generated_at"`
	Scope                string                       `json:"scope"`
	TotalChanges         int                          `json:"total_changes"`
	ProductionChanges    int                          `json:"production_changes"`
	CompletedChanges     int                          `json:"completed_changes"`
	AcceptedDecisions    int                          `json:"accepted_decisions"`
	RejectedDecisions    int                          `json:"rejected_decisions"`
	HighRiskChanges      int                          `json:"high_risk_changes"`
	TotalFindings        int                          `json:"total_findings"`
	BlockingFindings     int                          `json:"blocking_findings"`
	OpenBlockingFindings int                          `json:"open_blocking_findings"`
	VerifiedFindings     int                          `json:"verified_findings"`
	OverdueFindings      int                          `json:"overdue_findings"`
	ControlCoverage      GovernanceControlCoverage    `json:"control_coverage"`
	Flow                 GovernanceFlowMetrics        `json:"flow"`
	Operations           GovernanceOperationalMetrics `json:"operations"`
	Business             GovernanceBusinessMetrics    `json:"business"`
	OutcomeDataQuality   GovernanceOutcomeDataQuality `json:"outcome_data_quality"`
}

type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "PENDING"
	OutboxProcessing OutboxStatus = "PROCESSING"
	OutboxCompleted  OutboxStatus = "COMPLETED"
	OutboxDead       OutboxStatus = "DEAD"
)

type OutboxEvent struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organization_id"`
	AggregateType  string          `json:"aggregate_type"`
	AggregateID    string          `json:"aggregate_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Status         OutboxStatus    `json:"status"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	LockedBy       string          `json:"locked_by,omitempty"`
	LockedUntil    *time.Time      `json:"locked_until,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

type OperationsSummary struct {
	StoreMode        string     `json:"store_mode"`
	SessionMode      string     `json:"session_mode"`
	PendingEvents    int        `json:"pending_events"`
	ProcessingEvents int        `json:"processing_events"`
	DeadEvents       int        `json:"dead_events"`
	OldestPendingAt  *time.Time `json:"oldest_pending_at,omitempty"`
}
