package httpapi

// 把企业自配的模型配置接进 Agent 运行时。
//
// internal/agent.Runtime 早已预留 SetResolver 钩子（见 runtime.go 的注释：
// "新企业自配 Key，平台 Key 仅白名单"），但一直没有调用方。这里补上，
// 并刻意保持该钩子原有的失败关闭语义：解析不到企业配置时返回 false，
// Runtime 于是走本地规则归纳，而不是回退到平台 env 里的 Key。

import (
	"strings"

	"github.com/kyfd/changeguard/internal/agent"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/modelsecret"
)

// wireModelConfigResolver 让 Agent 运行时按企业解析模型凭据。
//
// 返回实际生效的企业数量，供启动日志判断"有没有配起来"。
func (s *Server) wireModelConfigResolver() int {
	if s.analyzer == nil || s.store == nil || s.secrets == nil {
		// 缺任何一样都意味着无法安全解析：不注入 resolver，
		// 让 Runtime 保持它自己的 env 行为，而不是注入一个必然失败的解析器。
		return 0
	}
	resolver := func(organizationID string) (agent.LLMConfig, bool) {
		config, found := s.store.ModelConfig(organizationID)
		if !found || !config.Enabled {
			return agent.LLMConfig{}, false
		}
		if strings.TrimSpace(config.APIKeyCipher) == "" {
			return agent.LLMConfig{}, false
		}
		apiKey, err := s.secrets.Decrypt(config.APIKeyCipher)
		if err != nil {
			// 解不开（主密钥被更换）时不静默跳过：记录下来，
			// 否则表现为"配了 Key 但模型一直不生效"且无从排查。
			s.logger.Printf("model config for org %s cannot be decrypted: %v", organizationID, err)
			return agent.LLMConfig{}, false
		}
		provider := config.Provider
		if provider == "" {
			provider = model.ModelProviderOpenAI
		}
		// Anthropic 的 Messages 接口与 OpenAI 兼容接口不同，Runtime 目前只实现
		// 后者。这里明确不生效，而不是把 Anthropic 的地址当 OpenAI 端点调用。
		if provider != model.ModelProviderOpenAI {
			return agent.LLMConfig{}, false
		}
		return agent.LLMConfig{
			BaseURL:   config.BaseURL,
			APIKey:    apiKey,
			Model:     config.Model,
			MaxTokens: config.MaxTokens,
			Source:    "organization",
		}, true
	}
	s.analyzer.SetResolver(resolver)

	count := 0
	for _, config := range s.store.ModelConfigs() {
		if config.Enabled && strings.TrimSpace(config.APIKeyCipher) != "" {
			count++
		}
	}
	return count
}

// modelSecretConfigured 供 /api/config/status 判断能否保存企业模型配置。
func (s *Server) modelSecretConfigured() bool {
	return s.secrets != nil
}

// modelSecretBox 仅供测试使用：返回内部的封装器以验证加密行为。
func (s *Server) modelSecretBox() *modelsecret.Box {
	return s.secrets
}
