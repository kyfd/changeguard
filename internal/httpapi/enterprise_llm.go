package httpapi

// 企业自配模型接入的 HTTP 接口。
//
// 边界（与仓库其他写操作一致）：
//
//   - 只允许企业管理员（enterprise_admin 或技术负责人）修改；其他人只读状态。
//   - API Key **永不回显**。响应里只有 Hint() 生成的尾缀提示。
//   - 未配置 DBGUARD_SECRETS_MASTER_KEY 时整体失败关闭，不落明文。
//   - 服务地址经 modelprobe.ValidateBaseURL 过滤，避免把服务端变成内网探针。
//   - 每次保存写一条审计，但不记录 Key 与服务地址以外的内容。

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/modelprobe"
	"github.com/kyfd/changeguard/internal/modelsecret"
)

const (
	maxModelBaseURLLen = 500
	maxModelNameLen    = 200
	maxAPIKeyLen       = 4000
	minModelMaxTokens  = 100
	maxModelMaxTokens  = 8000
)

// modelConfigUnavailable 是主密钥缺失时返回给前端的说明。
// 说清"要配什么、配在哪"，而不是只报一个内部错误码。
const modelConfigUnavailable = "未配置模型密钥主密钥：请在服务环境设置 DBGUARD_SECRETS_MASTER_KEY 后重启，" +
	"否则无法安全保存企业模型 Key。"

// currentEnterprise 返回当前成员与其所属企业；未登录或成员无效时已写出错误响应。
//
// 直接读 Server 上的 store：auth.Manager 的 store 是包私有字段，
// 而 Server 已经从 New 的 collectors 里拿到了同一个 *store.Store。
func (s *Server) currentEnterprise(w http.ResponseWriter, r *http.Request) (model.User, model.Organization, bool) {
	userID := actorID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "尚未登录")
		return model.User{}, model.Organization{}, false
	}
	user, err := s.store.User(userID)
	if err != nil || !user.Active {
		writeError(w, http.StatusUnauthorized, "当前成员不存在或已停用")
		return model.User{}, model.Organization{}, false
	}
	organization, err := s.store.Organization(user.OrganizationID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "企业工作空间不存在")
		return model.User{}, model.Organization{}, false
	}
	return user, organization, true
}

func canEditModelConfig(user model.User) bool {
	return user.EnterpriseAdmin || user.Role == model.RoleOwner
}

// modelConfigView 是返回给前端的视图：只有密文提示，没有密文本身。
type modelConfigView struct {
	Enabled      bool   `json:"enabled"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	MaxTokens    int    `json:"max_tokens"`
	APIKeyHint   string `json:"api_key_hint"`
	HasAPIKey    bool   `json:"has_api_key"`
	Source       string `json:"source"`
	ConfiguredBy string `json:"configured_by,omitempty"`
	ConfiguredAt string `json:"configured_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	LastTestOK   bool   `json:"last_test_ok"`
	LastTestAt   string `json:"last_test_at,omitempty"`
	LastTestErr  string `json:"last_test_error,omitempty"`
	Message      string `json:"message,omitempty"`
}

func toModelConfigView(config model.OrganizationModelConfig, message string) modelConfigView {
	view := modelConfigView{
		Enabled:      config.Enabled,
		Provider:     string(config.Provider),
		BaseURL:      config.BaseURL,
		Model:        config.Model,
		MaxTokens:    config.MaxTokens,
		APIKeyHint:   config.APIKeyHint,
		HasAPIKey:    strings.TrimSpace(config.APIKeyCipher) != "",
		Source:       "none",
		ConfiguredBy: config.ConfiguredBy,
		LastTestOK:   config.LastTestOK,
		LastTestErr:  config.LastTestError,
		Message:      message,
	}
	if config.Enabled && strings.TrimSpace(config.APIKeyCipher) != "" {
		view.Source = "organization"
	}
	if !config.ConfiguredAt.IsZero() {
		view.ConfiguredAt = config.ConfiguredAt.Format(time.RFC3339)
	}
	if !config.UpdatedAt.IsZero() {
		view.UpdatedAt = config.UpdatedAt.Format(time.RFC3339)
	}
	if config.LastTestAt != nil && !config.LastTestAt.IsZero() {
		view.LastTestAt = config.LastTestAt.Format(time.RFC3339)
	}
	if view.Provider == "" {
		view.Provider = string(model.ModelProviderOpenAI)
	}
	if view.MaxTokens <= 0 {
		view.MaxTokens = 700
	}
	return view
}

// handleEnterpriseLLM 处理 /api/enterprise/llm：GET 读状态，PUT 保存。
func (s *Server) handleEnterpriseLLM(w http.ResponseWriter, r *http.Request) {
	user, organization, ok := s.currentEnterprise(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.readModelConfig(w, organization.ID)
	case http.MethodPut:
		if !canEditModelConfig(user) {
			writeError(w, http.StatusForbidden, "只有企业管理员可以配置模型接入")
			return
		}
		s.saveModelConfig(w, r, organization.ID, user)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) readModelConfig(w http.ResponseWriter, organizationID string) {
	if s.secrets == nil {
		writeJSON(w, http.StatusOK, toModelConfigView(model.OrganizationModelConfig{}, modelConfigUnavailable))
		return
	}
	config, found := s.store.ModelConfig(organizationID)
	if !found {
		writeJSON(w, http.StatusOK, toModelConfigView(model.OrganizationModelConfig{}, "尚未接入企业模型"))
		return
	}
	message := "已接入企业模型"
	if !config.Enabled {
		message = "企业模型已关闭，分析将使用本地规则归纳"
	} else if strings.TrimSpace(config.APIKeyCipher) == "" {
		message = "尚未保存 API Key"
	} else if !config.LastTestOK && config.LastTestAt != nil {
		message = "最近一次连通性测试未通过：" + config.LastTestError
	}
	writeJSON(w, http.StatusOK, toModelConfigView(config, message))
}

func (s *Server) saveModelConfig(w http.ResponseWriter, r *http.Request, organizationID string, user model.User) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, modelConfigUnavailable)
		return
	}
	var input model.ModelConfigInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if err := validateModelInput(input, s.allowPrivateUpstream); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 先探测再落盘：保存一个连不上的配置，只会在事后变成一个难以定位的故障。
	existing, _ := s.store.ModelConfig(organizationID)
	apiKey := strings.TrimSpace(input.APIKey)
	if input.ClearAPIKey {
		apiKey = ""
	}
	if apiKey == "" && !input.ClearAPIKey && strings.TrimSpace(existing.APIKeyCipher) != "" {
		decrypted, err := s.secrets.Decrypt(existing.APIKeyCipher)
		if err != nil {
			// 解不开旧密文（换了主密钥）时必须明说，不能静默当成"没有 Key"。
			writeError(w, http.StatusConflict, "已保存的密钥无法解密（主密钥可能已更换），请重新填写 API Key")
			return
		}
		apiKey = decrypted
	}

	if input.Enabled {
		if apiKey == "" {
			writeError(w, http.StatusBadRequest, "启用企业模型前必须提供 API Key")
			return
		}
		probe := model.ModelConnectionTest{
			Provider: input.Provider, BaseURL: input.BaseURL,
			Model: input.Model, APIKey: apiKey, MaxTokens: input.MaxTokens,
		}
		if _, err := s.probe.Ping(probe); err != nil {
			writeError(w, http.StatusBadRequest, "保存前连通性测试失败："+err.Error())
			return
		}
	}

	var ciphertext, hint string
	if apiKey != "" {
		sealed, err := s.secrets.Encrypt(apiKey)
		if err != nil {
			s.logger.Printf("model key encryption failed: %v", err)
			writeError(w, http.StatusInternalServerError, "密钥加密失败，未保存")
			return
		}
		ciphertext = sealed
		hint = modelsecret.Hint(apiKey)
	}

	now := time.Now().UTC()
	saved, err := s.store.UpsertModelConfig(organizationID, user.ID, func(candidate *model.OrganizationModelConfig) error {
		candidate.Enabled = input.Enabled
		candidate.Provider = input.Provider
		candidate.BaseURL = strings.TrimSpace(input.BaseURL)
		candidate.Model = strings.TrimSpace(input.Model)
		candidate.MaxTokens = input.MaxTokens
		candidate.APIKeyCipher = ciphertext
		candidate.APIKeyHint = hint
		if candidate.ConfiguredBy == "" {
			candidate.ConfiguredBy = user.ID
		}
		return nil
	}, model.AuditEvent{
		OrganizationID: organizationID,
		ActorID:        user.ID,
		ActorName:      user.Name,
		Action:         "enterprise.model_config.update",
		Detail:         "enabled=" + boolString(input.Enabled) + " provider=" + string(input.Provider),
		CreatedAt:      now,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	message := "已保存企业模型接入"
	if !saved.Enabled {
		message = "已关闭企业模型，分析将使用本地规则归纳"
	}
	writeJSON(w, http.StatusOK, toModelConfigView(saved, message))
}

func validateModelInput(input model.ModelConfigInput, allowPrivateUpstream bool) error {
	provider := input.Provider
	if provider == "" {
		provider = model.ModelProviderOpenAI
	}
	if provider != model.ModelProviderOpenAI && provider != model.ModelProviderAnthropic {
		return errors.New("接口形态只支持 openai_compatible 或 anthropic")
	}
	if len(strings.TrimSpace(input.BaseURL)) > maxModelBaseURLLen {
		return errors.New("服务地址过长")
	}
	if len(strings.TrimSpace(input.Model)) > maxModelNameLen {
		return errors.New("模型名过长")
	}
	if len(input.APIKey) > maxAPIKeyLen {
		return errors.New("API Key 过长")
	}
	if input.Enabled {
		if strings.TrimSpace(input.BaseURL) == "" {
			return errors.New("启用企业模型前必须填写服务地址")
		}
		if strings.TrimSpace(input.Model) == "" {
			return errors.New("启用企业模型前必须填写模型名")
		}
		if err := modelprobe.ValidateBaseURL(input.BaseURL, allowPrivateUpstream); err != nil {
			return err
		}
	}
	if input.MaxTokens != 0 && (input.MaxTokens < minModelMaxTokens || input.MaxTokens > maxModelMaxTokens) {
		return errors.New("max_tokens 必须在 100 到 8000 之间")
	}
	return nil
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// handleEnterpriseLLMTest 处理 /api/enterprise/llm/test：用表单里的值探测连通性。
//
// 刻意允许未保存的配置：管理员应该能"先测再存"，不必为了测试先把 Key 写进去。
func (s *Server) handleEnterpriseLLMTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, _, ok := s.currentEnterprise(w, r)
	if !ok {
		return
	}
	if !canEditModelConfig(user) {
		writeError(w, http.StatusForbidden, "只有企业管理员可以测试模型连接")
		return
	}
	var input model.ModelConnectionTest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	provider := input.Provider
	if provider == "" {
		provider = model.ModelProviderOpenAI
	}
	if provider != model.ModelProviderOpenAI && provider != model.ModelProviderAnthropic {
		writeError(w, http.StatusBadRequest, "接口形态只支持 openai_compatible 或 anthropic")
		return
	}
	// Key 留空时尝试用已保存的：改地址不改 Key 是常见操作。
	if strings.TrimSpace(input.APIKey) == "" && s.secrets != nil {
		if existing, found := s.store.ModelConfig(user.OrganizationID); found && strings.TrimSpace(existing.APIKeyCipher) != "" {
			decrypted, err := s.secrets.Decrypt(existing.APIKeyCipher)
			if err != nil {
				writeError(w, http.StatusConflict, "已保存的密钥无法解密（主密钥可能已更换），请重新填写 API Key")
				return
			}
			input.APIKey = decrypted
		}
	}
	if input.Model == "" {
		if existing, found := s.store.ModelConfig(user.OrganizationID); found {
			input.Model = existing.Model
		}
	}
	if input.MaxTokens <= 0 {
		input.MaxTokens = 700
	}

	message, err := s.probe.Ping(input)
	if err != nil {
		// 记录失败原因供界面展示，但不记录 Key 与地址以外的内容。
		if _, upsertErr := s.store.UpsertModelConfig(user.OrganizationID, user.ID, func(candidate *model.OrganizationModelConfig) error {
			candidate.LastTestOK = false
			at := time.Now().UTC()
			candidate.LastTestAt = &at
			candidate.LastTestError = err.Error()
			return nil
		}, model.AuditEvent{
			OrganizationID: user.OrganizationID, ActorID: user.ID, ActorName: user.Name,
			Action: "enterprise.model_config.test_failed", CreatedAt: time.Now().UTC(),
		}); upsertErr != nil {
			s.logger.Printf("record model test failure: %v", upsertErr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	if _, upsertErr := s.store.UpsertModelConfig(user.OrganizationID, user.ID, func(candidate *model.OrganizationModelConfig) error {
		candidate.LastTestOK = true
		at := time.Now().UTC()
		candidate.LastTestAt = &at
		candidate.LastTestError = ""
		return nil
	}, model.AuditEvent{
		OrganizationID: user.OrganizationID, ActorID: user.ID, ActorName: user.Name,
		Action: "enterprise.model_config.test_ok", CreatedAt: time.Now().UTC(),
	}); upsertErr != nil {
		s.logger.Printf("record model test success: %v", upsertErr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message})
}

// handleEnterpriseLLMModels 处理 /api/enterprise/llm/models：拉取上游模型列表。
func (s *Server) handleEnterpriseLLMModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, _, ok := s.currentEnterprise(w, r)
	if !ok {
		return
	}
	if !canEditModelConfig(user) {
		writeError(w, http.StatusForbidden, "只有企业管理员可以拉取模型列表")
		return
	}
	var input model.ModelConnectionTest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	provider := input.Provider
	if provider == "" {
		provider = model.ModelProviderOpenAI
	}
	input.Provider = provider
	if provider != model.ModelProviderOpenAI && provider != model.ModelProviderAnthropic {
		writeError(w, http.StatusBadRequest, "接口形态只支持 openai_compatible 或 anthropic")
		return
	}
	if strings.TrimSpace(input.APIKey) == "" && s.secrets != nil {
		if existing, found := s.store.ModelConfig(user.OrganizationID); found && strings.TrimSpace(existing.APIKeyCipher) != "" {
			decrypted, err := s.secrets.Decrypt(existing.APIKeyCipher)
			if err != nil {
				writeError(w, http.StatusConflict, "已保存的密钥无法解密（主密钥可能已更换），请重新填写 API Key")
				return
			}
			input.APIKey = decrypted
		}
	}
	models, err := s.probe.ListModels(input)
	if err != nil {
		if errors.Is(err, modelprobe.ErrBlockedAddress) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleEnterpriseLLMPresets 返回常用接入预设，减少手输地址的机会。
func (s *Server) handleEnterpriseLLMPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if _, _, ok := s.currentEnterprise(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{
		{
			"id": "deepseek", "name": "DeepSeek", "provider": string(model.ModelProviderOpenAI),
			"base_url": "https://api.deepseek.com", "model": "deepseek-chat",
			"hint": "OpenAI 兼容接口，填 API Key 后即可测试",
		},
		{
			"id": "openai", "name": "OpenAI", "provider": string(model.ModelProviderOpenAI),
			"base_url": "https://api.openai.com", "model": "gpt-4o-mini",
			"hint": "OpenAI 兼容接口，填 API Key 后即可测试",
		},
		{
			"id": "anthropic", "name": "Anthropic", "provider": string(model.ModelProviderAnthropic),
			"base_url": "https://api.anthropic.com", "model": "claude-3-5-haiku-latest",
			"hint": "Anthropic Messages 接口，不提供模型列表，需手动填写模型名",
		},
	})
}
