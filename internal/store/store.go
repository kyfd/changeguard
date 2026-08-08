package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
)

var ErrNotFound = errors.New("record not found")

var demoCredentials = []model.UserCredential{
	{UserID: "usr_developer", PasswordSalt: "d4lYLYzhsIpp7yU2QEGj7A", PasswordHash: "QcJXWlfFLK5A8e9CulLKK41hanW9jc04HpdVYbQjDi8"},
	{UserID: "usr_reviewer", PasswordSalt: "TzG6EUXXYXo4SnmbSXnLLA", PasswordHash: "dYGfG0WKpKHuOEtXx2SQxgJISoChzSNnzoPnQnWtSuY"},
	{UserID: "usr_owner", PasswordSalt: "6eW2Zv30Rzdy4Gr8YQjIvQ", PasswordHash: "R41vNW0JjXKASoBKX1bhlOX8jkCVX9dC799lG1AlkWc"},
}

type state struct {
	Organizations     []model.Organization       `json:"organizations"`
	Invites           []model.OrganizationInvite `json:"invites"`
	Credentials       []model.UserCredential     `json:"credentials"`
	Applications      []model.Application        `json:"applications"`
	Users             []model.User               `json:"users"`
	Changes           []model.ChangeRequest      `json:"changes"`
	Audits            []model.AuditEvent         `json:"audits"`
	Policies          []model.RiskPolicy         `json:"policies"`
	ApplicationGrants []model.ApplicationGrant   `json:"application_grants"`
	Outbox            []model.OutboxEvent        `json:"outbox"`
	Passports         []model.StoredPassport     `json:"passports"`
	IntegrationEvents []model.IntegrationEvent   `json:"integration_events"`
	OutcomeSignals    []model.OutcomeSignal      `json:"outcome_signals"`
}

type Store struct {
	mu        sync.RWMutex
	path      string
	data      state
	backend   stateBackend
	version   int64
	persisted []byte
}

func New(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = initialState()
		normalizeState(&s.data)
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &s.data); err != nil {
		return nil, err
	}
	s.persisted = append([]byte(nil), content...)
	normalizeState(&s.data)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func NewMemory() *Store {
	data := seedState()
	normalizeState(&data)
	return &Store{data: data}
}

func normalizeState(data *state) {
	now := time.Now()
	if len(data.Organizations) == 0 {
		if demoDataEnabled() {
			*data = seedState()
		} else {
			return
		}
	}
	defaultOrganization := data.Organizations[0]
	for index := range data.Applications {
		application := &data.Applications[index]
		if application.OrganizationID == "" {
			application.OrganizationID = defaultOrganization.ID
		}
		if strings.TrimSpace(application.Kind) == "" {
			application.Kind = "后端服务"
		}
		if strings.TrimSpace(application.Runtime) == "" {
			application.Runtime = "Go"
		}
		if strings.TrimSpace(application.Tier) == "" {
			application.Tier = "重要"
		}
		if strings.TrimSpace(application.Lifecycle) == "" {
			application.Lifecycle = "生产运行"
		}
		if application.OrganizationID == "org_demo" && strings.TrimSpace(application.RepositoryURL) == "" {
			switch application.ID {
			case "app_order":
				application.RepositoryURL, application.Tier, application.Dependencies, application.Tags = "https://git.example.com/commerce/order-service", "核心", []string{"app_inventory", "app_payment", "app_member"}, []string{"交易链路", "高并发", "SLA-99.95"}
			case "app_inventory":
				application.RepositoryURL, application.Tier, application.Dependencies, application.Tags = "https://git.example.com/commerce/inventory-service", "核心", []string{"app_member"}, []string{"库存", "Redis", "最终一致性"}
			case "app_payment":
				application.RepositoryURL, application.Tier, application.Dependencies, application.Tags = "https://git.example.com/finance/payment-service", "核心", []string{"app_member"}, []string{"支付", "对账", "强审计"}
			case "app_member":
				application.RepositoryURL, application.Kind, application.Tags = "https://git.example.com/platform/member-service", "平台服务", []string{"企业账号", "SSO", "RBAC"}
			}
		}
	}
	for index := range data.Users {
		user := &data.Users[index]
		legacyUser := user.OrganizationID == ""
		if legacyUser {
			user.OrganizationID = defaultOrganization.ID
		}
		if user.OrganizationName == "" {
			user.OrganizationName = defaultOrganization.Name
		}
		if legacyUser {
			user.Active = true
		}
		if user.ID == "usr_owner" {
			user.EnterpriseAdmin = true
		}
	}
	mergeDemoCredentials(data)
	if demoDataEnabled() {
		ensureDemoCoverage(data, defaultOrganization, now)
	}
	for index := range data.Changes {
		change := &data.Changes[index]
		if change.OrganizationID == "" {
			change.OrganizationID = defaultOrganization.ID
		}
		if strings.TrimSpace(change.RollbackPlan) == "" && strings.TrimSpace(change.RollbackSQL) != "" {
			change.RollbackPlan = "数据库异常时执行已登记的回滚 SQL，并核对影响行数与核心业务指标。"
		}
		if strings.TrimSpace(change.ReleasePlan.Strategy) == "" {
			change.ReleasePlan.Strategy = "全量发布"
		}
		if change.ReleasePlan.ObservationMinutes <= 0 {
			change.ReleasePlan.ObservationMinutes = 15
		}
		if len(change.ReleasePlan.SuccessMetrics) == 0 {
			change.ReleasePlan.SuccessMetrics = []string{"错误率", "P99 延迟", "核心业务成功率"}
		}
		if len(change.Artifacts) == 0 && strings.TrimSpace(change.SQL) != "" {
			change.Artifacts = []model.ChangeArtifact{{ID: NewID("artifact_"), Kind: model.ArtifactDatabase, Name: "数据库 SQL", Source: "历史变更迁移", Language: "SQL", Content: change.SQL}}
		}
		if change.SQLSHA256 == "" {
			change.SQLSHA256 = changegate.SHA256(change.SQL)
		}
		if change.RollbackSHA256 == "" {
			change.RollbackSHA256 = changegate.SHA256(change.RollbackSQL)
		}
		for artifactIndex := range change.Artifacts {
			artifact := changegate.PrepareStoredArtifact(change.Artifacts[artifactIndex])
			change.Artifacts[artifactIndex] = artifact
		}
		if change.ArtifactSHA256 == "" {
			change.ArtifactSHA256 = changegate.ChangeDigest(change.Environment, change.ChangeType, change.Artifacts, change.SQLSHA256, change.RollbackSHA256, change.RollbackPlan)
		}
		change.SQL = changegate.Redact(change.SQL)
		change.RollbackSQL = changegate.Redact(change.RollbackSQL)
		change.RollbackPlan = changegate.Redact(change.RollbackPlan)
		change.Description = changegate.Redact(change.Description)
		if demoDataEnabled() && change.OrganizationID == "org_demo" && len(change.Artifacts) <= 1 {
			switch change.ID {
			case "chg_20260730_001":
				change.ChangeType, change.RepositoryURL, change.Branch, change.CommitSHA = "联合变更", "https://git.example.com/commerce/order-service", "feature/request-id", "8f2c1ab"
				change.Artifacts = append([]model.ChangeArtifact{{ID: migratedArtifactID(change.ID, "order-config"), Kind: model.ArtifactConfig, Name: "订单幂等开关配置", Source: "config/order.yaml", Language: "YAML", Content: "idempotency:\n  enabled: true\n  key_source: request_id"}, {ID: migratedArtifactID(change.ID, "order-kubernetes"), Kind: model.ArtifactKubernetes, Name: "订单服务 Deployment", Source: "deploy/order.yaml", Language: "YAML", Content: "image: registry.example.com/order:v2.8.0\nresources:\n  requests:\n    cpu: 500m\n    memory: 512Mi\n  limits:\n    cpu: 2\n    memory: 1Gi\nreadinessProbe:\n  httpGet:\n    path: /health/ready"}}, change.Artifacts...)
				change.RollbackPlan = "将订单服务镜像回退到 v2.7.4，关闭 request_id 写入开关；数据库异常时删除新索引。"
				change.ReleasePlan = model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 10, ObservationMinutes: 20, AutoRollback: true, SuccessMetrics: []string{"下单成功率", "HTTP 5xx", "P99 延迟"}}
			case "chg_20260730_002":
				change.ChangeType, change.RepositoryURL, change.Branch, change.CommitSHA = "Kubernetes 发布", "https://git.example.com/commerce/inventory-service", "release/v3.2", "4b9d0e1"
				change.Artifacts = []model.ChangeArtifact{{ID: migratedArtifactID(change.ID, "inventory-kubernetes"), Kind: model.ArtifactKubernetes, Name: "库存服务 Deployment", Source: "deploy/inventory.yaml", Language: "YAML", Content: "image: registry.example.com/inventory:v3.2.0\nreplicas: 6\nresources:\n  requests:\n    cpu: 400m\n    memory: 512Mi\n  limits:\n    cpu: 1500m\n    memory: 1Gi"}}
				change.SQL, change.RollbackSQL = "", ""
				change.RollbackPlan = "恢复 inventory:v3.1.6 镜像并将副本数恢复为 4，保留旧 ReplicaSet 30 分钟。"
				change.ReleasePlan = model.ReleasePlan{Strategy: "金丝雀发布", CanaryPercent: 20, ObservationMinutes: 15, AutoRollback: true, SuccessMetrics: []string{"库存预占成功率", "HTTP 5xx", "Redis 超时率"}}
			case "chg_20260729_003":
				change.ChangeType, change.RepositoryURL, change.Branch, change.CommitSHA = "数据库与配置联合变更", "https://git.example.com/finance/payment-service", "release/v5.1", "a61c90d"
				change.Artifacts = append([]model.ChangeArtifact{{ID: migratedArtifactID(change.ID, "refund-config"), Kind: model.ArtifactConfig, Name: "退款流量入口配置", Source: "config/refund.yaml", Language: "YAML", Content: "refund_v2:\n  enabled: true\n  rollout_percent: 10"}}, change.Artifacts...)
				change.RollbackPlan = "关闭退款 V2 流量入口，恢复旧字段读取逻辑；数据库按已登记 SQL 回退。"
				change.ReleasePlan = model.ReleasePlan{Strategy: "蓝绿发布", ObservationMinutes: 30, AutoRollback: true, SuccessMetrics: []string{"支付成功率", "退款成功率", "P99 延迟"}}
			case "chg_20260728_004":
				change.ChangeType, change.RepositoryURL, change.Branch, change.CommitSHA = "配置变更", "https://git.example.com/platform/member-service", "main", "1d8e77f"
				change.Artifacts = []model.ChangeArtifact{{ID: migratedArtifactID(change.ID, "archive-config"), Kind: model.ArtifactConfig, Name: "归档任务配置", Source: "config/archive.yaml", Language: "YAML", Content: "archive:\n  batch_size: 500\n  retention_days: 180\n  enabled: true"}}
				change.SQL, change.RollbackSQL = "", ""
				change.RollbackPlan = "关闭 archive.enabled 并恢复上一版本配置，已归档数据通过归档批次号恢复。"
				change.ReleasePlan = model.ReleasePlan{Strategy: "分批发布", CanaryPercent: 25, ObservationMinutes: 15, AutoRollback: false, SuccessMetrics: []string{"归档任务失败率", "数据库负载", "任务耗时"}}
			}
			for artifactIndex := range change.Artifacts {
				change.Artifacts[artifactIndex] = changegate.PrepareStoredArtifact(change.Artifacts[artifactIndex])
			}
			change.ArtifactSHA256 = changegate.ChangeDigest(change.Environment, change.ChangeType, change.Artifacts, change.SQLSHA256, change.RollbackSHA256, change.RollbackPlan)
		}
		if change.Experiment != nil {
			experiment := change.Experiment
			if strings.EqualFold(experiment.Mode, "SIMULATED") || strings.EqualFold(experiment.Mode, "DEMO") || strings.EqualFold(experiment.Mode, "DEMO_ONLY") {
				experiment.Mode = "DEMO_ONLY"
				experiment.Status = "NOT_RUN"
				experiment.ChecksPassed = 0
				experiment.RollbackVerified = false
				experiment.DatasetRows = 0
				experiment.LockWaitMS = 0
				experiment.P99BeforeMS = 0
				experiment.P99AfterMS = 0
				experiment.ExecutionError = "历史模拟证据已隔离为 DEMO_ONLY/NOT_RUN，不能用于生产放行"
			}
			if strings.TrimSpace(experiment.Kind) == "" {
				experiment.Kind = "多制品预发布验证"
			}
			if experiment.ChecksTotal <= 0 {
				experiment.ChecksTotal = len(experiment.Evidence)
				if experiment.ChecksTotal == 0 {
					experiment.ChecksTotal = 1
				}
			}
			if strings.EqualFold(experiment.Status, "PASSED") && strings.EqualFold(experiment.Mode, "POSTGRES") && experiment.ChecksPassed <= 0 {
				experiment.ChecksPassed = experiment.ChecksTotal
			}
			if strings.TrimSpace(experiment.Strategy) == "" {
				experiment.Strategy = change.ReleasePlan.Strategy
			}
			if experiment.CanaryPercent <= 0 {
				experiment.CanaryPercent = change.ReleasePlan.CanaryPercent
			}
			if experiment.ObservationMinutes <= 0 {
				experiment.ObservationMinutes = change.ReleasePlan.ObservationMinutes
			}
		}
	}
	for index := range data.IntegrationEvents {
		event := &data.IntegrationEvents[index]
		if event.OrganizationID == "" {
			event.OrganizationID = defaultOrganization.ID
		}
		if event.OccurredAt.IsZero() {
			event.OccurredAt = event.ReceivedAt
		}
	}
	for index := range data.OutcomeSignals {
		signal := &data.OutcomeSignals[index]
		if signal.OrganizationID == "" {
			signal.OrganizationID = defaultOrganization.ID
		}
		if signal.ReceivedAt.IsZero() {
			signal.ReceivedAt = signal.OccurredAt
		}
	}
	for index := range data.Audits {
		if data.Audits[index].OrganizationID == "" {
			data.Audits[index].OrganizationID = defaultOrganization.ID
		}
	}
	filteredPolicies := data.Policies[:0]
	for _, policy := range data.Policies {
		if policy.Builtin && (policy.Code == "CODE_PANIC_PATH" || policy.Code == "API_BREAKING_CHANGE") {
			continue
		}
		filteredPolicies = append(filteredPolicies, policy)
	}
	data.Policies = filteredPolicies
	defaults := model.DefaultRiskPolicies(now)
	defaultByCode := make(map[string]model.RiskPolicy, len(defaults))
	existingPolicies := make(map[string]bool, len(data.Policies))
	for _, policy := range defaults {
		defaultByCode[policy.Code] = policy
	}
	for index := range data.Policies {
		policy := &data.Policies[index]
		if policy.OrganizationID == "" {
			policy.OrganizationID = defaultOrganization.ID
		}
		existingPolicies[policy.OrganizationID+"|"+policy.Code] = true
		if policy.Version <= 0 {
			policy.Version = 1
		}
		if policy.CreatedAt.IsZero() {
			policy.CreatedAt = now
		}
		if policy.UpdatedAt.IsZero() {
			policy.UpdatedAt = policy.CreatedAt
		}
		if strings.TrimSpace(policy.UpdatedBy) == "" {
			policy.UpdatedBy = "系统迁移"
		}
	}
	for _, organization := range data.Organizations {
		for _, template := range defaults {
			key := organization.ID + "|" + template.Code
			if existingPolicies[key] {
				continue
			}
			template.ID = migratedPolicyID(organization.ID, template.Code)
			template.OrganizationID = organization.ID
			data.Policies = append(data.Policies, template)
		}
	}
	for changeIndex := range data.Changes {
		change := &data.Changes[changeIndex]
		filteredFindings := change.Findings[:0]
		for _, finding := range change.Findings {
			if finding.Code == "RELEASE_BASELINE_PASS" || finding.Code == "BASELINE_PASS" {
				continue
			}
			finding.Detail = changegate.Redact(finding.Detail)
			finding.Evidence = changegate.Redact(finding.Evidence)
			finding.Suggestion = changegate.Redact(finding.Suggestion)
			filteredFindings = append(filteredFindings, finding)
		}
		change.Findings = filteredFindings
		for findingIndex := range change.Findings {
			finding := &change.Findings[findingIndex]
			if finding.Status == "" {
				finding.Status = model.FindingOpen
			}
			if strings.HasSuffix(finding.Code, "PASS") {
				finding.Status = model.FindingVerified
			}
			finding.Detail = changegate.Redact(finding.Detail)
			finding.Evidence = changegate.Redact(finding.Evidence)
			finding.Suggestion = changegate.Redact(finding.Suggestion)
			if finding.UpdatedAt.IsZero() {
				finding.UpdatedAt = change.UpdatedAt
			}
			if policy, ok := defaultByCode[finding.Code]; ok {
				if finding.RuleVersion <= 0 {
					finding.RuleVersion = policy.Version
				}
				if policy.Blocking {
					finding.Blocking = true
				}
			}
		}
	}
}

func migratedArtifactID(changeID, role string) string {
	return "artifact_migrated_" + changegate.SHA256(strings.TrimSpace(changeID) + "|" + strings.TrimSpace(role))[:24]
}

func migratedPolicyID(organizationID, code string) string {
	return "pol_migrated_" + changegate.SHA256(strings.TrimSpace(organizationID) + "|" + strings.TrimSpace(code))[:24]
}

func demoAccountsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_ENABLE_DEMO_ACCOUNTS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func mergeDemoCredentials(data *state) {
	if !demoAccountsEnabled() {
		return
	}
	users := make(map[string]bool, len(data.Users))
	for _, user := range data.Users {
		users[user.ID] = true
	}
	for _, candidate := range demoCredentials {
		if !users[candidate.UserID] {
			continue
		}
		found := false
		for index := range data.Credentials {
			if data.Credentials[index].UserID != candidate.UserID {
				continue
			}
			found = true
			// Demo credentials are intentionally fixed while the explicit demo
			// flag is enabled, so persisted local data follows password updates.
			data.Credentials[index].PasswordSalt = candidate.PasswordSalt
			data.Credentials[index].PasswordHash = candidate.PasswordHash
			break
		}
		if !found {
			data.Credentials = append(data.Credentials, candidate)
		}
	}
}

func NewID(prefix string) string {
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return prefix + time.Now().Format("20060102150405")
	}
	return prefix + hex.EncodeToString(buf)
}

func (s *Store) Policies() []model.RiskPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := append([]model.RiskPolicy(nil), s.data.Policies...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].Builtin != items[j].Builtin {
			return items[i].Builtin
		}
		if items[i].Enabled != items[j].Enabled {
			return items[i].Enabled
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items
}

func (s *Store) Policy(id string) (model.RiskPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Policies {
		if item.ID == id {
			return item, nil
		}
	}
	return model.RiskPolicy{}, ErrNotFound
}

func (s *Store) PolicyByCode(code string) (model.RiskPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Policies {
		if item.Code == code {
			return item, nil
		}
	}
	return model.RiskPolicy{}, ErrNotFound
}

func (s *Store) CreatePolicy(policy model.RiskPolicy, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.data.Policies {
		if item.OrganizationID == policy.OrganizationID && item.Code == policy.Code {
			return errors.New("policy code already exists")
		}
	}
	s.data.Policies = append(s.data.Policies, policy)
	s.data.Audits = append(s.data.Audits, audit)
	return s.saveLocked()
}

func (s *Store) UpdatePolicy(id string, update func(*model.RiskPolicy) error, audits ...model.AuditEvent) (model.RiskPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.data.Policies {
		if s.data.Policies[index].ID != id {
			continue
		}
		if err := update(&s.data.Policies[index]); err != nil {
			return model.RiskPolicy{}, err
		}
		s.data.Policies[index].Version++
		s.data.Policies[index].UpdatedAt = time.Now()
		s.data.Audits = append(s.data.Audits, audits...)
		if err := s.saveLocked(); err != nil {
			return model.RiskPolicy{}, err
		}
		return s.data.Policies[index], nil
	}
	return model.RiskPolicy{}, ErrNotFound
}

func (s *Store) RecordPolicyHits(codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unique := make(map[string]bool, len(codes))
	for _, code := range codes {
		unique[code] = true
	}
	now := time.Now()
	for index := range s.data.Policies {
		if unique[s.data.Policies[index].Code] {
			s.data.Policies[index].HitCount++
			s.data.Policies[index].LastHitAt = &now
		}
	}
	return s.saveLocked()
}

func (s *Store) Applications() []model.Application {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.Application(nil), s.data.Applications...)
}

func (s *Store) Users() []model.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]model.User(nil), s.data.Users...)
}

func (s *Store) User(id string) (model.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Users {
		if item.ID == id {
			return item, nil
		}
	}
	return model.User{}, ErrNotFound
}

func (s *Store) Application(id string) (model.Application, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Applications {
		if item.ID == id {
			return item, nil
		}
	}
	return model.Application{}, ErrNotFound
}

func (s *Store) Changes() []model.ChangeRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := append([]model.ChangeRequest(nil), s.data.Changes...)
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

func (s *Store) Change(id string) (model.ChangeRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.data.Changes {
		if item.ID == id {
			return item, nil
		}
	}
	return model.ChangeRequest{}, ErrNotFound
}

func (s *Store) CreateChange(change model.ChangeRequest, audit model.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Changes = append(s.data.Changes, change)
	s.data.Audits = append(s.data.Audits, audit)
	return s.saveLocked()
}

func (s *Store) UpdateChange(id string, update func(*model.ChangeRequest) error, audits ...model.AuditEvent) (model.ChangeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Changes {
		if s.data.Changes[i].ID != id {
			continue
		}
		if err := update(&s.data.Changes[i]); err != nil {
			return model.ChangeRequest{}, err
		}
		s.data.Changes[i].UpdatedAt = time.Now()
		s.data.Changes[i].Version++
		s.data.Audits = append(s.data.Audits, audits...)
		if err := s.saveLocked(); err != nil {
			return model.ChangeRequest{}, err
		}
		return s.data.Changes[i], nil
	}
	return model.ChangeRequest{}, ErrNotFound
}

func (s *Store) Audits(limit int) []model.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := append([]model.AuditEvent(nil), s.data.Audits...)
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) RecordIntegrationEvent(event model.IntegrationEvent, audit model.AuditEvent) (model.IntegrationEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.IntegrationEvents {
		if existing.OrganizationID == event.OrganizationID && existing.Provider == event.Provider && existing.ExternalID == event.ExternalID {
			return existing, false, nil
		}
	}
	s.data.IntegrationEvents = append(s.data.IntegrationEvents, event)
	s.data.Audits = append(s.data.Audits, audit)
	if err := s.saveLocked(); err != nil {
		return model.IntegrationEvent{}, false, err
	}
	return event, true, nil
}

func (s *Store) RecordOutcomeSignal(signal model.OutcomeSignal, audit model.AuditEvent) (model.OutcomeSignal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.OutcomeSignals {
		if existing.OrganizationID == signal.OrganizationID && existing.Source == signal.Source && existing.ExternalID == signal.ExternalID {
			return existing, false, nil
		}
	}
	s.data.OutcomeSignals = append(s.data.OutcomeSignals, signal)
	s.data.Audits = append(s.data.Audits, audit)
	if err := s.saveLocked(); err != nil {
		return model.OutcomeSignal{}, false, err
	}
	return signal, true, nil
}

func (s *Store) OutcomeSignals(organizationID string, limit int) []model.OutcomeSignal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.OutcomeSignal, 0, len(s.data.OutcomeSignals))
	for _, signal := range s.data.OutcomeSignals {
		if organizationID == "" || signal.OrganizationID == organizationID {
			items = append(items, signal)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].OccurredAt.Equal(items[j].OccurredAt) {
			return items[i].ReceivedAt.After(items[j].ReceivedAt)
		}
		return items[i].OccurredAt.After(items[j].OccurredAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) IntegrationEvents(organizationID string, limit int) []model.IntegrationEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]model.IntegrationEvent, 0, len(s.data.IntegrationEvents))
	for _, event := range s.data.IntegrationEvents {
		if organizationID == "" || event.OrganizationID == organizationID {
			items = append(items, event)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i].OccurredAt, items[j].OccurredAt
		if left.IsZero() {
			left = items[i].ReceivedAt
		}
		if right.IsZero() {
			right = items[j].ReceivedAt
		}
		if left.Equal(right) {
			return items[i].ReceivedAt.After(items[j].ReceivedAt)
		}
		return left.After(right)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) Dashboard() model.Dashboard {
	changes := s.Changes()
	dashboard := model.Dashboard{
		RiskDistribution: map[model.RiskLevel]int{
			model.RiskLow: 0, model.RiskMedium: 0, model.RiskHigh: 0, model.RiskUnknown: 0,
		},
	}
	var experimentCount, experimentPass int
	var durationTotal int64
	for _, item := range changes {
		dashboard.RiskDistribution[item.Risk]++
		if item.Risk == model.RiskHigh {
			dashboard.HighRiskCount++
		}
		switch item.Status {
		case model.StatusDraft, model.StatusChecking, model.StatusReadyForExperiment,
			model.StatusExperimentQueued, model.StatusExperimentRunning, model.StatusWaitingApproval:
			dashboard.PendingCount++
		}
		if item.Status == model.StatusWaitingApproval {
			dashboard.PendingApprovals = append(dashboard.PendingApprovals, item)
		}
		if item.Experiment != nil {
			experimentCount++
			durationTotal += item.Experiment.DurationMS
			if item.Experiment.Status == "PASSED" {
				experimentPass++
			}
		}
	}
	if len(changes) > 6 {
		dashboard.RecentChanges = changes[:6]
	} else {
		dashboard.RecentChanges = changes
	}
	if experimentCount > 0 {
		dashboard.ExperimentPassRate = float64(experimentPass) / float64(experimentCount) * 100
		dashboard.AverageExperimentSec = float64(durationTotal) / float64(experimentCount) / 1000
	}
	return dashboard
}

func (s *Store) saveLocked() error {
	if s.backend != nil {
		content, err := json.Marshal(s.data)
		if err != nil {
			s.restoreLocked()
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		version, err := s.backend.Save(ctx, content, s.version)
		cancel()
		if err != nil {
			s.restoreLocked()
			return err
		}
		s.version = version
		s.persisted = append(s.persisted[:0], content...)
		return nil
	}
	if s.path == "" {
		return nil
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		s.restoreLocked()
		return err
	}
	tempPath := s.path + ".tmp"
	if err := os.WriteFile(tempPath, content, 0o600); err != nil {
		s.restoreLocked()
		return err
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		_ = os.Remove(tempPath)
		s.restoreLocked()
		return err
	}
	s.persisted = append(s.persisted[:0], content...)
	return nil
}

func (s *Store) restoreLocked() {
	var content []byte
	var version int64
	loadedLatest := false
	if s.backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		latest, latestVersion, err := s.backend.Load(ctx)
		cancel()
		if err == nil && len(latest) > 0 {
			content, version, loadedLatest = latest, latestVersion, true
		}
	} else if s.path != "" {
		if latest, err := os.ReadFile(s.path); err == nil && len(latest) > 0 {
			content, loadedLatest = latest, true
		}
	}
	if len(content) == 0 {
		content = s.persisted
	}
	if len(content) == 0 {
		return
	}
	var recovered state
	if json.Unmarshal(content, &recovered) != nil {
		return
	}
	normalizeState(&recovered)
	s.data = recovered
	if loadedLatest {
		s.persisted = append(s.persisted[:0], content...)
		if s.backend != nil {
			s.version = version
		}
	}
}
func demoDataEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_ENABLE_DEMO_DATA"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		// Enabling the fixed local demo accounts also implies their sample
		// organization, applications and users must exist.
		return demoAccountsEnabled()
	}
}

func initialState() state {
	if demoDataEnabled() {
		return seedState()
	}
	return state{}
}

func seedState() state {
	now := time.Now()
	apps := []model.Application{
		{ID: "app_order", Name: "订单服务", Owner: "刘丰熙", Kind: "后端服务", Runtime: "Go 1.26", RepositoryURL: "https://git.example.com/commerce/order-service", Tier: "核心", Lifecycle: "生产运行", Database: "commerce", Schema: "public", Environment: "生产 / Kubernetes", Dependencies: []string{"app_inventory", "app_payment", "app_member"}, Tags: []string{"交易链路", "高并发", "SLA-99.95"}, Description: "订单创建、状态流转与幂等控制"},
		{ID: "app_inventory", Name: "库存服务", Owner: "陈嘉", Kind: "后端服务", Runtime: "Go 1.26", RepositoryURL: "https://git.example.com/commerce/inventory-service", Tier: "核心", Lifecycle: "生产运行", Database: "inventory", Schema: "public", Environment: "生产 / Kubernetes", Dependencies: []string{"app_member"}, Tags: []string{"库存", "Redis", "最终一致性"}, Description: "库存预占、确认与释放"},
		{ID: "app_payment", Name: "支付结算", Owner: "周宁", Kind: "后端服务", Runtime: "Go 1.26", RepositoryURL: "https://git.example.com/finance/payment-service", Tier: "核心", Lifecycle: "生产运行", Database: "payment", Schema: "settlement", Environment: "生产 / Kubernetes", Dependencies: []string{"app_member"}, Tags: []string{"支付", "对账", "强审计"}, Description: "支付流水、退款和对账"},
		{ID: "app_member", Name: "会员中心", Owner: "赵可", Kind: "平台服务", Runtime: "Go 1.26", RepositoryURL: "https://git.example.com/platform/member-service", Tier: "重要", Lifecycle: "生产运行", Database: "member", Schema: "public", Environment: "生产 / Kubernetes", Tags: []string{"企业账号", "SSO", "RBAC"}, Description: "用户、组织与权限关系"},
	}
	users := []model.User{
		{ID: "usr_developer", Name: "刘丰熙", Role: "后端开发"},
		{ID: "usr_reviewer", Name: "周宁", Role: "数据库审核人"},
		{ID: "usr_owner", Name: "陈嘉", Role: "技术负责人"},
	}
	changes := []model.ChangeRequest{
		seedChange("chg_20260730_001", "订单表增加请求幂等字段", apps[0], users[0], model.StatusWaitingApproval, model.RiskMedium, now.Add(-42*time.Minute)),
		seedChange("chg_20260730_002", "库存流水表补充业务索引", apps[1], users[2], model.StatusReadyForExperiment, model.RiskLow, now.Add(-2*time.Hour)),
		seedChange("chg_20260729_003", "支付流水金额字段类型调整", apps[2], users[1], model.StatusExperimentRunning, model.RiskHigh, now.Add(-20*time.Hour)),
		seedChange("chg_20260728_004", "会员邀请记录归档", apps[3], users[0], model.StatusApproved, model.RiskLow, now.Add(-38*time.Hour)),
	}
	changes[0].PlannedAt = now.Add(4 * time.Hour)
	changes[1].PlannedAt = now.Add(6 * time.Hour)
	changes[2].PlannedAt = now.Add(8 * time.Hour)
	changes[3].PlannedAt = now.Add(10 * time.Hour)
	changes[0].SQL = "CREATE UNIQUE INDEX ux_orders_request_id ON orders(request_id);"
	changes[0].RollbackSQL = "DROP INDEX IF EXISTS ux_orders_request_id;"
	changes[0].Findings = []model.Finding{{ID: "ev_rule_001", Code: "INDEX_NOT_CONCURRENT", Severity: model.RiskMedium, Title: "索引创建未使用 CONCURRENTLY", Detail: "高写入表直接创建索引可能扩大锁等待窗口。", Evidence: "CREATE UNIQUE INDEX", Suggestion: "先清理重复数据，并评估 CREATE UNIQUE INDEX CONCURRENTLY。", Status: model.FindingVerified, OwnerID: users[0].ID, OwnerName: users[0].Name, Resolution: "已完成重复值检查并将执行方式调整为并发创建", VerifiedByID: users[1].ID, VerifiedByName: users[1].Name, VerificationComment: "整改证据已核对", UpdatedAt: now.Add(-36 * time.Minute)}}
	changes[0].Experiment = &model.ExperimentReport{ID: "exp_seed_001", Kind: "DEMO_ONLY", Mode: "DEMO_ONLY", Status: "NOT_RUN", StartedAt: now.Add(-35 * time.Minute), FinishedAt: now.Add(-35 * time.Minute), ExecutionError: "显式演示数据未执行真实 PostgreSQL 影子演练，不能用于生产放行"}
	changes[0].Analysis = &model.AgentAnalysis{Provider: "rules-fallback", Risk: model.RiskMedium, Summary: "演练已通过，但唯一索引上线前需要先处理重复数据并控制索引创建窗口。", Reasons: []string{"高写入表直接创建唯一索引存在锁等待放大风险", "回滚 SQL 已提供且验证通过"}, Suggestions: []string{"上线前执行重复值检查", "优先在低峰期执行并观察锁等待"}, EvidenceIDs: []string{"ev_rule_001", "ev_exp_lock"}, Steps: 2, ToolCalls: 2, GeneratedAt: now.Add(-33 * time.Minute)}
	for i := range changes {
		changes[i].Timeline = []model.TimelineEntry{{ID: NewID("tl_"), Status: model.StatusDraft, Title: "创建变更单", Detail: "登记 SQL、回滚方案和计划时间", Actor: changes[i].SubmitterName, CreatedAt: changes[i].CreatedAt}}
	}
	audits := []model.AuditEvent{
		{ID: NewID("audit_"), ChangeID: changes[0].ID, ActorID: users[0].ID, ActorName: users[0].Name, Action: "RUN_EXPERIMENT", Detail: "影子库演练完成，等待审核", CreatedAt: now.Add(-34 * time.Minute)},
		{ID: NewID("audit_"), ChangeID: changes[1].ID, ActorID: users[2].ID, ActorName: users[2].Name, Action: "SUBMIT", Detail: "规则检查通过，等待演练", CreatedAt: now.Add(-90 * time.Minute)},
	}
	organization := model.Organization{ID: "org_demo", Name: "研发效能示范企业", Slug: "demo", EmailDomains: []string{"example.com"}, ApplicationAccessControlled: true, CreatedBy: users[2].ID, CreatedAt: now, UpdatedAt: now}
	for index := range apps {
		apps[index].OrganizationID = organization.ID
	}
	for index := range users {
		users[index].OrganizationID = organization.ID
		users[index].OrganizationName = organization.Name
		users[index].Active = true
	}
	users[0].Email = "developer@example.com"
	users[1].Email = "reviewer@example.com"
	users[2].Email = "owner@example.com"
	users[2].EnterpriseAdmin = true
	for index := range changes {
		changes[index].OrganizationID = organization.ID
	}
	for index := range audits {
		audits[index].OrganizationID = organization.ID
	}
	policies := model.DefaultRiskPolicies(now)
	for index := range policies {
		policies[index].OrganizationID = organization.ID
	}
	grants := make([]model.ApplicationGrant, 0, len(apps)*len(users))
	for _, user := range users {
		for _, application := range apps {
			grants = append(grants, model.ApplicationGrant{
				OrganizationID: organization.ID, UserID: user.ID, ApplicationID: application.ID,
				CanSubmit: user.Role != model.RoleReviewer, CanReview: user.Role != model.RoleDeveloper,
				UpdatedBy: "system_seed", UpdatedAt: now,
			})
		}
	}
	result := state{Organizations: []model.Organization{organization}, Applications: apps, Users: users, Changes: changes, Audits: audits, Policies: policies, ApplicationGrants: grants}
	ensureDemoCoverage(&result, organization, now)
	mergeDemoCredentials(&result)
	return result
}

func seedChange(id, title string, app model.Application, user model.User, status model.ChangeStatus, risk model.RiskLevel, created time.Time) model.ChangeRequest {
	return model.ChangeRequest{
		OrganizationID: app.OrganizationID, ID: id, Title: title, ApplicationID: app.ID, ApplicationName: app.Name,
		Environment: "生产环境", ChangeType: "DDL", SQL: "ALTER TABLE orders ADD COLUMN note varchar(255);",
		RollbackSQL: "ALTER TABLE orders DROP COLUMN IF EXISTS note;", Description: "按业务需求补充字段，计划低峰期执行。",
		SubmitterID: user.ID, SubmitterName: user.Name, Status: status, Risk: risk,
		PlannedAt: created.Add(24 * time.Hour), CreatedAt: created, UpdatedAt: created, Version: 1,
	}
}
