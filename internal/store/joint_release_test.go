package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

const releaseTestOrg = "org_demo"
const releaseTestOwner = "usr_owner"

type jointReleaseFixture struct {
	input  model.JointReleaseInput
	source model.ReleaseSource
	passes []model.Passport
}

func seedJointRelease(t *testing.T, data *Store, label string, count int) jointReleaseFixture {
	t.Helper()
	app, err := data.Application("app_order")
	if err != nil {
		t.Fatal(err)
	}
	f := jointReleaseFixture{
		input:  model.JointReleaseInput{Name: label, Environment: "production"},
		source: model.ReleaseSource{OrganizationID: releaseTestOrg, URL: "https://git.example.com", Projects: map[int64]string{12: app.RepositoryURL}},
	}
	rules := changegate.RuleSetVersion(data.PoliciesByOrganization(releaseTestOrg))
	now := time.Now().UTC()
	for range count {
		id := NewID("chg_")
		c := model.ChangeRequest{ID: id, OrganizationID: releaseTestOrg, ApplicationID: app.ID, RepositoryURL: app.RepositoryURL,
			Title: label, CommitSHA: strings.Repeat("a", 40), Environment: f.input.Environment,
			ChangeType: "配置变更", RollbackPlan: "restore " + id, SQLSHA256: changegate.SHA256(""), RollbackSHA256: changegate.SHA256(""),
			ArtifactVersion: 1, RuleSetVersion: rules, Status: model.StatusApproved,
			SubmitterID: "usr_developer", ReviewerID: releaseTestOwner, Version: 1, CreatedAt: now, UpdatedAt: now}
		c.ArtifactSHA256 = changegate.ChangeDigest(c.Environment, c.ChangeType, c.Artifacts, c.SQLSHA256, c.RollbackSHA256, c.RollbackPlan)
		c.CheckRun = &model.CheckRun{Status: "PASSED", ArtifactSHA256: c.ArtifactSHA256, RuleSetVersion: rules}
		p := model.Passport{ID: NewID("pass_"), OrganizationID: c.OrganizationID, ChangeID: id, Environment: c.Environment,
			ArtifactSHA256: c.ArtifactSHA256, ArtifactVersion: 1, RuleSetVersion: rules, ApproverID: c.ReviewerID,
			Status: model.PassportActive, TokenSHA256: changegate.SHA256(id + "token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute * 10)}
		if err := data.CreateChange(c, releaseTestAudit(p)); err != nil {
			t.Fatal(err)
		}
		if err := data.CreatePassport(p, releaseTestAudit(p)); err != nil {
			t.Fatal(err)
		}
		f.input.Steps = append(f.input.Steps, model.ReleaseStepPin{ChangeID: id, ArtifactVersion: 1,
			ArtifactSHA256: c.ArtifactSHA256, ProjectID: 12, JobName: "deploy"})
		f.passes = append(f.passes, p)
	}
	return f
}

func releaseTestAudit(p model.Passport) model.AuditEvent {
	return model.AuditEvent{OrganizationID: p.OrganizationID, ChangeID: p.ChangeID, PassportID: p.ID, CreatedAt: time.Now().UTC()}
}

func releaseUse(s *Store, p model.Passport, job *model.GitLabJob, consume bool) (model.Passport, bool, error) {
	return s.UsePassportForJob(p.ID, p.TokenSHA256, "ci", time.Time{}, consume, releaseTestAudit(p), nil, job)
}

func releaseJob(plan model.JointReleaseView, i int, jobID int64) model.GitLabJob {
	step := plan.Steps[i]
	return model.GitLabJob{Source: step.Source, ProjectID: step.ProjectID, Name: step.JobName, OrganizationID: plan.OrganizationID,
		CommitSHA: step.CommitSHA, Environment: plan.Environment, JobID: jobID, PipelineID: 44, StartedAt: plan.CreatedAt.Truncate(time.Second)}
}

func releaseEvent(plan model.JointReleaseView, step int, p model.Passport, job model.GitLabJob, status string) model.GitLabDeployment {
	return model.GitLabDeployment{Source: job.Source, OrganizationID: job.OrganizationID, JobID: job.JobID, DeploymentID: job.JobID + 100,
		ProjectID: job.ProjectID, ProjectURL: plan.Steps[step].ProjectURL, Environment: plan.Environment, CommitSHA: job.CommitSHA,
		Status: status, OccurredAt: p.ConsumedAt.Truncate(time.Second), ReceivedAt: time.Now().UTC()}
}

func releaseMustCreate(t *testing.T, s *Store, f jointReleaseFixture) model.JointReleaseView {
	t.Helper()
	plan, err := s.CreateJointRelease(releaseTestOwner, releaseTestOrg, f.input, f.source)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func releaseMustView(t *testing.T, s *Store, id string) model.JointReleaseView {
	t.Helper()
	views, err := s.JointReleasesFor(releaseTestOwner, releaseTestOrg, id)
	if err != nil || len(views) != 1 {
		t.Fatalf("view: count=%d error=%v", len(views), err)
	}
	return views[0]
}

func releaseMustRecord(t *testing.T, s *Store, event model.GitLabDeployment, want bool, wantErr error) {
	t.Helper()
	if changed, err := s.RecordGitLabDeployment(event); changed != want || !errors.Is(err, wantErr) {
		t.Fatalf("record: changed=%v error=%v; want %v %v", changed, err, want, wantErr)
	}
}

// Exercise both execution and the recovery path with the same three-step
// contract on memory, file restart, and independent PostgreSQL stores.
func exerciseJointRelease(t *testing.T, first, second *Store, restart func() *Store) {
	t.Helper()
	f := seedJointRelease(t, first, "ordered", 3)
	extra := seedJointRelease(t, first, "replacement", 2)
	independent := seedJointRelease(t, first, "independent", 2)
	ordinary := seedJointRelease(t, first, "ordinary", 2)
	if _, err := second.CreateJointRelease(releaseTestOwner, "other_org", f.input, f.source); !errors.Is(err, ErrReleasePermission) {
		t.Fatalf("cross-org create: %v", err)
	}
	if _, err := second.CreateJointRelease("usr_developer", releaseTestOrg, f.input, f.source); !errors.Is(err, ErrReleasePermission) {
		t.Fatalf("developer create: %v", err)
	}
	plan := releaseMustCreate(t, first, f)
	unrelated := releaseMustCreate(t, first, independent)
	if _, err := second.JointReleasesFor(releaseTestOwner, "other_org", plan.ID); !errors.Is(err, ErrReleasePermission) {
		t.Fatalf("foreign read: %v", err)
	}
	jobs := []model.GitLabJob{releaseJob(plan, 0, 30), releaseJob(plan, 1, 31), releaseJob(plan, 2, 32)}
	if _, _, err := releaseUse(second, f.passes[0], nil, false); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, _, err := releaseUse(second, f.passes[1], nil, false); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("out-of-order verify: %v", err)
	}
	if _, _, err := releaseUse(second, f.passes[0], nil, true); !errors.Is(err, ErrReleaseJob) {
		t.Fatalf("missing trusted job: %v", err)
	}
	wrong := jobs[0]
	wrong.Environment = "staging"
	if _, _, err := releaseUse(second, f.passes[0], &wrong, true); !errors.Is(err, ErrReleaseJob) {
		t.Fatalf("mismatched trusted job: %v", err)
	}

	type outcome struct {
		pass   model.Passport
		replay bool
		err    error
	}
	results, start := make(chan outcome, 2), make(chan struct{})
	for _, s := range []*Store{first, second} {
		go func(s *Store) {
			<-start
			p, replay, err := releaseUse(s, f.passes[0], &jobs[0], true)
			results <- outcome{p, replay, err}
		}(s)
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.replay == b.replay || a.pass.ConsumedAt == nil ||
		b.pass.ConsumedAt == nil || !a.pass.ConsumedAt.Equal(*b.pass.ConsumedAt) {
		t.Fatalf("concurrent same-job consumes: %+v %+v", a, b)
	}
	if _, _, err := releaseUse(second, f.passes[0], nil, true); !errors.Is(err, ErrReleaseJob) {
		t.Fatalf("legacy replay bypass: %v", err)
	}
	wrong = jobs[0]
	wrong.JobID = 67
	if _, _, err := releaseUse(second, f.passes[0], &wrong, true); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("different-job replay: %v", err)
	}
	otherJob := releaseJob(unrelated, 0, jobs[0].JobID)
	if _, _, err := releaseUse(second, independent.passes[0], &otherJob, true); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("same job reused in independent plan: %v", err)
	}
	if _, _, err := releaseUse(second, independent.passes[0], &otherJob, false); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("bound job verified in independent plan: %v", err)
	}
	if _, _, err := releaseUse(second, ordinary.passes[0], &jobs[0], true); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("same job reused without a plan: %v", err)
	}
	if _, _, err := releaseUse(second, ordinary.passes[0], nil, true); err != nil {
		t.Fatalf("ordinary change without trusted job: %v", err)
	}
	if _, _, err := releaseUse(second, f.passes[1], &jobs[1], true); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("consumption is not deployment: %v", err)
	}
	firstEvent := releaseEvent(plan, 0, a.pass, jobs[0], model.ReleaseDeploymentSuccess)
	bad := firstEvent
	bad.OrganizationID = "other_org"
	releaseMustRecord(t, second, bad, false, ErrReleaseEvidence)
	bad = firstEvent
	bad.OccurredAt = bad.OccurredAt.Add(-time.Second)
	releaseMustRecord(t, second, bad, false, ErrReleaseEvidence)
	releaseMustRecord(t, second, firstEvent, true, nil)
	bad = firstEvent
	bad.Status = model.ReleaseDeploymentFailed
	releaseMustRecord(t, first, bad, false, ErrReleaseEvidence)
	releaseMustRecord(t, first, firstEvent, false, nil)
	if _, _, err := releaseUse(second, f.passes[2], &jobs[2], true); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("third step skipped second: %v", err)
	}
	p, replay, err := releaseUse(second, f.passes[1], &jobs[1], true)
	if err != nil || replay {
		t.Fatalf("second step: replay=%v err=%v", replay, err)
	}
	replacement := extra
	replacement.input.Supersedes = plan.ID
	newStep := extra.input.Steps[0]
	newStep.ReplacesChangeID = f.input.Steps[1].ChangeID
	replacement.input.Steps = []model.ReleaseStepPin{newStep, f.input.Steps[2]}
	if _, err := first.CreateJointRelease(releaseTestOwner, releaseTestOrg, replacement.input, f.source); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("replaced an unsettled deployment: %v", err)
	}
	failure := releaseEvent(plan, 1, p, jobs[1], model.ReleaseDeploymentFailed)
	bad = failure
	bad.DeploymentID = firstEvent.DeploymentID
	releaseMustRecord(t, first, bad, false, ErrReleaseEvidence)
	releaseMustRecord(t, first, failure, true, nil)
	if _, _, err := releaseUse(first, f.passes[2], nil, false); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("failed predecessor unlocked third step: %v", err)
	}
	if _, _, err := releaseUse(first, f.passes[1], &jobs[1], true); !errors.Is(err, ErrPassportReplay) {
		t.Fatalf("failed deployment replay: %v", err)
	}
	dropped := replacement.input
	dropped.Steps = []model.ReleaseStepPin{newStep}
	if _, err := first.CreateJointRelease(releaseTestOwner, releaseTestOrg, dropped, f.source); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("dropped third step: %v", err)
	}
	reordered := replacement.input
	reordered.Steps = []model.ReleaseStepPin{f.input.Steps[2], newStep}
	if _, err := first.CreateJointRelease(releaseTestOwner, releaseTestOrg, reordered, f.source); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("reordered dependencies: %v", err)
	}
	next := releaseMustCreate(t, first, replacement)
	if len(next.Steps) != 3 || next.Steps[0].InheritedFrom != plan.ID ||
		next.Steps[0].Deployment == nil || next.Steps[0].Deployment.DeploymentID != firstEvent.DeploymentID {
		t.Fatalf("lost successful dependency: %+v", next.Steps)
	}
	next.Steps[0].Deployment.Status = "corrupted caller copy"
	if got := releaseMustView(t, second, next.ID).Steps[0].Deployment.Status; got != model.ReleaseDeploymentSuccess {
		t.Fatalf("caller changed stored evidence to %s", got)
	}
	third := releaseJob(next, 2, 43)
	if _, _, err := releaseUse(second, f.passes[2], &third, true); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("replacement dependency bypass: %v", err)
	}
	if _, _, err := releaseUse(second, f.passes[0], &jobs[0], true); !errors.Is(err, ErrReleaseBlocked) {
		t.Fatalf("superseded gate still allowed: %v", err)
	}
	releaseMustRecord(t, second, firstEvent, false, nil)
	bad = failure
	bad.Status = model.ReleaseDeploymentSuccess
	releaseMustRecord(t, second, bad, false, ErrReleaseEvidence)
	after := restart()
	old := releaseMustView(t, after, plan.ID)
	if old.SupersededBy != next.ID || old.Status != "SUPERSEDED" || old.Steps[1].Deployment.Status != model.ReleaseDeploymentFailed {
		t.Fatalf("lost superseded result on restart: %+v", old)
	}
	replaceJob := releaseJob(next, 1, 42)
	p, replay, err = releaseUse(after, extra.passes[0], &replaceJob, true)
	if err != nil || replay {
		t.Fatalf("replacement gate: replay=%v err=%v", replay, err)
	}
	releaseMustRecord(t, after, releaseEvent(next, 1, p, replaceJob, model.ReleaseDeploymentSuccess), true, nil)
	p, replay, err = releaseUse(after, f.passes[2], &third, true)
	if err != nil || replay {
		t.Fatalf("third step: replay=%v err=%v", replay, err)
	}
	releaseMustRecord(t, after, releaseEvent(next, 2, p, third, model.ReleaseDeploymentSuccess), true, nil)
	if final := releaseMustView(t, after, next.ID); final.Status != "COMPLETED" {
		t.Fatalf("final plan: %+v", final.Timeline)
	}
	consumes := 0
	for _, audit := range after.AuditsByChange(releaseTestOrg, f.passes[0].ChangeID) {
		if audit.Action == "PASSPORT_CONSUMED_AND_CHANGE_COMPLETED" {
			consumes++
		}
	}
	if consumes != 1 {
		t.Fatalf("expected one durable consume audit, got %d", consumes)
	}
}

func TestJointReleaseMemoryAndFile(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		s := NewMemory()
		exerciseJointMembershipRace(t, s, s)
		exerciseJointRelease(t, s, s, func() *Store { return s })
	})
	t.Run("file", func(t *testing.T) {
		t.Setenv("CHANGEGUARD_ENABLE_DEMO_DATA", "true")
		path := filepath.Join(t.TempDir(), "state.json")
		s, err := newFileStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		exerciseJointRelease(t, s, s, func() *Store {
			s.Close()
			next, err := newFileStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(next.Close)
			return next
		})
	})
}

func exerciseJointMembershipRace(t *testing.T, first, second *Store) {
	t.Helper()
	f := seedJointRelease(t, first, "membership race", 2)
	start := make(chan struct{})
	created, used := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		_, err := first.CreateJointRelease(releaseTestOwner, releaseTestOrg, f.input, f.source)
		created <- err
	}()
	go func() {
		<-start
		_, _, err := releaseUse(second, f.passes[0], nil, true)
		used <- err
	}()
	close(start)
	createErr, useErr := <-created, <-used
	if !(createErr == nil && errors.Is(useErr, ErrReleaseJob) ||
		useErr == nil && errors.Is(createErr, ErrReleaseInput)) {
		t.Fatalf("membership and legacy consume must serialize: create=%v consume=%v", createErr, useErr)
	}
	_, replayed, err := releaseUse(first, f.passes[0], nil, true)
	if createErr == nil {
		if replayed || !errors.Is(err, ErrReleaseJob) {
			t.Fatalf("created membership did not block retry: replayed=%v err=%v", replayed, err)
		}
	} else if !replayed || err != nil {
		t.Fatalf("failed plan creation affected ordinary replay: replayed=%v err=%v", replayed, err)
	}
}

func exerciseOrdinaryGateWithoutLegacyRowLock(t *testing.T, first, second *Store) {
	t.Helper()
	p := seedJointRelease(t, first, "ordinary gate under legacy write lock", 1).passes[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := first.backend.(*postgresBackend).pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// A separate transaction holds the same row write lock as a snapshot save,
	// without changing any policies or touching normalized rows.
	tag, err := tx.Exec(ctx, `UPDATE dbguard_state SET updated_at=now() WHERE id=1`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold legacy write lock: rows=%d err=%v", tag.RowsAffected(), err)
	}
	audit := releaseTestAudit(p)
	audit.Action = "PASSPORT_VERIFIED"
	verified, replayed, err := second.UsePassportForJob(p.ID, p.TokenSHA256, "ci", time.Time{}, false, audit, &p, nil)
	if err != nil || replayed || verified.Status != model.PassportActive {
		t.Fatalf("ordinary verify under legacy write lock: status=%s replayed=%v err=%v", verified.Status, replayed, err)
	}
	consumed, replayed, err := releaseUse(second, p, nil, true)
	if err != nil || replayed || consumed.ConsumedAt == nil {
		t.Fatalf("ordinary consume under legacy write lock: replayed=%v err=%v", replayed, err)
	}
	retry, replayed, err := releaseUse(second, p, nil, true)
	if err != nil || !replayed || retry.ConsumedAt == nil || !retry.ConsumedAt.Equal(*consumed.ConsumedAt) {
		t.Fatalf("ordinary replay under legacy write lock: replayed=%v err=%v", replayed, err)
	}
}

func exercisePostgresGateRejectsStaleState(t *testing.T, first, second *Store) {
	t.Helper()
	fixture := seedJointRelease(t, first, "stale gate evidence", 3)
	stalePin, expiring, stalePolicy := fixture.passes[0], fixture.passes[1], fixture.passes[2]
	if _, err := first.UpdateChange(stalePin.ChangeID, func(c *model.ChangeRequest) error {
		c.ArtifactVersion++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseUse(second, stalePin, nil, true); !errors.Is(err, ErrPassportChangeInvalid) {
		t.Fatalf("ordinary Gate authorized a stale artifact version: %v", err)
	}
	if _, err := second.UsePassport(expiring.ID, expiring.TokenSHA256, "ci", expiring.ExpiresAt, true,
		releaseTestAudit(expiring)); !errors.Is(err, ErrPassportExpired) {
		t.Fatalf("ordinary Gate authorized an expired passport: %v", err)
	}
	policies := first.PoliciesByOrganization(releaseTestOrg)
	if len(policies) == 0 {
		t.Fatal("expected seeded policies")
	}
	if _, err := first.UpdatePolicy(policies[0].ID, func(policy *model.RiskPolicy) error {
		policy.Enabled = !policy.Enabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, consume := range []bool{false, true} {
		if _, _, err := releaseUse(second, stalePolicy, nil, consume); !errors.Is(err, ErrPassportRuleSet) {
			t.Fatalf("Gate used stale cached rules: consume=%v err=%v", consume, err)
		}
	}
}

func exerciseCrossOrganizationJobRace(t *testing.T, first, second *Store) {
	t.Helper()
	f := seedJointRelease(t, first, "independent ordinary change", 1)
	own := f.passes[0]
	orgID := NewID("org_")
	user := model.User{ID: NewID("user_"), OrganizationID: orgID, Role: model.RoleOwner, Active: true}
	if err := first.CreateEnterprise(model.Organization{ID: orgID, Slug: orgID}, user, model.UserCredential{}, nil,
		model.AuditEvent{OrganizationID: orgID}); err != nil {
		t.Fatal(err)
	}
	c, err := first.Change(own.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	c.ID, c.OrganizationID, c.RuleSetVersion = NewID("chg_"), orgID, changegate.RuleSetVersion(nil)
	foreign := own
	foreign.ID, foreign.ChangeID, foreign.OrganizationID, foreign.RuleSetVersion = NewID("pass_"), c.ID, orgID, c.RuleSetVersion
	if err := first.CreateChange(c, releaseTestAudit(foreign)); err != nil {
		t.Fatal(err)
	}
	if err := first.CreatePassport(foreign, releaseTestAudit(foreign)); err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for i, p := range []model.Passport{own, foreign} {
		s := []*Store{first, second}[i]
		go func() {
			// No release or org lock common to these two authorized operations.
			job := model.GitLabJob{Source: f.source.URL, JobID: 60, PipelineID: 7, ProjectID: 12, OrganizationID: p.OrganizationID}
			<-start
			_, _, err := releaseUse(s, p, &job, true)
			results <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	if !(a == nil && errors.Is(b, ErrPassportReplay) || b == nil && errors.Is(a, ErrPassportReplay)) {
		t.Fatalf("one job bound to two independent changes: %v / %v", a, b)
	}
}

func TestJointReleaseRequiresGrantsForEveryApplication(t *testing.T) {
	s := NewMemory()
	f := seedJointRelease(t, s, "two application grants", 2)
	app, err := s.Application("app_inventory")
	if err != nil {
		t.Fatal(err)
	}
	f.source.Projects[13] = app.RepositoryURL
	f.input.Steps[1].ProjectID = 13
	if _, err := s.UpdateChange(f.input.Steps[1].ChangeID, func(c *model.ChangeRequest) error {
		c.ApplicationID = app.ID
		c.RepositoryURL = app.RepositoryURL
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reviewer := "usr_reviewer"
	restrict := func(grants ...model.ApplicationGrantInput) {
		t.Helper()
		if _, err := s.UpdateMember(releaseTestOrg, reviewer, func(*model.User) error { return nil }, grants, model.AuditEvent{OrganizationID: releaseTestOrg}); err != nil {
			t.Fatal(err)
		}
	}
	order := model.ApplicationGrantInput{ApplicationID: "app_order", CanReview: true}
	restrict(order)
	if _, err := s.CreateJointRelease(reviewer, releaseTestOrg, f.input, f.source); !errors.Is(err, ErrReleasePermission) {
		t.Fatalf("create without every review grant: %v", err)
	}
	plan := releaseMustCreate(t, s, f)
	if _, err := s.JointReleasesFor(reviewer, releaseTestOrg, plan.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial read leaked plan: %v", err)
	}
	restrict(order, model.ApplicationGrantInput{ApplicationID: app.ID, CanReview: true})
	if _, err := s.JointReleasesFor(reviewer, releaseTestOrg, plan.ID); err != nil {
		t.Fatalf("fully authorized review: %v", err)
	}
	s.data.ApplicationGrants = nil // enabled access control with no grants is deny-by-default
	if _, err := s.JointReleasesFor(reviewer, releaseTestOrg, plan.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no grants leaked plan in access-controlled org: %v", err)
	}
}

func TestJointReleaseConsumeRollback(t *testing.T) {
	s := NewMemory()
	f := seedJointRelease(t, s, "rollback", 2)
	plan := releaseMustCreate(t, s, f)
	payload, err := json.Marshal(s.data)
	if err != nil {
		t.Fatal(err)
	}
	persistence := errors.New("injected persistence failure")
	s.persisted = payload
	s.backend = &failingSaveBackend{payload: payload, version: 1, err: persistence}
	job := releaseJob(plan, 0, 10)
	p := f.passes[0]
	if _, _, err := s.UsePassportForJob(p.ID, p.TokenSHA256, "ci", time.Time{}, false, releaseTestAudit(p), &p, nil); !errors.Is(err, persistence) {
		t.Fatalf("verify allowed despite audit persistence failure: %v", err)
	}
	if _, _, err := releaseUse(s, f.passes[0], &job, true); !errors.Is(err, persistence) {
		t.Fatalf("consume did not fail closed: %v", err)
	}
	gotPass, _ := s.Passport(f.passes[0].ID)
	gotChange, _ := s.Change(f.passes[0].ChangeID)
	if gotPass.Status != model.PassportActive || gotChange.Status != model.StatusApproved || len(s.data.DeploymentJobs) != 0 ||
		releaseMustView(t, s, plan.ID).Steps[0].Consumption != nil {
		t.Fatal("failed save leaked a state transition")
	}
	s.backend = nil
	if _, replay, err := releaseUse(s, f.passes[0], &job, true); err != nil || replay {
		t.Fatalf("retry after persistence failure: replay=%v err=%v", replay, err)
	}
}

func TestJointReleaseExternalTimestampPrecision(t *testing.T) {
	base := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	local := base.Add(541*time.Millisecond + 92*time.Microsecond)
	for _, tc := range []struct {
		reported time.Time
		isOlder  bool
	}{
		{base.Add(-time.Second), true},
		{base, false},
		{base.Add(539 * time.Millisecond), true},
		{base.Add(541 * time.Millisecond), false},
		{base.Add(541*time.Millisecond + 91*time.Microsecond), true},
	} {
		if got := externalTimestampBefore(tc.reported, local); got != tc.isOlder {
			t.Errorf("report=%v local=%v older=%v; want %v", tc.reported, local, got, tc.isOlder)
		}
	}
}

func TestPostgresJointReleaseMultiInstance(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CHANGEGUARD_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CHANGEGUARD_TEST_POSTGRES_DSN is not set; PostgreSQL joint release integration skipped")
	}
	t.Setenv("CHANGEGUARD_ENABLE_DEMO_DATA", "true")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := NewID("joint_test_")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(dropCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
	}()
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	open := func() *Store {
		s, err := NewPostgres(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := open(), open()
	defer first.Close()
	defer second.Close()
	t.Run("ordinary_gate_during_legacy_snapshot_write", func(t *testing.T) {
		exerciseOrdinaryGateWithoutLegacyRowLock(t, first, second)
	})
	exerciseJointMembershipRace(t, first, second)
	exerciseCrossOrganizationJobRace(t, first, second)
	var restarted *Store
	defer func() {
		if restarted != nil {
			restarted.Close()
		}
	}()
	exerciseJointRelease(t, first, second, func() *Store {
		first.Close()
		second.Close()
		restarted = open()
		return restarted
	})
	t.Run("stale_pin_expiry_and_committed_policy", func(t *testing.T) {
		other := open()
		defer other.Close()
		exercisePostgresGateRejectsStaleState(t, restarted, other)
	})
}
