package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func TestJointReleaseServiceRecoveryAndArtifactPin(t *testing.T) {
	t.Setenv("CHANGEGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("x", 32))
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	app, err := data.Application("app_order")
	if err != nil {
		t.Fatal(err)
	}
	source := model.ReleaseSource{OrganizationID: app.OrganizationID, URL: "https://git.example.com",
		Projects: map[int64]string{51: app.RepositoryURL}}
	type fixture struct {
		input model.CreateChangeInput
		pin   model.ReleaseStepPin
		gate  model.GateRequest
	}
	approve := func(c model.ChangeRequest, input model.CreateChangeInput) fixture {
		t.Helper()
		c, err := svc.Submit(c.ID, "usr_developer")
		if err != nil || c.Status != model.StatusWaitingApproval {
			t.Fatalf("submit: status=%s err=%v findings=%+v", c.Status, err, c.Findings)
		}
		c, err = svc.Approve(c.ID, "usr_owner", "reviewed release pin")
		if err != nil {
			t.Fatal(err)
		}
		p, err := svc.IssuePassport(c.ID, "usr_owner", 0)
		if err != nil || p.Passport.ArtifactVersion != c.MaterialVersion() {
			t.Fatalf("issue: version=%d err=%v", p.Passport.ArtifactVersion, err)
		}
		return fixture{input: input, pin: model.ReleaseStepPin{ChangeID: c.ID, ArtifactVersion: c.MaterialVersion(),
			ArtifactSHA256: c.ArtifactSHA256, ProjectID: 51, JobName: "deploy"},
			gate: model.GateRequest{Token: p.Token, ArtifactSHA256: c.ArtifactSHA256, Environment: c.Environment, Consumer: "release-ci"}}
	}
	created := 0
	newFixture := func(title string) fixture {
		t.Helper()
		created++
		input := model.CreateChangeInput{Title: title, ApplicationID: app.ID, Environment: "production", ChangeType: "配置变更",
			CommitSHA: strings.Repeat("a", 40), RepositoryURL: app.RepositoryURL,
			Artifacts:    []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "application.yaml", Content: "feature_enabled: false"}},
			RollbackPlan: "restore prior configuration", PlannedAt: time.Now().Add(time.Duration(48+created*3) * time.Hour),
			ReleasePlan: model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx", "P99 延迟"}}}
		c, err := svc.Create(input, "usr_developer")
		if err != nil {
			t.Fatal(err)
		}
		return approve(c, input)
	}
	first, second, third := newFixture("first"), newFixture("second"), newFixture("third")
	create := func(supersedes string, pins ...model.ReleaseStepPin) model.JointReleaseView {
		t.Helper()
		view, err := svc.CreateJointRelease("usr_owner", model.JointReleaseInput{Name: "ordered", Environment: "production",
			Supersedes: supersedes, Steps: pins}, source)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	original := create("", first.pin, second.pin, third.pin)
	if _, err := svc.VerifyGate(first.gate, true); !errors.Is(err, store.ErrReleaseJob) {
		t.Fatalf("unbound legacy consume: %v", err)
	}
	if got, err := svc.VerifyGate(first.gate, false); err != nil || !got.Allowed {
		t.Fatalf("first verify: allowed=%v err=%v", got.Allowed, err)
	}
	if _, err := svc.VerifyGate(second.gate, false); !errors.Is(err, store.ErrReleaseBlocked) {
		t.Fatalf("second verified prematurely: %v", err)
	}
	run := func(plan model.JointReleaseView, i int, f fixture, id int64, status string) {
		t.Helper()
		step := plan.Steps[i]
		job := model.GitLabJob{Source: step.Source, OrganizationID: plan.OrganizationID, Name: step.JobName,
			CommitSHA: step.CommitSHA, ProjectID: step.ProjectID, JobID: id, PipelineID: 9, StartedAt: plan.CreatedAt.Truncate(time.Second)}
		res, err := svc.VerifyGateForJob(f.gate, true, &job)
		if err != nil || !res.Allowed || res.Replayed || res.Passport.ConsumedAt == nil {
			t.Fatalf("consume: allowed=%v replay=%v err=%v", res.Allowed, res.Replayed, err)
		}
		if i == 0 {
			replay, err := svc.VerifyGateForJob(f.gate, true, &job)
			if err != nil || !replay.Replayed || !replay.Passport.ConsumedAt.Equal(*res.Passport.ConsumedAt) {
				t.Fatalf("service replay: replay=%v err=%v", replay.Replayed, err)
			}
		}
		event := model.GitLabDeployment{Source: step.Source, OrganizationID: plan.OrganizationID, ProjectID: step.ProjectID,
			ProjectURL: step.ProjectURL, JobID: job.JobID, DeploymentID: id + 50, CommitSHA: step.CommitSHA,
			Environment: plan.Environment, Status: status, OccurredAt: res.Passport.ConsumedAt.Truncate(time.Second)}
		if changed, err := svc.RecordGitLabDeployment(event); err != nil || !changed {
			t.Fatalf("record: changed=%v err=%v", changed, err)
		}
	}
	run(original, 0, first, 1, model.ReleaseDeploymentSuccess)
	run(original, 1, second, 2, model.ReleaseDeploymentFailed)
	if _, err := svc.VerifyGate(third.gate, false); !errors.Is(err, store.ErrReleaseBlocked) {
		t.Fatalf("third verified despite failed predecessor: %v", err)
	}
	replacement := newFixture("second retry")
	replacement.pin.ReplacesChangeID = second.pin.ChangeID
	recovered := create(original.ID, replacement.pin, third.pin)
	run(recovered, 1, replacement, 3, model.ReleaseDeploymentSuccess)

	// A same-digest update still increments the material revision. Neither the
	// old token nor a newly approved token can authorize the stale plan pin.
	third.input.Title = "third revision"
	edited, err := svc.Update(third.pin.ChangeID, third.input, "usr_developer")
	if err != nil || edited.MaterialVersion() != third.pin.ArtifactVersion+1 || edited.ArtifactSHA256 != third.pin.ArtifactSHA256 {
		t.Fatalf("same-digest version change: version=%d err=%v", edited.MaterialVersion(), err)
	}
	if _, err := svc.VerifyGate(third.gate, false); !errors.Is(err, ErrPassportRevoked) {
		t.Fatalf("old token after update: %v", err)
	}
	refreshed := approve(edited, third.input)
	if _, err := svc.VerifyGate(refreshed.gate, false); !errors.Is(err, store.ErrReleaseBlocked) {
		t.Fatalf("new passport with stale pin: %v", err)
	}
	final := create(recovered.ID, refreshed.pin)
	if len(final.Steps) != 3 || final.Steps[0].InheritedFrom != original.ID || final.Steps[1].InheritedFrom != recovered.ID {
		t.Fatalf("lost inherited history: %+v", final.Steps)
	}
	run(final, 2, refreshed, 4, model.ReleaseDeploymentSuccess)
	items, err := svc.JointReleasesFor("usr_owner", final.ID)
	if err != nil || len(items) != 1 || items[0].Status != "COMPLETED" {
		t.Fatalf("final view: count=%d err=%v", len(items), err)
	}
}
