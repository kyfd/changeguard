package store

import (
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

// 企业自配模型接入的持久化。
//
// 隔离口径与仓库其他按组织的数据一致：所有读写都带 organizationID，
// 不在调用方做过滤。API Key 以密文存储，本包不接触明文——
// 加解密由 internal/modelsecret 在上层完成，避免密钥材料散落到存储层。

// ModelConfigs 返回所有企业的模型配置（仅供内部解析器使用）。
// 注意：返回的记录含密文，不得直接序列化给客户端。
func (s *Store) ModelConfigs() []model.OrganizationModelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.OrganizationModelConfig(nil), s.data.ModelConfigs...)
}

// ModelConfig 返回指定企业的模型配置。未配置时返回零值与 false。
func (s *Store) ModelConfig(organizationID string) (model.OrganizationModelConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.ModelConfigs {
		if item.OrganizationID == organizationID {
			return item, true
		}
	}
	return model.OrganizationModelConfig{}, false
}

// UpsertModelConfig 保存（或清除）企业模型配置。
//
// update 收到的候选记录是当前值的副本；返回 error 表示拒绝本次修改。
// 与 UpdateOrganization 相同：先改副本、校验通过才落盘，失败不影响现值。
func (s *Store) UpsertModelConfig(organizationID, actorID string, update func(*model.OrganizationModelConfig) error, audit model.AuditEvent) (model.OrganizationModelConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	var candidate model.OrganizationModelConfig
	found := false
	for _, item := range s.data.ModelConfigs {
		if item.OrganizationID == organizationID {
			candidate = item
			found = true
			break
		}
	}
	if !found {
		candidate = model.OrganizationModelConfig{
			OrganizationID: organizationID,
			ConfiguredBy:   actorID,
			ConfiguredAt:   now,
		}
	}
	if err := update(&candidate); err != nil {
		return model.OrganizationModelConfig{}, err
	}
	candidate.OrganizationID = organizationID
	candidate.UpdatedAt = now

	if found {
		for index := range s.data.ModelConfigs {
			if s.data.ModelConfigs[index].OrganizationID == organizationID {
				s.data.ModelConfigs[index] = candidate
				break
			}
		}
	} else {
		s.data.ModelConfigs = append(s.data.ModelConfigs, candidate)
	}
	s.appendAuditsLocked(audit)
	if err := s.saveLocked(); err != nil {
		return model.OrganizationModelConfig{}, err
	}
	return candidate, nil
}

// ModelConfigUpdatedAt 供审计与界面展示"最后一次改动时间"。
func (s *Store) ModelConfigUpdatedAt(organizationID string) (time.Time, bool) {
	config, ok := s.ModelConfig(organizationID)
	if !ok {
		return time.Time{}, false
	}
	return config.UpdatedAt, true
}
