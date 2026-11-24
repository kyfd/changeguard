package httpapi

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
)

func (s *Server) handleIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	events, err := s.service.IntegrationEventsFor(actorID(r), 250)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	signals, err := s.service.OutcomeSignalsFor(actorID(r), 0)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var gitLabLast, jenkinsLast, operationsLast *time.Time
	for _, event := range events {
		received := event.ReceivedAt
		if event.Provider == "GITLAB" && gitLabLast == nil {
			gitLabLast = &received
		}
		if event.Provider == "JENKINS" && jenkinsLast == nil {
			jenkinsLast = &received
		}
	}
	for _, signal := range signals {
		if operationsLast == nil || signal.ReceivedAt.After(*operationsLast) {
			received := signal.ReceivedAt
			operationsLast = &received
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CHANGEGUARD_PUBLIC_URL")), "/")
	if baseURL == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	gitLabAuth := "X-Gitlab-Token（兼容模式）"
	gitLabEvents := []string{"Pipeline Hook"}
	if s.gitLabRelease != nil {
		gitLabEvents = append(gitLabEvents, "Deployment Hook")
	}
	if s.integrations.GitLabSigningToken != "" {
		gitLabAuth = "HMAC-SHA256 Signing Token（推荐）"
	}
	status := model.IntegrationStatus{
		GitLab: model.IntegrationProviderStatus{
			Provider: "GITLAB", Configured: s.integrations.GitLabConfigured(),
			Authentication:  gitLabAuth,
			Endpoint:        baseURL + "/api/integrations/gitlab/webhook",
			SupportedEvents: gitLabEvents,
			LastReceivedAt:  gitLabLast,
		},
		Jenkins: model.IntegrationProviderStatus{
			Provider: "JENKINS", Configured: s.integrations.JenkinsConfigured(),
			Authentication:  "Authorization: Bearer Token",
			Endpoint:        baseURL + "/api/integrations/jenkins/events",
			SupportedEvents: []string{"RUNNING", "SUCCESS", "FAILURE", "ABORTED", "UNSTABLE"},
			LastReceivedAt:  jenkinsLast,
		},
		Operations: model.IntegrationProviderStatus{
			Provider: "OPERATIONS", Configured: s.integrations.OperationsConfigured(),
			Authentication:  "Authorization: Bearer Token",
			Endpoint:        baseURL + "/api/integrations/operations/webhook",
			SupportedEvents: []string{"INCIDENT", "ROLLBACK", "BUSINESS_SLI"},
			LastReceivedAt:  operationsLast,
		},
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleIntegrationEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 500 之间")
			return
		}
		limit = parsed
	}
	events, err := s.service.IntegrationEventsFor(actorID(r), limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GitLab Webhook 请求体无效")
		return
	}
	received := time.Now().UTC()
	if integration.IsGitLabDeployment(r.Header) {
		s.handleGitLabDeployment(w, r, body, received)
		return
	}
	if err := integration.VerifyGitLab(s.integrations, r.Header, body, received); err != nil {
		writeIntegrationError(w, err)
		return
	}
	event, err := integration.ParseGitLab(s.integrations, r.Header, body)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	s.recordIntegrationEvent(w, event)
}

func (s *Server) handleJenkinsEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Jenkins 事件请求体无效")
		return
	}
	if err := integration.VerifyJenkins(s.integrations, r.Header); err != nil {
		writeIntegrationError(w, err)
		return
	}
	event, err := integration.ParseJenkins(s.integrations, r.Header, body)
	if err != nil {
		writeIntegrationError(w, err)
		return
	}
	s.recordIntegrationEvent(w, event)
}

func (s *Server) handleOperationsEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 500 之间")
			return
		}
		limit = parsed
	}
	signals, err := s.service.OutcomeSignalsFor(actorID(r), limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": signals})
}

func (s *Server) handleOperationsWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := readIntegrationBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "运维结果事件请求体无效")
		return
	}
	if err := integration.VerifyOperations(s.integrations, r.Header); err != nil {
		writeOperationsError(w, err)
		return
	}
	signal, err := integration.ParseOperations(s.integrations, body, time.Now())
	if err != nil {
		writeOperationsError(w, err)
		return
	}
	recorded, created, err := s.service.RecordOutcomeSignal(signal)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"accepted": true, "duplicate": !created,
		"event_id": recorded.ExternalID, "signal_id": recorded.ID,
		"change_id": recorded.ChangeID, "kind": recorded.Kind,
	})
}

func (s *Server) recordIntegrationEvent(w http.ResponseWriter, event model.IntegrationEvent) {
	recorded, created, err := s.service.RecordIntegrationEvent(event)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"accepted": true, "duplicate": !created,
		"event_id": recorded.ID, "change_id": recorded.ChangeID,
	})
}

func readIntegrationBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
}

func writeIntegrationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "流水线集成尚未配置")
	case errors.Is(err, integration.ErrUnauthorized), errors.Is(err, integration.ErrReplay):
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"changeguard-integration\"")
		writeError(w, http.StatusUnauthorized, "流水线凭据或签名无效")
	case errors.Is(err, integration.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "仅支持 GitLab Pipeline Hook 或 Deployment Hook")
	default:
		writeError(w, http.StatusBadRequest, "流水线事件格式无效")
	}
}

func writeOperationsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "运维结果集成尚未配置")
	case errors.Is(err, integration.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"changeguard-operations\"")
		writeError(w, http.StatusUnauthorized, "运维结果凭据无效")
	case errors.Is(err, integration.ErrReplay):
		writeError(w, http.StatusBadRequest, "运维结果事件时间超出允许范围")
	case errors.Is(err, integration.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "仅支持 INCIDENT、ROLLBACK 和 BUSINESS_SLI 事件")
	default:
		writeError(w, http.StatusBadRequest, "运维结果事件格式无效")
	}
}
