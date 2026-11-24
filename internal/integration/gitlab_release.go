package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

const GitLabJobTokenHeader = "X-ChangeGuard-GitLab-Job-Token"

var (
	ErrGitLabUnavailable = errors.New("trusted GitLab API did not provide usable evidence")
	projectPath          = regexp.MustCompile(`^(/[A-Za-z0-9_.-]+){2,}$`)
)

// GitLabReleaseClient only calls paths constructed locally on the configured HTTPS origin.
// Neither webhook URLs nor CI environment values are ever used as request targets.
type GitLabReleaseClient struct {
	source model.ReleaseSource
	token  string
	http   *http.Client
}

func NewGitLabReleaseClient(cfg Config, transport http.RoundTripper) (*GitLabReleaseClient, error) {
	if cfg.GitLabReleaseOrigin == "" && cfg.GitLabReleaseProjects == "" && cfg.GitLabReadAPIToken == "" {
		return nil, nil
	}
	base, err := url.Parse(strings.TrimRight(cfg.GitLabReleaseOrigin, "/"))
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil ||
		base.Path != "" || base.ForceQuery || base.RawQuery != "" || strings.Contains(cfg.GitLabReleaseOrigin, "#") ||
		!cfg.GitLabConfigured() || cfg.GitLabOrganization == "" || !validGitLabToken(cfg.GitLabReadAPIToken) {
		return nil, ErrNotConfigured
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(cfg.GitLabReleaseProjects), &configured); err != nil || len(configured) == 0 {
		return nil, ErrNotConfigured
	}
	projects := make(map[int64]string, len(configured))
	for id, raw := range configured {
		projectID, err := strconv.ParseInt(id, 10, 64)
		u, parseErr := url.Parse(raw)
		if err != nil || projectID <= 0 || strconv.FormatInt(projectID, 10) != id || parseErr != nil ||
			u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.RawPath != "" || u.ForceQuery || u.RawQuery != "" ||
			strings.Contains(raw, "#") || strings.HasSuffix(u.Path, ".git") || !projectPath.MatchString(u.Path) || raw != base.String()+u.Path {
			return nil, ErrNotConfigured
		}
		for _, part := range strings.Split(u.Path, "/") {
			if part == "." || part == ".." || part == "-" {
				return nil, ErrNotConfigured
			}
		}
		projects[projectID] = raw
	}
	return &GitLabReleaseClient{
		source: model.ReleaseSource{OrganizationID: cfg.GitLabOrganization, URL: base.String(), Projects: projects},
		token:  cfg.GitLabReadAPIToken,
		http: &http.Client{Transport: transport, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *GitLabReleaseClient) Source() model.ReleaseSource {
	if c == nil {
		return model.ReleaseSource{}
	}
	source := c.source
	source.Projects = make(map[int64]string, len(c.source.Projects))
	for id, project := range c.source.Projects {
		source.Projects[id] = project
	}
	return source
}

func validGitLabToken(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, b := range []byte(value) {
		if b <= ' ' || b >= 127 || b == ',' {
			return false
		}
	}
	return true
}

func (c *GitLabReleaseClient) get(ctx context.Context, path, header, token string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.source.URL+"/api/v4"+path, nil)
	if err != nil {
		return ErrGitLabUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(header, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrGitLabUnavailable
	}
	defer resp.Body.Close()
	if header == "JOB-TOKEN" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return ErrGitLabUnavailable
	}
	const maxResponse = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, target) != nil {
		return ErrGitLabUnavailable
	}
	return nil
}

func commitSHA(value string) string {
	if _, err := hex.DecodeString(value); len(value) != 40 || err != nil {
		return ""
	}
	return strings.ToLower(value)
}

type gitLabAPIJob struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	WebURL     string    `json:"web_url"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Commit     struct {
		ID string `json:"id"`
	} `json:"commit"`
	Pipeline struct {
		ID        int64  `json:"id"`
		ProjectID int64  `json:"project_id"`
		SHA       string `json:"sha"`
	} `json:"pipeline"`
}

func (c *GitLabReleaseClient) ResolveJob(ctx context.Context, token string, now time.Time) (*model.GitLabJob, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if !validGitLabToken(token) {
		return nil, ErrUnauthorized
	}
	var api gitLabAPIJob
	if err := c.get(ctx, "/job", "JOB-TOKEN", token, &api); err != nil {
		return nil, err
	}
	project := c.source.Projects[api.Pipeline.ProjectID]
	sha := commitSHA(api.Commit.ID)
	if api.ID <= 0 || api.Pipeline.ID <= 0 || project == "" || api.WebURL != jobURL(project, api.ID) ||
		api.Name == "" || api.Status != "running" || api.StartedAt.IsZero() || api.StartedAt.After(now) || !api.FinishedAt.IsZero() ||
		sha == "" || sha != commitSHA(api.Pipeline.SHA) {
		return nil, ErrUnauthorized
	}
	return &model.GitLabJob{Source: c.source.URL, OrganizationID: c.source.OrganizationID,
		ProjectID: api.Pipeline.ProjectID, JobID: api.ID, PipelineID: api.Pipeline.ID,
		Name: api.Name, CommitSHA: sha, StartedAt: api.StartedAt.UTC()}, nil
}

func jobURL(project string, id int64) string {
	return project + "/-/jobs/" + strconv.FormatInt(id, 10)
}

func IsGitLabDeployment(headers http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(headers.Get("X-Gitlab-Event")), "Deployment Hook")
}

type gitLabDeploymentHook struct {
	Kind            string `json:"object_kind"`
	Status          string `json:"status"`
	StatusChangedAt string `json:"status_changed_at"`
	DeploymentID    int64  `json:"deployment_id"`
	JobID           int64  `json:"deployable_id"`
	JobURL          string `json:"deployable_url"`
	Environment     string `json:"environment"`
	ShortSHA        string `json:"short_sha"`
	Project         struct {
		ID     int64  `json:"id"`
		WebURL string `json:"web_url"`
	} `json:"project"`
}

type gitLabAPIDeployment struct {
	ID          int64  `json:"id"`
	SHA         string `json:"sha"`
	Status      string `json:"status"`
	Environment struct {
		Name string `json:"name"`
	} `json:"environment"`
	Deployable *gitLabAPIJob `json:"deployable"`
}

// ResolveDeployment requires an authenticated hook at the caller. The documented
// hook has only a short SHA; neither it nor its URLs can prove the full commit.
// Use the project-scoped Deployments API and require matching terminal job evidence.
// A nil event means a non-terminal notification that must not update release state.
func (c *GitLabReleaseClient) ResolveDeployment(ctx context.Context, body []byte, received time.Time) (*model.GitLabDeployment, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	var hook gitLabDeploymentHook
	if err := json.Unmarshal(body, &hook); err != nil || hook.Kind != "deployment" || hook.Status == "" || hook.DeploymentID <= 0 || hook.Project.ID <= 0 {
		return nil, ErrInvalidPayload
	}
	project := c.source.Projects[hook.Project.ID]
	if project == "" || hook.Project.WebURL != "" && hook.Project.WebURL != project {
		return nil, ErrUnauthorized
	}
	status := terminalDeploymentStatus(hook.Status)
	if status == "" {
		return nil, nil
	}
	var notifiedAt time.Time
	if hook.StatusChangedAt != "" {
		notifiedAt = firstEventTime(hook.StatusChangedAt)
		if notifiedAt.IsZero() || notifiedAt.After(received.Add(5*time.Second)) {
			return nil, ErrInvalidPayload
		}
	}
	var api gitLabAPIDeployment
	if err := c.get(ctx, fmt.Sprintf("/projects/%d/deployments/%d", hook.Project.ID, hook.DeploymentID), "PRIVATE-TOKEN", c.token, &api); err != nil {
		return nil, err
	}
	job := api.Deployable
	sha := commitSHA(api.SHA)
	if api.ID != hook.DeploymentID || api.Status != hook.Status || job == nil || job.ID <= 0 ||
		job.Status != api.Status || job.FinishedAt.IsZero() || job.FinishedAt.After(received.Add(5*time.Second)) ||
		sha == "" || sha != commitSHA(job.Commit.ID) || api.Environment.Name == "" ||
		(hook.JobID != 0 && hook.JobID != job.ID) || (hook.JobURL != "" && hook.JobURL != jobURL(project, job.ID)) ||
		(job.WebURL != "" && job.WebURL != jobURL(project, job.ID)) ||
		(hook.Environment != "" && hook.Environment != api.Environment.Name) ||
		(hook.ShortSHA != "" && !strings.HasPrefix(sha, strings.ToLower(hook.ShortSHA))) {
		return nil, ErrInvalidPayload
	}
	// Use the earlier observed terminal time so BOTH clocks must be later than
	// Gate consumption. Never substitute webhook receipt wall time for evidence.
	occurred := job.FinishedAt.UTC()
	if !notifiedAt.IsZero() && notifiedAt.Before(occurred) {
		occurred = notifiedAt
	}
	return &model.GitLabDeployment{Source: c.source.URL, OrganizationID: c.source.OrganizationID,
		ProjectID: hook.Project.ID, ProjectURL: project, JobID: job.ID, DeploymentID: api.ID,
		CommitSHA: sha, Environment: api.Environment.Name, Status: status, OccurredAt: occurred, ReceivedAt: received.UTC()}, nil
}

func terminalDeploymentStatus(status string) string {
	switch status {
	case "success":
		return model.ReleaseDeploymentSuccess
	case "failed":
		return model.ReleaseDeploymentFailed
	case "canceled":
		return model.ReleaseDeploymentCanceled
	default:
		return ""
	}
}
