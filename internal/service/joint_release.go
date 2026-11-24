package service

import (
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func (s *Service) CreateJointRelease(actorID string, input model.JointReleaseInput, source model.ReleaseSource) (model.JointReleaseView, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.JointReleaseView{}, store.ErrReleasePermission
	}
	return s.store.CreateJointRelease(actor.ID, actor.OrganizationID, input, source)
}

func (s *Service) JointReleasesFor(actorID, id string) ([]model.JointReleaseView, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, store.ErrReleasePermission
	}
	return s.store.JointReleasesFor(actor.ID, actor.OrganizationID, strings.TrimSpace(id))
}

// RecordGitLabDeployment receives webhook evidence only after source
// authentication. The store can attach it only to the bound execution.
func (s *Service) RecordGitLabDeployment(event model.GitLabDeployment) (bool, error) {
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	return s.store.RecordGitLabDeployment(event)
}
