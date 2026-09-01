package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

var (
	ErrNotConfigured  = errors.New("integration is not configured")
	ErrUnauthorized   = errors.New("integration credential is invalid")
	ErrReplay         = errors.New("webhook timestamp is outside the allowed window")
	ErrUnsupported    = errors.New("webhook event is not supported")
	ErrInvalidPayload = errors.New("webhook payload is invalid")
)

type Config struct {
	GitLabSigningToken     string
	GitLabSecretToken      string
	GitLabOrganization     string
	JenkinsToken           string
	JenkinsOrganization    string
	OperationsToken        string
	OperationsOrganization string
	MaxWebhookAge          time.Duration
}

func FromEnvironment() Config {
	maxAge := 5 * time.Minute
	if value := strings.TrimSpace(os.Getenv("DBGUARD_GITLAB_WEBHOOK_MAX_AGE")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			maxAge = parsed
		}
	}
	return Config{
		GitLabSigningToken:     strings.TrimSpace(os.Getenv("DBGUARD_GITLAB_SIGNING_TOKEN")),
		GitLabSecretToken:      strings.TrimSpace(os.Getenv("DBGUARD_GITLAB_WEBHOOK_SECRET")),
		GitLabOrganization:     envOr("DBGUARD_GITLAB_ORGANIZATION_ID", "org_demo"),
		JenkinsToken:           strings.TrimSpace(os.Getenv("DBGUARD_JENKINS_WEBHOOK_TOKEN")),
		JenkinsOrganization:    envOr("DBGUARD_JENKINS_ORGANIZATION_ID", "org_demo"),
		OperationsToken:        strings.TrimSpace(os.Getenv("DBGUARD_OPERATIONS_WEBHOOK_TOKEN")),
		OperationsOrganization: envOr("DBGUARD_OPERATIONS_ORGANIZATION_ID", "org_demo"),
		MaxWebhookAge:          maxAge,
	}
}

func (config Config) GitLabConfigured() bool {
	return config.GitLabSigningToken != "" || config.GitLabSecretToken != ""
}

func (config Config) JenkinsConfigured() bool {
	return config.JenkinsToken != ""
}

func (config Config) OperationsConfigured() bool {
	return config.OperationsToken != ""
}

func VerifyGitLab(config Config, headers http.Header, body []byte, now time.Time) error {
	signature := strings.TrimSpace(headers.Get("webhook-signature"))
	if signature != "" {
		if config.GitLabSigningToken == "" {
			return ErrNotConfigured
		}
		messageID := strings.TrimSpace(headers.Get("webhook-id"))
		timestampValue := strings.TrimSpace(headers.Get("webhook-timestamp"))
		timestamp, err := strconv.ParseInt(timestampValue, 10, 64)
		if err != nil || messageID == "" {
			return ErrUnauthorized
		}
		sentAt := time.Unix(timestamp, 0)
		age := now.Sub(sentAt)
		if age < -config.MaxWebhookAge || age > config.MaxWebhookAge {
			return ErrReplay
		}
		token := strings.TrimPrefix(config.GitLabSigningToken, "whsec_")
		key, err := base64.StdEncoding.DecodeString(token)
		if err != nil || len(key) == 0 {
			return ErrNotConfigured
		}
		message := messageID + "." + timestampValue + "." + string(body)
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(message))
		expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		for _, candidate := range strings.Fields(signature) {
			if subtle.ConstantTimeCompare([]byte(expected), []byte(candidate)) == 1 {
				return nil
			}
		}
		return ErrUnauthorized
	}
	if config.GitLabSecretToken == "" {
		return ErrNotConfigured
	}
	provided := strings.TrimSpace(headers.Get("X-Gitlab-Token"))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(config.GitLabSecretToken)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

func VerifyJenkins(config Config, headers http.Header) error {
	if config.JenkinsToken == "" {
		return ErrNotConfigured
	}
	provided := bearerToken(headers.Get("Authorization"))
	if provided == "" {
		provided = strings.TrimSpace(headers.Get("X-ChangeGuard-Token"))
	}
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(config.JenkinsToken)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

type gitLabPayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
		Status     string `json:"status"`
		URL        string `json:"url"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
		FinishedAt string `json:"finished_at"`
		Variables  []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"variables"`
	} `json:"object_attributes"`
	Project struct {
		ID                int64  `json:"id"`
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
}

func ParseGitLab(config Config, headers http.Header, body []byte) (model.IntegrationEvent, error) {
	if !strings.EqualFold(strings.TrimSpace(headers.Get("X-Gitlab-Event")), "Pipeline Hook") {
		return model.IntegrationEvent{}, ErrUnsupported
	}
	var payload gitLabPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return model.IntegrationEvent{}, ErrInvalidPayload
	}
	if !strings.EqualFold(payload.ObjectKind, "pipeline") || payload.ObjectAttributes.ID == 0 {
		return model.IntegrationEvent{}, ErrInvalidPayload
	}
	changeID := ""
	for _, variable := range payload.ObjectAttributes.Variables {
		if strings.EqualFold(strings.TrimSpace(variable.Key), "CHANGEGUARD_CHANGE_ID") {
			changeID = clean(variable.Value, 128)
			break
		}
	}
	externalID := firstNonEmpty(
		headers.Get("webhook-id"),
		headers.Get("Idempotency-Key"),
		headers.Get("X-Gitlab-Event-UUID"),
	)
	if externalID == "" {
		sum := sha256.Sum256(body)
		externalID = hex.EncodeToString(sum[:])
	}
	project := firstNonEmpty(payload.Project.PathWithNamespace, payload.Project.Name)
	status := strings.ToUpper(clean(payload.ObjectAttributes.Status, 48))
	pipeline := strconv.FormatInt(payload.ObjectAttributes.ID, 10)
	occurredAt := firstEventTime(payload.ObjectAttributes.FinishedAt, payload.ObjectAttributes.UpdatedAt, payload.ObjectAttributes.CreatedAt)
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return model.IntegrationEvent{
		OrganizationID: config.GitLabOrganization,
		Provider:       "GITLAB", ExternalID: clean(externalID, 160),
		EventType: "PIPELINE", Status: status, ChangeID: changeID,
		Project: clean(project, 200), Pipeline: pipeline,
		CommitSHA:   clean(payload.ObjectAttributes.SHA, 128),
		ExternalURL: safeExternalURL(payload.ObjectAttributes.URL),
		Detail:      fmt.Sprintf("GitLab 流水线 %s 状态：%s", pipeline, status),
		OccurredAt:  occurredAt,
		ReceivedAt:  time.Now(),
	}, nil
}

type JenkinsPayload struct {
	ChangeID    string    `json:"change_id"`
	JobName     string    `json:"job_name"`
	BuildNumber int64     `json:"build_number"`
	BuildURL    string    `json:"build_url"`
	Status      string    `json:"status"`
	CommitSHA   string    `json:"commit_sha"`
	Environment string    `json:"environment"`
	OccurredAt  time.Time `json:"occurred_at,omitempty"`
}

func ParseJenkins(config Config, headers http.Header, body []byte) (model.IntegrationEvent, error) {
	var payload JenkinsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return model.IntegrationEvent{}, ErrInvalidPayload
	}
	payload.JobName = clean(payload.JobName, 200)
	payload.Status = strings.ToUpper(clean(payload.Status, 48))
	if payload.JobName == "" || payload.BuildNumber <= 0 || payload.Status == "" {
		return model.IntegrationEvent{}, ErrInvalidPayload
	}
	build := strconv.FormatInt(payload.BuildNumber, 10)
	externalID := clean(headers.Get("X-ChangeGuard-Event-ID"), 160)
	if externalID == "" {
		externalID = strings.Join([]string{"jenkins", payload.JobName, build, payload.Status}, ":")
	}
	occurredAt := payload.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return model.IntegrationEvent{
		OrganizationID: config.JenkinsOrganization,
		Provider:       "JENKINS", ExternalID: externalID,
		EventType: "BUILD", Status: payload.Status,
		ChangeID: clean(payload.ChangeID, 128),
		Project:  payload.JobName, Pipeline: build,
		CommitSHA:   clean(payload.CommitSHA, 128),
		ExternalURL: safeExternalURL(payload.BuildURL),
		Detail:      fmt.Sprintf("Jenkins 构建 %s #%s 状态：%s", payload.JobName, build, payload.Status),
		OccurredAt:  occurredAt,
		ReceivedAt:  time.Now(),
	}, nil
}

func safeExternalURL(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return clean(value, 1000)
}

func clean(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func bearerToken(value string) string {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func firstEventTime(values ...string) time.Time {
	for _, value := range values {
		value = strings.TrimSpace(value)
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
