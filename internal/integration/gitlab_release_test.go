package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const fixtureProject = "https://gitlab.example.com/root/test-deployment-webhooks"

type releaseRoundTripper func(*http.Request) (*http.Response, error)

func (f releaseRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func releaseConfig() Config {
	return Config{GitLabReleaseOrigin: "https://gitlab.example.com", GitLabReleaseProjects: `{"30":"` + fixtureProject + `"}`,
		GitLabOrganization: "org_fixture", GitLabSecretToken: "hook-secret", GitLabReadAPIToken: "private-test-token"}
}

func gitLabFixture(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func releaseResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGitLabReleaseDocumentedProtocol(t *testing.T) {
	for _, status := range []string{"success", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			jobJSON := gitLabFixture(t, "gitlab_job")
			deploymentJSON := strings.ReplaceAll(gitLabFixture(t, "gitlab_deployment_api"), `"success"`, `"`+status+`"`)
			hook := strings.ReplaceAll(gitLabFixture(t, "gitlab_deployment_hook"), `"success"`, `"`+status+`"`)
			client, err := NewGitLabReleaseClient(releaseConfig(), releaseRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "gitlab.example.com" || r.URL.Scheme != "https" || r.URL.RawQuery != "" || r.Method != http.MethodGet {
					t.Fatalf("unexpected GitLab request: %v %v", r.Method, r.URL)
				}
				switch r.URL.Path {
				case "/api/v4/job":
					if r.Header.Get("JOB-TOKEN") != "job-test-token" || r.Header.Get("PRIVATE-TOKEN") != "" {
						t.Fatal("incorrect job credentials")
					}
					return releaseResponse(200, jobJSON), nil
				case "/api/v4/projects/30/deployments/15":
					if r.Header.Get("PRIVATE-TOKEN") != "private-test-token" || r.Header.Get("JOB-TOKEN") != "" {
						t.Fatal("incorrect API credentials")
					}
					return releaseResponse(200, deploymentJSON), nil
				default:
					t.Fatalf("unexpected GitLab path %s", r.URL.Path)
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 24, 12, 1, 1, 0, time.UTC)
			job, err := client.ResolveJob(t.Context(), "job-test-token", now)
			if err != nil || job.ProjectID != 30 || job.JobID != 796 || job.PipelineID != 142 || job.OrganizationID != "org_fixture" || job.Name != "migrate_orders" {
				t.Fatalf("unexpected job: %+v / %v", job, err)
			}
			event, err := client.ResolveDeployment(t.Context(), []byte(hook), now)
			if err != nil || event.ProjectURL != fixtureProject || event.JobID != job.JobID || event.DeploymentID != 15 ||
				event.CommitSHA != job.CommitSHA || event.Status != strings.ToUpper(status) || event.Environment != "production" ||
				!event.ReceivedAt.Equal(now) || !event.OccurredAt.Equal(now.Add(-time.Second)) {
				t.Fatalf("unexpected deployment: %+v / %v", event, err)
			}
		})
	}
}

func TestGitLabReleaseFailsClosed(t *testing.T) {
	job := gitLabFixture(t, "gitlab_job")
	api := gitLabFixture(t, "gitlab_deployment_api")
	hook := gitLabFixture(t, "gitlab_deployment_hook")
	now := time.Date(2026, 11, 24, 12, 1, 1, 0, time.UTC)
	for _, tc := range []struct {
		name, override  string
		job, replaceAPI bool
		want            error
	}{
		{"finished job", strings.Replace(job, `"running"`, `"success"`, 1), true, false, ErrUnauthorized},
		{"foreign project", strings.Replace(job, `"project_id": 30`, `"project_id": 31`, 1), true, false, ErrUnauthorized},
		{"foreign job URL", strings.Replace(job, fixtureProject, "https://untrusted.example.com/root/test-deployment-webhooks", 1), true, false, ErrUnauthorized},
		{"wrong job", strings.Replace(hook, `"deployable_id": 796`, `"deployable_id": 795`, 1), false, false, ErrInvalidPayload},
		{"wrong environment", strings.Replace(hook, `"environment": "production"`, `"environment": "test"`, 1), false, false, ErrInvalidPayload},
		{"wrong sha", strings.Replace(hook, `"279484c0"`, `"00000000"`, 1), false, false, ErrInvalidPayload},
		{"unallowlisted hook", strings.Replace(hook, `"id": 30`, `"id": 31`, 1), false, false, ErrUnauthorized},
		{"nonterminal", strings.Replace(hook, `"success"`, `"running"`, 1), false, false, nil},
		{"API deployment without job", strings.Replace(api, `"id": 796`, `"id": 0`, 1), false, true, ErrInvalidPayload},
		{"API job still running", strings.Replace(api, "\"status\": \"success\",\n    \"stage\"", "\"status\": \"running\",\n    \"stage\"", 1), false, true, ErrInvalidPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, err := NewGitLabReleaseClient(releaseConfig(), releaseRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				if tc.job || tc.replaceAPI {
					return releaseResponse(200, tc.override), nil
				}
				return releaseResponse(200, api), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if tc.job {
				result, err := client.ResolveJob(t.Context(), "job-token", now)
				if !errors.Is(err, tc.want) || result != nil {
					t.Fatalf("ResolveJob result=%+v err=%v", result, err)
				}
				return
			}
			body := tc.override
			if tc.replaceAPI {
				body = hook
			}
			result, err := client.ResolveDeployment(t.Context(), []byte(body), now)
			if !errors.Is(err, tc.want) || result != nil || (tc.want == nil || tc.want == ErrUnauthorized) && calls != 0 {
				t.Fatalf("ResolveDeployment result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
	client, err := NewGitLabReleaseClient(releaseConfig(), releaseRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "gitlab.example.com" {
			t.Fatal("followed untrusted redirect")
		}
		response := releaseResponse(http.StatusTemporaryRedirect, "never-log-a-secret")
		response.Header.Set("Location", "https://untrusted.example.com/collect")
		return response, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveJob(context.Background(), "secret-ci-token", now); !errors.Is(err, ErrGitLabUnavailable) {
		t.Fatalf("job redirect was not refused: %v", err)
	}
	if _, err := client.ResolveDeployment(t.Context(), []byte(hook), now); !errors.Is(err, ErrGitLabUnavailable) {
		t.Fatalf("deployment redirect was not refused: %v", err)
	}
}

func TestGitLabReleaseUsesAPIWhenWebhookOmitsFields(t *testing.T) {
	api := gitLabFixture(t, "gitlab_deployment_api")
	client, err := NewGitLabReleaseClient(releaseConfig(), releaseRoundTripper(func(*http.Request) (*http.Response, error) {
		return releaseResponse(200, api), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	minimal := []byte(`{"object_kind":"deployment","status":"success","deployment_id":15,"project":{"id":30}}`)
	now := time.Date(2026, 11, 24, 12, 1, 1, 0, time.UTC)
	event, err := client.ResolveDeployment(t.Context(), minimal, now)
	if err != nil || event.JobID != 796 || event.Environment != "production" || event.OccurredAt.Nanosecond() != 4_000_000 {
		t.Fatalf("fallback evidence: %+v / %v", event, err)
	}
}
