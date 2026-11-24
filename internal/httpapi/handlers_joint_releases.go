package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func (s *Server) handleJointReleases(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() {
		writeCodedError(w, http.StatusServiceUnavailable, "SESSION_AUTH_REQUIRED", "联合发布需要启用会话认证")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.service.JointReleasesFor(actorID(r), "")
		if err != nil {
			writeJointReleaseError(w, err)
			return
		}
		if items == nil {
			items = []model.JointReleaseView{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var input model.JointReleaseInput
		if err := decodeJSON(r, &input); err != nil {
			writeCodedError(w, http.StatusBadRequest, "RELEASE_INPUT", "联合发布请求格式不正确")
			return
		}
		plan, err := s.service.CreateJointRelease(actorID(r), input, s.gitLabRelease.Source())
		if err != nil {
			writeJointReleaseError(w, err)
			return
		}
		w.Header().Set("Location", "/api/joint-releases/"+plan.ID)
		writeJSON(w, http.StatusCreated, plan)
	default:
		w.Header().Set("Allow", "GET, POST")
		methodNotAllowed(w)
	}
}

func (s *Server) handleJointRelease(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() {
		writeCodedError(w, http.StatusServiceUnavailable, "SESSION_AUTH_REQUIRED", "联合发布需要启用会话认证")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		methodNotAllowed(w)
		return
	}
	items, err := s.service.JointReleasesFor(actorID(r), r.PathValue("id"))
	if err != nil {
		writeJointReleaseError(w, err)
		return
	}
	if len(items) != 1 {
		writeServiceError(w, store.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, items[0])
}

func writeJointReleaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrReleasePermission):
		writeCodedError(w, http.StatusForbidden, "RELEASE_PERMISSION", err.Error())
	case errors.Is(err, store.ErrReleaseInput):
		writeCodedError(w, http.StatusBadRequest, "RELEASE_INPUT", err.Error())
	case errors.Is(err, store.ErrReleaseBlocked):
		writeCodedError(w, http.StatusConflict, "RELEASE_BLOCKED", err.Error())
	default:
		writeServiceError(w, err)
	}
}

func (s *Server) handleGitLabDeployment(w http.ResponseWriter, r *http.Request, body []byte, received time.Time) {
	// When signing is configured, this enforcement path does not downgrade to
	// the compatibility secret. Pipeline telemetry retains its existing behavior.
	config := s.integrations
	if config.GitLabSigningToken != "" {
		config.GitLabSecretToken = ""
		if r.Header.Get("webhook-signature") == "" {
			writeIntegrationError(w, integration.ErrUnauthorized)
			return
		}
	}
	if err := integration.VerifyGitLab(config, r.Header, body, received); err != nil {
		writeIntegrationError(w, err)
		return
	}
	event, err := s.gitLabRelease.ResolveDeployment(r.Context(), body, received)
	if err != nil {
		if errors.Is(err, integration.ErrGitLabUnavailable) {
			writeCodedError(w, http.StatusServiceUnavailable, "GITLAB_EVIDENCE_UNAVAILABLE", "暂时无法核实 GitLab 部署证据")
		} else {
			writeIntegrationError(w, err)
		}
		return
	}
	if event == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"ignored": true})
		return
	}
	created, err := s.service.RecordGitLabDeployment(*event)
	if errors.Is(err, store.ErrReleaseEvidence) {
		// The configured project can have unrelated jobs, or late events for a
		// superseded release. Acknowledge without recording or exposing plan data.
		writeJSON(w, http.StatusAccepted, map[string]any{"ignored": true})
		return
	}
	if err != nil {
		writeJointReleaseError(w, err)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"accepted": true, "duplicate": !created})
}
