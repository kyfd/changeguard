package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

func TestHTTPJointReleaseGitLabEndToEnd(t *testing.T) {
	const env, hookToken, readToken = "production", "test-webhook-secret", "test-read-api-secret"
	t.Setenv("CHANGEGUARD_PASSPORT_HMAC_SECRET", strings.Repeat("j", 32))
	t.Setenv("CHANGEGUARD_GITLAB_SIGNING_TOKEN", "")
	t.Setenv("CHANGEGUARD_GITLAB_WEBHOOK_SECRET", hookToken)
	t.Setenv("CHANGEGUARD_GITLAB_READ_API_TOKEN", readToken)
	var mu sync.Mutex
	jobs, deployments := map[string]any{}, map[string]any{}
	gitlab := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var payload any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/job" && r.Header.Get("PRIVATE-TOKEN") == "":
			payload = jobs[r.Header.Get("JOB-TOKEN")]
		case r.Method == http.MethodGet && r.Header.Get("PRIVATE-TOKEN") == readToken && r.Header.Get("JOB-TOKEN") == "":
			payload = deployments[r.URL.Path]
		}
		if payload == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, payload)
	}))
	defer gitlab.Close()
	project := gitlab.URL + "/test/orders"
	// Seed organization/user via the same session and CSRF endpoints as the Web UI.
	data := store.NewMemory()
	svc := service.New(data, idempotencyRunner{}, idempotencyAnalyzer{})
	svc.Start(t.Context(), 1)
	logger := log.New(io.Discard, "", 0)
	manager := auth.New(auth.Config{Mode: "local", SessionTTL: time.Hour}, data, logger)
	loggerServer := New(svc, manager, logger)
	// Configure the fake API after registration; at runtime the equivalent config
	// is read exactly once from the environment when constructing the server.
	httpServer := httptest.NewServer(loggerServer)
	defer httpServer.Close()
	type sessionFixture struct {
		model.AuthSession
		cookie *http.Cookie
	}
	request := func(s *httptest.Server, method, path string, subject *sessionFixture, headers map[string]string, value any, expected int, result any) ([]byte, []*http.Cookie) {
		t.Helper()
		var content io.Reader
		if value != nil {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			content = bytes.NewReader(encoded)
		}
		r, err := http.NewRequestWithContext(t.Context(), method, s.URL+path, content)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		if subject != nil {
			r.AddCookie(subject.cookie)
			r.Header.Set("X-CSRF-Token", subject.CSRFToken)
		}
		for key, val := range headers {
			r.Header.Set(key, val)
		}
		response, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != expected {
			t.Fatalf("%s %s: HTTP %d, want %d; body %.450s; err %v", method, path, response.StatusCode, expected, body, err)
		}
		if result != nil {
			if err := json.Unmarshal(body, result); err != nil {
				t.Fatalf("%s %s: invalid response: %v", method, path, err)
			}
		}
		return body, response.Cookies()
	}
	register := func(name, slug string) sessionFixture {
		var s sessionFixture
		_, cookies := request(httpServer, http.MethodPost, "/api/auth/register", nil, nil, model.RegisterEnterpriseInput{
			OrganizationName: name, OrganizationSlug: slug, Name: "owner", Email: "owner@" + slug + ".example", Password: "fixture-password-strong1",
		}, http.StatusCreated, &s.AuthSession)
		if len(cookies) == 0 || s.CSRFToken == "" {
			t.Fatal("registration did not open a session")
		}
		s.cookie = cookies[0]
		return s
	}
	owner := register("Joint Release Test", "joint-release-test")
	outsider := register("Other Organization", "joint-release-outsider")
	t.Setenv("CHANGEGUARD_GITLAB_ORGANIZATION_ID", owner.Organization.ID)
	t.Setenv("CHANGEGUARD_GITLAB_RELEASE_ORIGIN", gitlab.URL)
	t.Setenv("CHANGEGUARD_GITLAB_RELEASE_PROJECTS", fmt.Sprintf(`{"42":%q}`, project))
	server := New(svc, manager, logger)
	var err error
	server.gitLabRelease, err = integration.NewGitLabReleaseClient(server.integrations, gitlab.Client().Transport)
	if err != nil || server.gitLabRelease == nil {
		t.Fatalf("GitLab config: %v", err)
	}
	joint := httptest.NewServer(server)
	defer joint.Close()
	call := func(method, path string, subject *sessionFixture, headers map[string]string, value any, expected int, result any) []byte {
		t.Helper()
		body, _ := request(joint, method, path, subject, headers, value, expected, result)
		return body
	}
	var invite model.InviteCreated
	const developerEmail = "developer@joint-release-test.example"
	call(http.MethodPost, "/api/enterprise/invites", &owner, nil, model.CreateInviteInput{Email: developerEmail, Role: model.RoleDeveloper}, http.StatusCreated, &invite)
	var developer sessionFixture
	_, cookies := request(joint, http.MethodPost, "/api/auth/invitations/accept", nil, nil, model.AcceptInviteInput{
		Token: invite.PlainToken, Name: "developer", Email: developerEmail, Password: "fixture-developer-password1",
	}, http.StatusCreated, &developer.AuthSession)
	if len(cookies) == 0 {
		t.Fatal("invite acceptance did not open a session")
	}
	developer.cookie = cookies[0]
	mkApp := func(name string) string {
		t.Helper()
		a, err := svc.CreateApplication(model.SaveApplicationInput{Name: name, Owner: "on-call", RepositoryURL: project, Environment: env}, owner.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	databaseID, appID, flagID := mkApp("Orders database"), mkApp("Orders application"), mkApp("Orders feature flags")
	call(http.MethodPut, "/api/enterprise/members/"+developer.User.ID, &owner, nil, model.UpdateMemberInput{
		Role: model.RoleDeveloper, Active: true, ApplicationGrants: []model.ApplicationGrantInput{
			{ApplicationID: databaseID, CanSubmit: true}, {ApplicationID: appID, CanSubmit: true}, {ApplicationID: flagID, CanSubmit: true},
		},
	}, http.StatusOK, nil)
	type changeFixture struct {
		pin        model.ReleaseStepPin
		sha        string
		credential model.PassportCredential
	}
	mkChange := func(title, app, job, sha string, migration bool) changeFixture {
		t.Helper()
		input := model.CreateChangeInput{Title: title, ApplicationID: app, Environment: env, RepositoryURL: project, CommitSHA: sha,
			RollbackPlan: "Restore the previous approved version", ReleasePlan: model.ReleasePlan{
				Strategy: "金丝雀发布", ObservationMinutes: 15, SuccessMetrics: []string{"HTTP 5xx"},
			}, Artifacts: []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}}}
		if migration {
			input.Artifacts = nil
			input.ChangeType = "DDL"
			input.SQL = "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_created_at ON orders (created_at);"
			input.RollbackSQL = "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_created_at;"
		} else if app == flagID {
			input.Artifacts = []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "flags.yaml", Content: "feature_flags:\n  orders_v2: true"}}
		}
		change, err := svc.Create(input, developer.User.ID)
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		change, err = svc.Submit(change.ID, developer.User.ID)
		if err != nil {
			t.Fatalf("submit %q: %v", title, err)
		}
		if change.Status == model.StatusReadyForExperiment {
			_, err = svc.QueueExperiment(change.ID, developer.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
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
		}
		change, err = svc.Approve(change.ID, owner.User.ID, "approved in fixture")
		if err != nil {
			t.Fatalf("approve %q in %s: %v", title, change.Status, err)
		}
		credential, err := svc.IssuePassport(change.ID, owner.User.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		return changeFixture{model.ReleaseStepPin{ChangeID: change.ID, ArtifactVersion: change.MaterialVersion(), ArtifactSHA256: change.ArtifactSHA256, ProjectID: 42, JobName: job}, sha, credential}
	}
	const plans = "/api/joint-releases"
	create := func(name, old string, changes ...changeFixture) model.JointReleaseView {
		t.Helper()
		pins := make([]model.ReleaseStepPin, 0, len(changes))
		for _, c := range changes {
			pins = append(pins, c.pin)
		}
		var plan model.JointReleaseView
		call(http.MethodPost, plans, &owner, nil, model.JointReleaseInput{Name: name, Environment: env, Supersedes: old,
			Steps: pins}, http.StatusCreated, &plan)
		return plan
	}
	detail := func(planID string) model.JointReleaseView {
		t.Helper()
		var plan model.JointReleaseView
		call(http.MethodGet, plans+"/"+planID, &owner, nil, nil, http.StatusOK, &plan)
		return plan
	}
	setJob := func(c changeFixture, id int64, job string, started time.Time) string {
		mu.Lock()
		defer mu.Unlock()
		token := fmt.Sprintf("test-ci-job-%d", id)
		jobs[token] = map[string]any{"id": id, "status": "running", "name": job, "started_at": started,
			"commit": map[string]any{"id": c.sha}, "pipeline": map[string]any{"id": 900, "project_id": 42, "sha": c.sha},
			"web_url": fmt.Sprintf("%s/-/jobs/%d", project, id)}
		return token
	}
	gate := func(command string, c changeFixture, token string, status int, code string) model.GateResult {
		t.Helper()
		headers := map[string]string{"Authorization": "Bearer " + c.credential.Token}
		if token != "" {
			headers[integration.GitLabJobTokenHeader] = token
		}
		var result model.GateResult
		call(http.MethodPost, "/api/gate/"+command, nil, headers, model.GateRequest{ArtifactSHA256: c.pin.ArtifactSHA256,
			Environment: env, Consumer: "arbitrary-untrusted-label"}, status, &result)
		if result.Code != code || result.Allowed != (status == http.StatusOK) {
			t.Fatalf("%s status=%d code=%s allowed=%v, want %s", command, status, result.Code, result.Allowed, code)
		}
		return result
	}
	emit := func(c changeFixture, job, deployment int64, environment, status string, at time.Time, secret string, accepted bool) {
		t.Helper()
		mu.Lock()
		deployments[fmt.Sprintf("/api/v4/projects/42/deployments/%d", deployment)] = map[string]any{
			"id": deployment, "status": status, "sha": c.sha, "environment": map[string]any{"name": environment},
			"deployable": map[string]any{"id": job, "status": status, "finished_at": at, "commit": map[string]any{"id": c.sha}},
		}
		mu.Unlock()
		hook := map[string]any{"object_kind": "deployment", "status": status, "deployment_id": deployment, "deployable_id": job,
			"deployable_url": fmt.Sprintf("%s/-/jobs/%d", project, job), "status_changed_at": at, "environment": environment,
			"project": map[string]any{"id": 42, "web_url": project}, "short_sha": c.sha[:8]}
		want := http.StatusAccepted
		if secret != hookToken {
			want = http.StatusUnauthorized
		}
		response := call(http.MethodPost, "/api/integrations/gitlab/webhook", nil,
			map[string]string{"X-Gitlab-Event": "Deployment Hook", "X-Gitlab-Token": secret}, hook, want, nil)
		if want == http.StatusAccepted && bytes.Contains(response, []byte(`"ignored":true`)) == accepted {
			t.Fatalf("deployment #%d accepted=%v, response=%s", deployment, accepted, response)
		}
	}

	migration := mkChange("Migrate orders", databaseID, "migrate_orders", strings.Repeat("a", 40), true)
	application := mkChange("Deploy orders", appID, "deploy_orders", strings.Repeat("b", 40), false)
	flag := mkChange("Enable orders v2 feature flag", flagID, "enable_orders_v2", strings.Repeat("f", 40), false)
	call(http.MethodGet, plans, nil, map[string]string{"X-Actor-ID": owner.User.ID}, nil, http.StatusUnauthorized, nil)
	input := model.JointReleaseInput{Name: "Unauthorised", Environment: env, Steps: []model.ReleaseStepPin{migration.pin, application.pin}}
	call(http.MethodPost, plans, &developer, map[string]string{"X-Actor-ID": owner.User.ID}, input, http.StatusForbidden, nil)
	call(http.MethodPost, plans, &owner, map[string]string{"X-CSRF-Token": ""}, input, http.StatusForbidden, nil)
	old := create("Orders plan", "", migration, application, flag)
	asset := httptest.NewRecorder()
	server.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/joint-releases.mjs", nil))
	if asset.Code != http.StatusOK || !strings.HasPrefix(asset.Header().Get("Content-Type"), "application/javascript") ||
		!strings.Contains(asset.Body.String(), "export async function renderJointReleases") {
		t.Fatal("joint release console module is not served as JavaScript")
	}
	call(http.MethodGet, plans+"/"+old.ID, &outsider, nil, nil, http.StatusNotFound, nil)
	var visible struct{ Items []model.JointReleaseView }
	call(http.MethodGet, plans, &owner, nil, nil, http.StatusOK, &visible)
	if len(visible.Items) != 1 || visible.Items[0].ID != old.ID {
		t.Fatal("joint release is missing from its organization's list")
	}
	gate("consume", migration, "", http.StatusForbidden, "RELEASE_JOB_REQUIRED")
	var forged model.GateResult
	call(http.MethodPost, "/api/gate/consume", nil, map[string]string{"Authorization": "Bearer " + migration.credential.Token},
		map[string]any{"artifact_sha256": migration.pin.ArtifactSHA256, "environment": env, "job": map[string]any{"id": 700}}, http.StatusBadRequest, &forged)
	if forged.Code != "INVALID_REQUEST" {
		t.Fatal("client-controlled job object was not rejected")
	}
	gate("consume", migration, setJob(migration, 699, "other_job", time.Now().UTC()), http.StatusForbidden, "RELEASE_JOB_REQUIRED")
	migrationResult := gate("consume", migration, setJob(migration, 700, migration.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	if migrationResult.Passport == nil || migrationResult.Passport.ConsumedBy != "gitlab-job:700" {
		t.Fatal("proved job must determine the Gate consumer label")
	}
	appToken := setJob(application, 701, application.pin.JobName, time.Now().UTC())
	gate("consume", application, appToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", application, "", http.StatusConflict, "RELEASE_BLOCKED")
	call(http.MethodPost, "/api/integrations/gitlab/webhook", nil, map[string]string{"X-Gitlab-Event": "Pipeline Hook", "X-Gitlab-Token": hookToken},
		map[string]any{"object_kind": "pipeline", "object_attributes": map[string]any{"id": 900, "status": "success", "sha": migration.sha}}, http.StatusAccepted, nil)
	gate("consume", application, appToken, http.StatusConflict, "RELEASE_BLOCKED")
	emit(migration, 700, 80, env, "failed", time.Now().UTC(), hookToken, true)
	if detail(old.ID).Timeline[0].DeploymentStatus != model.ReleaseDeploymentFailed {
		t.Fatal("failure evidence not visible on release timeline")
	}
	flagToken := setJob(flag, 710, flag.pin.JobName, time.Now().UTC())
	gate("consume", application, appToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", flag, flagToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", application, "", http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", flag, "", http.StatusConflict, "RELEASE_BLOCKED")

	replacement := mkChange("Repair migration", databaseID, migration.pin.JobName, strings.Repeat("c", 40), true)
	replacement.pin.ReplacesChangeID = migration.pin.ChangeID
	next := create("Replacement plan", old.ID, replacement, application, flag)
	if detail(old.ID).SupersededBy != next.ID {
		t.Fatal("old plan not superseded")
	}
	emit(migration, 700, 80, env, "success", time.Now().UTC(), hookToken, false)
	gate("consume", application, appToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", flag, flagToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", replacement, setJob(replacement, 702, replacement.pin.JobName, old.CreatedAt), http.StatusForbidden, "RELEASE_JOB_REQUIRED")
	gate("consume", replacement, setJob(replacement, 703, replacement.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	emit(replacement, 700, 81, env, "success", time.Now().UTC(), hookToken, false)
	emit(replacement, 703, 82, "staging", "success", time.Now().UTC(), hookToken, false)
	emit(replacement, 703, 83, env, "success", old.CreatedAt, hookToken, false)
	emit(replacement, 703, 84, env, "success", time.Now().UTC(), "bad-secret", false)
	emit(migration, 703, 89, env, "success", time.Now().UTC(), hookToken, false)
	gate("consume", application, appToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", flag, flagToken, http.StatusConflict, "RELEASE_BLOCKED")
	emit(replacement, 703, 85, env, "success", time.Now().UTC(), hookToken, true)
	flagToken = setJob(flag, 711, flag.pin.JobName, time.Now().UTC())
	gate("consume", flag, flagToken, http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", application, appToken, http.StatusForbidden, "RELEASE_JOB_REQUIRED")
	gate("consume", application, setJob(application, 704, application.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	gate("consume", flag, flagToken, http.StatusConflict, "RELEASE_BLOCKED")
	emit(application, 704, 86, env, "success", time.Now().UTC(), hookToken, true)
	gate("consume", flag, flagToken, http.StatusOK, "GATE_ALLOWED")
	if detail(next.ID).Status == "COMPLETED" {
		t.Fatal("consuming the feature flag passport is not deployment success")
	}
	emit(flag, 711, 93, env, "success", time.Now().UTC(), hookToken, true)
	if detail(next.ID).Status != "COMPLETED" {
		t.Fatal("replacement sequence did not complete")
	}

	first := mkChange("Normal serial migration", databaseID, "migrate_orders", strings.Repeat("d", 40), true)
	last := mkChange("Normal serial application", appID, "deploy_orders", strings.Repeat("e", 40), false)
	normal := create("Ordinary serial joint release", "", first, last)
	lastToken := setJob(last, 706, last.pin.JobName, time.Now().UTC())
	gate("verify", last, "", http.StatusConflict, "RELEASE_BLOCKED")
	gate("consume", first, setJob(first, 705, first.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	gate("consume", last, lastToken, http.StatusConflict, "RELEASE_BLOCKED")
	emit(first, 705, 87, env, "success", time.Now().UTC(), hookToken, true)
	gate("verify", last, "", http.StatusOK, "GATE_ALLOWED")
	gate("consume", last, lastToken, http.StatusOK, "GATE_ALLOWED")
	emit(last, 706, 88, env, "success", time.Now().UTC(), hookToken, true)
	if detail(normal.ID).Status != "COMPLETED" {
		t.Fatal("normal sequence did not complete")
	}

	first = mkChange("Migration remains successful", databaseID, "migrate_orders", strings.Repeat("1", 40), true)
	last = mkChange("Application rollout fails", appID, "deploy_orders", strings.Repeat("2", 40), false)
	partial := create("Partially deployed", "", first, last)
	gate("consume", first, setJob(first, 707, first.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	emit(first, 707, 90, env, "success", time.Now().UTC(), hookToken, true)
	gate("consume", last, setJob(last, 708, last.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	emit(last, 708, 91, env, "failed", time.Now().UTC(), hookToken, true)
	fixApp := mkChange("Application rollout repair", appID, "deploy_orders", strings.Repeat("3", 40), false)
	fixApp.pin.ReplacesChangeID = last.pin.ChangeID
	var inherited model.JointReleaseView
	call(http.MethodPost, plans, &owner, nil, model.JointReleaseInput{Name: "Replace only application", Environment: env, Supersedes: partial.ID,
		Steps: []model.ReleaseStepPin{fixApp.pin}}, http.StatusCreated, &inherited)
	if len(inherited.Steps) != 2 || inherited.Steps[0].InheritedFrom != partial.ID || inherited.Timeline[0].DeploymentStatus != model.ReleaseDeploymentSuccess {
		t.Fatal("single-step replacement lost successful predecessor evidence")
	}
	gate("consume", fixApp, setJob(fixApp, 709, fixApp.pin.JobName, time.Now().UTC()), http.StatusOK, "GATE_ALLOWED")
	emit(fixApp, 709, 92, env, "success", time.Now().UTC(), hookToken, true)
	if detail(inherited.ID).Status != "COMPLETED" {
		t.Fatal("single-step replacement did not complete")
	}

	call(http.MethodPut, "/api/enterprise/members/"+developer.User.ID, &owner, nil, model.UpdateMemberInput{
		Role: model.RoleDeveloper, Active: true, ApplicationGrants: []model.ApplicationGrantInput{{ApplicationID: appID, CanSubmit: true}},
	}, http.StatusOK, nil)
	call(http.MethodGet, plans+"/"+normal.ID, &developer, nil, nil, http.StatusNotFound, nil)
}

func TestHTTPJointReleaseWebhookDoesNotDowngradeSigning(t *testing.T) {
	server := &Server{integrations: integration.Config{GitLabSigningToken: "whsec_Zml4dHVyZS1zaWduaW5nLWtleQ==",
		GitLabSecretToken: "legacy-fixture-secret", MaxWebhookAge: time.Minute}}
	request := httptest.NewRequest(http.MethodPost, "/api/integrations/gitlab/webhook", strings.NewReader(`{"object_kind":"deployment"}`))
	request.Header.Set("X-Gitlab-Event", "Deployment Hook")
	request.Header.Set("X-Gitlab-Token", "legacy-fixture-secret")
	response := httptest.NewRecorder()
	server.handleGitLabWebhook(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong response to missing deployment signature: %d", response.Code)
	}
}
