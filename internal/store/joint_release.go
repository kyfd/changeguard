package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

var (
	ErrReleasePermission = errors.New("没有联合发布计划或相关应用的权限")
	ErrReleaseInput      = errors.New("联合发布计划参数无效")
	ErrReleaseBlocked    = errors.New("联合发布已阻断")
	ErrReleaseJob        = errors.New("需要与计划匹配且仍在运行的 GitLab 部署作业")
	ErrReleaseEvidence   = errors.New("部署证据未匹配当前计划中已消费的执行")
)

func (s *Store) CreateJointRelease(actorID, org string, input model.JointReleaseInput, source model.ReleaseSource) (model.JointReleaseView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return backend.createJointRelease(ctx, actorID, org, input, source)
	}
	snapshot := releaseSnapshot{data: s.data, plans: s.data.JointReleases}
	plan, old, err := snapshot.newPlan(actorID, org, input, source)
	if err != nil {
		return model.JointReleaseView{}, err
	}
	before := append([]model.JointRelease(nil), s.data.JointReleases...)
	if old != nil {
		s.replaceReleaseLocked(*old)
	}
	s.data.JointReleases = append(s.data.JointReleases, plan)
	if err := s.saveLocked(); err != nil {
		s.data.JointReleases = before
		return model.JointReleaseView{}, err
	}
	return snapshot.view(cloneRelease(plan)), nil
}

func (s *Store) JointReleasesFor(actorID, org, id string) ([]model.JointReleaseView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return backend.jointReleasesFor(ctx, actorID, org, id)
	}
	return (releaseSnapshot{data: s.data, plans: s.data.JointReleases}).visible(actorID, org, id)
}

func (s *Store) RecordGitLabDeployment(event model.GitLabDeployment) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if backend, ok := s.backend.(*postgresBackend); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return backend.recordGitLabDeployment(ctx, event)
	}
	for i, binding := range s.data.DeploymentJobs {
		if !binding.matchesEvent(event) || binding.ReleaseID == "" {
			continue
		}
		plan := (releaseSnapshot{plans: s.data.JointReleases}).plan(binding.OrganizationID, binding.ReleaseID)
		updated, changed, err := (releaseSnapshot{data: s.data}).recordDeployment(plan, binding, event)
		if err != nil || !changed {
			return false, err
		}
		for _, other := range s.data.DeploymentJobs {
			if other.Source == event.Source && other.ProjectID == event.ProjectID && other.DeploymentID == event.DeploymentID {
				return false, ErrReleaseEvidence
			}
		}
		s.replaceReleaseLocked(updated)
		s.data.DeploymentJobs[i].DeploymentID = event.DeploymentID
		if err := s.saveLocked(); err != nil {
			s.replaceReleaseLocked(plan)
			s.data.DeploymentJobs[i] = binding
			return false, err
		}
		return true, nil
	}
	return false, ErrReleaseEvidence
}

// GitLab job IDs are instance-wide. The trusted source and job ID together
// identify one execution, even across plans, projects, and organizations.
type deploymentJobBinding struct {
	Source         string `json:"source"`
	JobID          int64  `json:"job_id"`
	ProjectID      int64  `json:"project_id"`
	PipelineID     int64  `json:"pipeline_id"`
	OrganizationID string `json:"organization_id"`
	PassportID     string `json:"passport_id"`
	ReleaseID      string `json:"release_id,omitempty"`
	DeploymentID   int64  `json:"deployment_id,omitempty"`
}

func bindJob(job *model.GitLabJob, passport model.StoredPassport, plan model.JointRelease) deploymentJobBinding {
	return deploymentJobBinding{Source: job.Source, JobID: job.JobID, ProjectID: job.ProjectID,
		PipelineID: job.PipelineID, OrganizationID: job.OrganizationID, PassportID: passport.ID, ReleaseID: plan.ID}
}

func (b deploymentJobBinding) matchesEvent(event model.GitLabDeployment) bool {
	return b.Source == event.Source && b.JobID == event.JobID && b.ProjectID == event.ProjectID && b.OrganizationID == event.OrganizationID
}

func (b deploymentJobBinding) matchesJob(job *model.GitLabJob, passport model.StoredPassport, plan model.JointRelease) bool {
	if job == nil {
		return false
	}
	candidate := bindJob(job, passport, plan)
	candidate.DeploymentID = b.DeploymentID
	return candidate == b
}

func (s *Store) replaceReleaseLocked(plan model.JointRelease) {
	for i := range s.data.JointReleases {
		if s.data.JointReleases[i].ID == plan.ID && s.data.JointReleases[i].OrganizationID == plan.OrganizationID {
			s.data.JointReleases[i] = plan
			return
		}
	}
}

type releaseSnapshot struct {
	data  state
	plans []model.JointRelease
}

func (r releaseSnapshot) plan(org, id string) model.JointRelease {
	for _, plan := range r.plans {
		if plan.OrganizationID == org && plan.ID == id {
			return plan
		}
	}
	return model.JointRelease{}
}

func (r releaseSnapshot) change(org, id string) model.ChangeRequest {
	for _, change := range r.data.Changes {
		if change.OrganizationID == org && change.ID == id {
			return change
		}
	}
	return model.ChangeRequest{}
}

func (r releaseSnapshot) user(org, id string) (model.User, error) {
	for _, user := range r.data.Users {
		if user.ID == id && user.OrganizationID == org && user.Active {
			return user, nil
		}
	}
	return model.User{}, ErrReleasePermission
}

func (r releaseSnapshot) app(org, id string) model.Application {
	for _, app := range r.data.Applications {
		if app.ID == id && app.OrganizationID == org {
			return app
		}
	}
	return model.Application{}
}

func (r releaseSnapshot) canApp(user model.User, appID string, review bool) bool {
	if r.app(user.OrganizationID, appID).ID == "" {
		return false
	}
	if user.EnterpriseAdmin || user.Role == model.RoleOwner {
		return true
	}
	for _, org := range r.data.Organizations {
		if org.ID == user.OrganizationID && !org.ApplicationAccessControlled {
			return true
		}
	}
	for _, grant := range r.data.ApplicationGrants {
		if grant.OrganizationID == user.OrganizationID && grant.UserID == user.ID && grant.ApplicationID == appID &&
			(grant.CanReview || !review && grant.CanSubmit) {
			return true
		}
	}
	return false
}

func (r releaseSnapshot) canPlan(user model.User, plan model.JointRelease, review bool) bool {
	for _, step := range plan.Steps {
		if !r.canApp(user, step.ApplicationID, review) {
			return false
		}
		if c := r.change(plan.OrganizationID, step.ChangeID); c.ID != "" && c.ApplicationID != step.ApplicationID && !r.canApp(user, c.ApplicationID, review) {
			return false
		}
	}
	return true
}

func (r releaseSnapshot) visible(actorID, org, id string) ([]model.JointReleaseView, error) {
	actor, err := r.user(org, actorID)
	if err != nil {
		return nil, err
	}
	items := make([]model.JointReleaseView, 0)
	for _, plan := range r.plans {
		if plan.OrganizationID != org || id != "" && id != plan.ID || !r.canPlan(actor, plan, false) {
			continue
		}
		// Copy steps so callers cannot mutate store-owned slices or evidence.
		items = append(items, r.view(cloneRelease(plan)))
	}
	if id != "" && len(items) == 0 {
		return nil, ErrNotFound
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items, nil
}

func cloneRelease(plan model.JointRelease) model.JointRelease {
	plan.Steps = append([]model.JointReleaseStep(nil), plan.Steps...)
	for i := range plan.Steps {
		step := &plan.Steps[i]
		if step.Consumption != nil {
			value := *step.Consumption
			step.Consumption = &value
		}
		if step.Deployment != nil {
			value := *step.Deployment
			step.Deployment = &value
		}
	}
	return plan
}

func (r releaseSnapshot) membership(org, id string) (model.JointRelease, int) {
	// Append order is authoritative for the file backend. Superseded plans
	// remain discoverable when a member was not moved into the replacement.
	for i := len(r.plans) - 1; i >= 0; i-- {
		plan := r.plans[i]
		if plan.OrganizationID != org {
			continue
		}
		for n, step := range plan.Steps {
			if step.ChangeID == id && step.InheritedFrom == "" {
				return plan, n
			}
		}
	}
	return model.JointRelease{}, -1
}

func (r releaseSnapshot) newPlan(actorID, org string, input model.JointReleaseInput, source model.ReleaseSource) (model.JointRelease, *model.JointRelease, error) {
	var plan model.JointRelease
	actor, err := r.user(org, actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner && actor.Role != model.RoleReviewer) {
		return plan, nil, ErrReleasePermission
	}
	name, environment := strings.TrimSpace(input.Name), strings.TrimSpace(input.Environment)
	supersedes := strings.TrimSpace(input.Supersedes)
	if name == "" || len([]rune(name)) > 120 || environment == "" || len([]rune(environment)) > 64 ||
		len(input.Steps) == 0 || len(input.Steps) > 32 || supersedes == "" && len(input.Steps) < 2 {
		return plan, nil, fmt.Errorf("%w：需要名称、环境和有序步骤（新计划至少两个）", ErrReleaseInput)
	}
	if source.OrganizationID != org || source.URL == "" || len(source.Projects) == 0 {
		return plan, nil, fmt.Errorf("%w：本组织未配置受信任的 GitLab 发布源", ErrReleaseInput)
	}
	plan = model.JointRelease{ID: NewID("release_"), OrganizationID: org, Name: name, Environment: environment,
		CreatedBy: actorID, CreatedAt: time.Now().UTC(), Supersedes: supersedes}
	if r.plan(org, plan.ID).ID != "" {
		return model.JointRelease{}, nil, ErrConflict
	}
	seen := make(map[string]bool)
	for n, pin := range input.Steps {
		c := r.change(org, pin.ChangeID)
		if c.ID == "" || !r.canApp(actor, c.ApplicationID, true) {
			return model.JointRelease{}, nil, ErrReleasePermission
		}
		projectURL := source.Projects[pin.ProjectID]
		pin.JobName = strings.TrimSpace(pin.JobName)
		pin.ArtifactSHA256 = strings.ToLower(pin.ArtifactSHA256)
		if seen[c.ID] || c.Environment != environment || c.Status == model.StatusCompleted || c.ArtifactSHA256 != pin.ArtifactSHA256 ||
			!hexLength(pin.ArtifactSHA256, 64) || c.MaterialVersion() != pin.ArtifactVersion || !hexLength(c.CommitSHA, 40) ||
			pin.JobName == "" || len(pin.JobName) > 255 || pin.ProjectID <= 0 || projectURL == "" ||
			!sameRepo(r.app(org, c.ApplicationID).RepositoryURL, projectURL) ||
			(c.RepositoryURL != "" && !sameRepo(c.RepositoryURL, projectURL)) {
			return model.JointRelease{}, nil, fmt.Errorf("%w：步骤 %d 的环境、版本、摘要、提交、项目或作业不匹配", ErrReleaseInput, n+1)
		}
		seen[c.ID] = true
		current, _ := r.membership(org, c.ID)
		if current.ID != "" && current.ID != plan.Supersedes {
			return model.JointRelease{}, nil, fmt.Errorf("%w：步骤 %d 已属于另一发布计划", ErrReleaseBlocked, n+1)
		}
		plan.Steps = append(plan.Steps, model.JointReleaseStep{ReleaseStepPin: pin, Title: c.Title,
			ApplicationID: c.ApplicationID, CommitSHA: strings.ToLower(c.CommitSHA), Source: source.URL, ProjectURL: projectURL})
	}
	if plan.Supersedes == "" {
		for _, step := range plan.Steps {
			if step.ReplacesChangeID != "" {
				return model.JointRelease{}, nil, ErrReleaseInput
			}
		}
		return plan, nil, nil
	}
	for _, old := range r.plans {
		if old.OrganizationID != org || old.ID != plan.Supersedes {
			continue
		}
		if !r.canPlan(actor, old, true) {
			return model.JointRelease{}, nil, ErrReleasePermission
		}
		if old.SupersededBy != "" || old.Environment != environment {
			return model.JointRelease{}, nil, ErrReleaseBlocked
		}
		steps, err := r.replacementSteps(old, plan)
		if err != nil {
			return model.JointRelease{}, nil, err
		}
		plan.Steps = steps
		old.SupersededBy = plan.ID
		return plan, &old, nil
	}
	return model.JointRelease{}, nil, ErrNotFound
}

func (r releaseSnapshot) replacementSteps(old, next model.JointRelease) ([]model.JointReleaseStep, error) {
	recoverable, pos := false, 0
	steps := make([]model.JointReleaseStep, 0, len(old.Steps))
	for _, step := range cloneRelease(old).Steps {
		c := r.change(old.OrganizationID, step.ChangeID)
		if c.ID == "" {
			return nil, ErrReleaseBlocked
		}
		invalid := !pinned(old, step, c) || c.Status == model.StatusRejected
		if step.Consumption != nil && step.Deployment == nil {
			return nil, fmt.Errorf("%w：旧计划已有消费，必须先收到该作业的终态部署证据", ErrReleaseBlocked)
		}
		if step.Deployment != nil && step.Deployment.Status == model.ReleaseDeploymentSuccess {
			if invalid || step.Consumption == nil || c.Status != model.StatusCompleted {
				return nil, fmt.Errorf("%w：不能丢弃已成功步骤的部署依赖", ErrReleaseBlocked)
			}
			if step.InheritedFrom == "" {
				step.InheritedFrom = old.ID
			}
			steps = append(steps, step)
			continue
		}
		failed := step.Deployment != nil
		recoverable = recoverable || failed || invalid
		if pos >= len(next.Steps) {
			return nil, fmt.Errorf("%w：新计划必须保留所有未完成步骤的相对顺序", ErrReleaseBlocked)
		}
		replacement := next.Steps[pos]
		pos++
		same := replacement.ChangeID == step.ChangeID && replacement.ReplacesChangeID == "" && !failed &&
			replacement.ApplicationID == step.ApplicationID && replacement.Source == step.Source &&
			replacement.ProjectID == step.ProjectID && replacement.ProjectURL == step.ProjectURL && replacement.JobName == step.JobName
		newChange := (failed || invalid) && replacement.ReplacesChangeID == step.ChangeID &&
			replacement.ChangeID != step.ChangeID && replacement.ApplicationID == step.ApplicationID
		if !same && !newChange {
			return nil, fmt.Errorf("%w：必须在原顺序中保留或显式替换变更 %s", ErrReleaseBlocked, step.ChangeID)
		}
		steps = append(steps, replacement)
	}
	if !recoverable {
		return nil, fmt.Errorf("%w：只有部署失败、取消或固定变更失效才允许替代计划", ErrReleaseBlocked)
	}
	for _, added := range next.Steps[pos:] {
		if added.ReplacesChangeID != "" {
			return nil, ErrReleaseInput
		}
		steps = append(steps, added)
	}
	if len(steps) > 32 {
		return nil, ErrReleaseInput
	}
	return steps, nil
}

func hexLength(value string, length int) bool {
	_, err := hex.DecodeString(value)
	return len(value) == length && err == nil
}

func sameRepo(a, canonical string) bool {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(a), "/"), ".git") == canonical
}

func pinned(plan model.JointRelease, step model.JointReleaseStep, c model.ChangeRequest) bool {
	return c.ID == step.ChangeID && c.ApplicationID == step.ApplicationID && c.Environment == plan.Environment &&
		c.MaterialVersion() == step.ArtifactVersion && c.ArtifactSHA256 == step.ArtifactSHA256 && strings.EqualFold(c.CommitSHA, step.CommitSHA) &&
		(c.RepositoryURL == "" || sameRepo(c.RepositoryURL, step.ProjectURL))
}

func deploymentState(step model.JointReleaseStep) string {
	if step.Deployment != nil {
		return step.Deployment.Status
	}
	return model.ReleaseDeploymentPending
}

// predecessorCause is shared by both the authoritative Gate and the console.
func (r releaseSnapshot) predecessorCause(plan model.JointRelease, index int) string {
	if plan.SupersededBy != "" {
		return "计划已被新计划替代"
	}
	for i, step := range plan.Steps[:index+1] {
		c := r.change(plan.OrganizationID, step.ChangeID)
		switch {
		case !pinned(plan, step, c):
			return fmt.Sprintf("步骤 %d：所固定的变更版本、制品、应用或提交已失效", i+1)
		case i == index:
			return ""
		case step.Consumption == nil:
			return fmt.Sprintf("前置步骤 %d 尚未消费 Gate 通行证", i+1)
		case step.Deployment == nil:
			return fmt.Sprintf("前置步骤 %d 尚无匹配的 GitLab 部署成功证据", i+1)
		case step.Deployment.Status != model.ReleaseDeploymentSuccess:
			return fmt.Sprintf("前置步骤 %d 的 GitLab 部署结果为 %s", i+1, step.Deployment.Status)
		case c.Status != model.StatusCompleted:
			return fmt.Sprintf("前置步骤 %d 的已消费状态已失效", i+1)
		}
	}
	return ""
}

func (r releaseSnapshot) rules(org string) string {
	var policies []model.RiskPolicy
	for _, policy := range r.data.Policies {
		if policy.OrganizationID == org {
			policies = append(policies, policy)
		}
	}
	return changegate.RuleSetVersion(policies)
}

func (r releaseSnapshot) view(plan model.JointRelease) model.JointReleaseView {
	result := model.JointReleaseView{JointRelease: plan, Status: "COMPLETED", Timeline: make([]model.ReleaseStepStatus, 0, len(plan.Steps))}
	rules := r.rules(plan.OrganizationID)
	for i, step := range plan.Steps {
		c := r.change(plan.OrganizationID, step.ChangeID)
		entry := model.ReleaseStepStatus{ChangeID: step.ChangeID, ApprovalStatus: c.Status, ReviewerName: c.ReviewerName,
			GateStatus: "NOT_CONSUMED", DeploymentStatus: deploymentState(step), BlockingCause: r.predecessorCause(plan, i)}
		if step.Consumption != nil {
			entry.GateStatus = "CONSUMED"
		}
		if entry.BlockingCause == "" {
			switch {
			case step.Deployment != nil && entry.DeploymentStatus != model.ReleaseDeploymentSuccess:
				entry.BlockingCause = "GitLab 部署未成功；需要替代计划"
			case step.Consumption != nil && step.Deployment == nil:
				entry.BlockingCause = "Gate 已消费，正在等待匹配的 GitLab 部署证据"
			case step.Consumption == nil && c.Status != model.StatusApproved:
				entry.BlockingCause = "尚未通过独立审批"
			case step.Consumption == nil && c.RuleSetVersion != rules:
				entry.BlockingCause = "审批所用规则已变化"
			case step.Consumption == nil:
				entry.BlockingCause = "已批准，等待部署作业消费 Gate 通行证"
			}
		}
		if entry.BlockingCause != "" || entry.DeploymentStatus != model.ReleaseDeploymentSuccess {
			result.Status = "IN_PROGRESS"
		}
		result.Timeline = append(result.Timeline, entry)
	}
	if plan.SupersededBy != "" {
		result.Status = "SUPERSEDED"
	}
	return result
}

func (r releaseSnapshot) gate(plan model.JointRelease, index int, pass model.StoredPassport, job *model.GitLabJob, at time.Time, consume bool) (model.JointRelease, error) {
	if cause := r.predecessorCause(plan, index); cause != "" {
		return plan, fmt.Errorf("%w：%s", ErrReleaseBlocked, cause)
	}
	step := plan.Steps[index]
	c := r.change(plan.OrganizationID, step.ChangeID)
	if pass.ArtifactVersion != step.ArtifactVersion || pass.ApproverID == "" || pass.ApproverID == c.SubmitterID ||
		pass.ApproverID != c.ReviewerID || c.CheckRun == nil || c.CheckRun.Status != "PASSED" || c.CheckRun.Blocking != 0 ||
		c.CheckRun.ArtifactSHA256 != step.ArtifactSHA256 || c.CheckRun.RuleSetVersion != pass.RuleSetVersion {
		return plan, fmt.Errorf("%w：需要当前制品版本的独立审批和通行证", ErrReleaseBlocked)
	}
	if !consume && job == nil {
		return plan, nil
	}
	if !validJob(job, plan.OrganizationID) || job.Source != step.Source || job.ProjectID != step.ProjectID ||
		job.Name != step.JobName || !strings.EqualFold(job.CommitSHA, step.CommitSHA) ||
		job.Environment != "" && job.Environment != plan.Environment || job.StartedAt.IsZero() ||
		externalTimestampBefore(job.StartedAt, plan.CreatedAt) || job.StartedAt.After(at) {
		return plan, ErrReleaseJob
	}
	if !consume {
		return plan, nil
	}
	if step.Consumption != nil {
		if step.Consumption.PassportID != pass.ID || step.Consumption.JobID != job.JobID || step.Consumption.PipelineID != job.PipelineID ||
			(step.Deployment != nil && step.Deployment.Status != model.ReleaseDeploymentSuccess) {
			return plan, ErrPassportReplay
		}
		return plan, nil
	}
	if pass.Status != model.PassportActive {
		return plan, ErrPassportReplay
	}
	plan = cloneRelease(plan)
	plan.Steps[index].Consumption = &model.ReleaseConsumption{PassportID: pass.ID, At: at, JobID: job.JobID, PipelineID: job.PipelineID}
	return plan, nil
}

func validJob(job *model.GitLabJob, org string) bool {
	return job != nil && job.Source != "" && job.OrganizationID == org && job.ProjectID > 0 && job.JobID > 0 && job.PipelineID > 0
}

// GitLab can report whole-second webhook timestamps or fractional API times.
// Compare at observable precision; a whole-second report must not lose to a
// sub-second local time in the same second, but preserve finer evidence.
func externalTimestampBefore(reported, local time.Time) bool {
	nanos := time.Duration(reported.Nanosecond())
	precision := time.Nanosecond
	switch {
	case nanos == 0:
		precision = time.Second
	case nanos%time.Millisecond == 0:
		precision = time.Millisecond
	case nanos%time.Microsecond == 0:
		precision = time.Microsecond
	}
	return reported.Before(local.Truncate(precision))
}

func (r releaseSnapshot) recordDeployment(plan model.JointRelease, binding deploymentJobBinding, event model.GitLabDeployment) (model.JointRelease, bool, error) {
	for i, step := range plan.Steps {
		if step.InheritedFrom == "" && step.Consumption != nil && step.Consumption.PassportID == binding.PassportID {
			return r.recordStepDeployment(plan, i, binding, event)
		}
	}
	return plan, false, ErrReleaseEvidence
}

func (r releaseSnapshot) recordStepDeployment(plan model.JointRelease, i int, binding deploymentJobBinding, event model.GitLabDeployment) (model.JointRelease, bool, error) {
	step := plan.Steps[i]
	consumption := step.Consumption
	if event.OrganizationID != plan.OrganizationID || event.ProjectID != step.ProjectID || event.ProjectURL != step.ProjectURL || event.Source != step.Source ||
		event.JobID != consumption.JobID || consumption.PipelineID != binding.PipelineID || !strings.EqualFold(event.CommitSHA, step.CommitSHA) ||
		event.Environment != plan.Environment || event.DeploymentID <= 0 || event.OccurredAt.IsZero() || event.ReceivedAt.Before(consumption.At) ||
		externalTimestampBefore(event.OccurredAt, consumption.At) || event.OccurredAt.After(event.ReceivedAt.Add(5*time.Second)) ||
		(event.Status != model.ReleaseDeploymentSuccess && event.Status != model.ReleaseDeploymentFailed && event.Status != model.ReleaseDeploymentCanceled) {
		return plan, false, ErrReleaseEvidence
	}
	if old := step.Deployment; old != nil {
		if binding.DeploymentID == event.DeploymentID && old.DeploymentID == event.DeploymentID &&
			old.Status == event.Status && old.OccurredAt.Equal(event.OccurredAt) {
			return plan, false, nil
		}
		return plan, false, ErrReleaseEvidence // the first authenticated terminal result is immutable
	}
	c := r.change(plan.OrganizationID, step.ChangeID)
	if binding.DeploymentID != 0 || plan.SupersededBy != "" || c.Status != model.StatusCompleted || !pinned(plan, step, c) {
		return plan, false, ErrReleaseEvidence
	}
	plan = cloneRelease(plan)
	plan.Steps[i].Deployment = &event
	return plan, true, nil
}
