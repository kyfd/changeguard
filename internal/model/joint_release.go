package model

import "time"

const (
	ReleaseDeploymentPending  = "NOT_RUN"
	ReleaseDeploymentSuccess  = "SUCCESS"
	ReleaseDeploymentFailed   = "FAILED"
	ReleaseDeploymentCanceled = "CANCELED"
)

// JointRelease is separate from the rollout instructions on a single change.
type JointRelease struct {
	ID             string             `json:"id"`
	OrganizationID string             `json:"organization_id"`
	Name           string             `json:"name"`
	Environment    string             `json:"environment"`
	CreatedBy      string             `json:"created_by"`
	CreatedAt      time.Time          `json:"created_at"`
	Supersedes     string             `json:"supersedes,omitempty"`
	SupersededBy   string             `json:"superseded_by,omitempty"`
	Steps          []JointReleaseStep `json:"steps"`
}

type JointReleaseInput struct {
	Name        string           `json:"name"`
	Environment string           `json:"environment"`
	Supersedes  string           `json:"supersedes,omitempty"`
	Steps       []ReleaseStepPin `json:"steps"`
}

type ReleaseStepPin struct {
	ChangeID         string `json:"change_id"`
	ArtifactVersion  int    `json:"artifact_version"`
	ArtifactSHA256   string `json:"artifact_sha256"`
	ProjectID        int64  `json:"gitlab_project_id"`
	JobName          string `json:"gitlab_job"`
	ReplacesChangeID string `json:"replaces_change_id,omitempty"`
}

type JointReleaseStep struct {
	ReleaseStepPin
	InheritedFrom string              `json:"inherited_from,omitempty"`
	Title         string              `json:"title"`
	ApplicationID string              `json:"application_id"`
	CommitSHA     string              `json:"commit_sha"`
	Source        string              `json:"gitlab_source"`
	ProjectURL    string              `json:"gitlab_project_url"`
	Consumption   *ReleaseConsumption `json:"consumption,omitempty"`
	Deployment    *GitLabDeployment   `json:"deployment,omitempty"`
}

type ReleaseConsumption struct {
	PassportID string    `json:"passport_id"`
	At         time.Time `json:"at"`
	JobID      int64     `json:"job_id"`
	PipelineID int64     `json:"pipeline_id"`
}

// GitLabJob is populated only after ChangeGuard authenticates the token against
// its configured GitLab /api/v4/job endpoint. It is not part of the Gate JSON.
type GitLabJob struct {
	Source, OrganizationID, Name, CommitSHA string
	Environment                             string
	ProjectID, JobID, PipelineID            int64
	StartedAt                               time.Time
}

type GitLabDeployment struct {
	Source         string    `json:"source"`
	OrganizationID string    `json:"organization_id"`
	ProjectID      int64     `json:"project_id"`
	ProjectURL     string    `json:"project_url"`
	JobID          int64     `json:"job_id"`
	DeploymentID   int64     `json:"deployment_id"`
	CommitSHA      string    `json:"commit_sha"`
	Environment    string    `json:"environment"`
	Status         string    `json:"status"`
	OccurredAt     time.Time `json:"occurred_at"`
	ReceivedAt     time.Time `json:"received_at"`
}

type ReleaseSource struct {
	OrganizationID string
	URL            string
	Projects       map[int64]string // project ID -> canonical GitLab project URL
}

type JointReleaseView struct {
	JointRelease
	Status   string              `json:"status"`
	Timeline []ReleaseStepStatus `json:"timeline"`
}

type ReleaseStepStatus struct {
	ChangeID         string       `json:"change_id"`
	ApprovalStatus   ChangeStatus `json:"approval_status"`
	ReviewerName     string       `json:"reviewer_name,omitempty"`
	GateStatus       string       `json:"gate_status"`
	DeploymentStatus string       `json:"deployment_status"`
	BlockingCause    string       `json:"blocking_cause,omitempty"`
}

func (c ChangeRequest) MaterialVersion() int {
	if c.ArtifactVersion < 1 {
		return 1 // records written before joint releases had one implicit revision
	}
	return c.ArtifactVersion
}
