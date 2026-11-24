package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func (s *Service) CreateApplication(input model.SaveApplicationInput, actorID string) (model.Application, error) {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return model.Application{}, ErrForbidden
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Database), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Schema), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Owner), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) ||
		!validRuneLength(strings.TrimSpace(input.Environment), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.Description), 0, 2000) {
		return model.Application{}, fmt.Errorf("%w：应用字段为空或长度超出限制", ErrValidation)
	}
	dependencies := normalizeStringList(input.Dependencies)
	if err := s.validateApplicationDependencies(actor.OrganizationID, "", dependencies); err != nil {
		return model.Application{}, err
	}
	application := model.Application{
		OrganizationID: actor.OrganizationID, ID: store.NewID("app_"),
		Name: strings.TrimSpace(input.Name), Owner: strings.TrimSpace(input.Owner),
		Kind: defaultString(input.Kind, "后端服务"), Runtime: defaultString(input.Runtime, "Go"),
		RepositoryURL: strings.TrimSpace(input.RepositoryURL), Tier: defaultString(input.Tier, "重要"),
		Lifecycle: defaultString(input.Lifecycle, "生产运行"),
		Database:  strings.TrimSpace(input.Database), Schema: strings.TrimSpace(input.Schema),
		Environment: strings.TrimSpace(input.Environment), Dependencies: dependencies,
		Tags: normalizeStringList(input.Tags), Description: strings.TrimSpace(input.Description),
	}
	if application.Environment == "" {
		application.Environment = "生产 / 只读"
	}
	err = s.store.CreateApplication(application, audit(actor, "", "CREATE_APPLICATION", "纳管应用 "+application.Name))
	if errors.Is(err, store.ErrConflict) {
		return model.Application{}, fmt.Errorf("%w：应用名称或数据库 Schema 已存在", ErrValidation)
	}
	return application, err
}

func (s *Service) UpdateApplication(id string, input model.SaveApplicationInput, actorID string) (model.Application, error) {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return model.Application{}, ErrForbidden
	}
	if !validRuneLength(strings.TrimSpace(input.Name), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.Database), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Schema), 0, 100) ||
		!validRuneLength(strings.TrimSpace(input.Owner), 1, 100) ||
		!validRuneLength(strings.TrimSpace(input.RepositoryURL), 0, 500) ||
		!validRuneLength(strings.TrimSpace(input.Environment), 0, 120) ||
		!validRuneLength(strings.TrimSpace(input.Description), 0, 2000) {
		return model.Application{}, fmt.Errorf("%w：应用字段为空或长度超出限制", ErrValidation)
	}
	dependencies := normalizeStringList(input.Dependencies)
	if err := s.validateApplicationDependencies(actor.OrganizationID, id, dependencies); err != nil {
		return model.Application{}, err
	}
	updated, err := s.store.UpdateApplication(actor.OrganizationID, id, func(application *model.Application) error {
		application.Name = strings.TrimSpace(input.Name)
		application.Owner = strings.TrimSpace(input.Owner)
		application.Kind = defaultString(input.Kind, application.Kind)
		application.Runtime = defaultString(input.Runtime, application.Runtime)
		application.RepositoryURL = strings.TrimSpace(input.RepositoryURL)
		application.Tier = defaultString(input.Tier, application.Tier)
		application.Lifecycle = defaultString(input.Lifecycle, application.Lifecycle)
		application.Database = strings.TrimSpace(input.Database)
		application.Schema = strings.TrimSpace(input.Schema)
		application.Environment = strings.TrimSpace(input.Environment)
		application.Dependencies = dependencies
		application.Tags = normalizeStringList(input.Tags)
		application.Description = strings.TrimSpace(input.Description)
		if application.Environment == "" {
			application.Environment = "生产 / 只读"
		}
		return nil
	}, audit(actor, "", "UPDATE_APPLICATION", "更新纳管应用 "+strings.TrimSpace(input.Name)))
	if errors.Is(err, store.ErrConflict) {
		return model.Application{}, fmt.Errorf("%w：应用名称或数据库 Schema 已存在", ErrValidation)
	}
	return updated, err
}

func (s *Service) validateApplicationDependencies(organizationID, applicationID string, dependencies []string) error {
	applications := s.store.ApplicationsByOrganization(organizationID)
	applicationByID := make(map[string]model.Application, len(applications))
	for _, application := range applications {
		applicationByID[application.ID] = application
	}
	for _, dependencyID := range dependencies {
		if dependencyID == applicationID && applicationID != "" {
			return fmt.Errorf("%w：服务不能依赖自身", ErrValidation)
		}
		if _, ok := applicationByID[dependencyID]; !ok {
			return fmt.Errorf("%w：依赖服务 %s 不存在或不属于当前企业", ErrValidation, dependencyID)
		}
	}
	if applicationID == "" {
		return nil
	}

	graph := make(map[string][]string, len(applications))
	for _, application := range applications {
		graph[application.ID] = append([]string(nil), application.Dependencies...)
	}
	graph[applicationID] = append([]string(nil), dependencies...)
	state := make(map[string]uint8, len(graph))
	var hasCycle func(string) bool
	hasCycle = func(node string) bool {
		if state[node] == 1 {
			return true
		}
		if state[node] == 2 {
			return false
		}
		state[node] = 1
		for _, dependencyID := range graph[node] {
			if hasCycle(dependencyID) {
				return true
			}
		}
		state[node] = 2
		return false
	}
	if hasCycle(applicationID) {
		return fmt.Errorf("%w：依赖关系形成循环，请调整上下游服务", ErrValidation)
	}
	return nil
}

func (s *Service) canUseApplication(actor model.User, applicationID, capability string) bool {
	if actor.EnterpriseAdmin || actor.Role == model.RoleOwner {
		return true
	}
	if !s.store.HasApplicationGrants(actor.OrganizationID) {
		return true
	}
	for _, grant := range s.store.ApplicationGrantsByUser(actor.OrganizationID, actor.ID) {
		if grant.ApplicationID != applicationID {
			continue
		}
		switch capability {
		case "submit":
			return grant.CanSubmit
		case "review":
			return grant.CanReview
		default:
			return grant.CanSubmit || grant.CanReview
		}
	}
	return false
}
