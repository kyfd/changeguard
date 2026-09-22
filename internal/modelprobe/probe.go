// Package modelprobe 负责与上游模型服务对话：测试连通性、拉取模型列表。
//
// 两条硬边界：
//
//  1. **SSRF 防护**。服务地址由企业管理员填写，若不加限制，
//     一个管理员就能让服务器去探测内网、云元数据端点或其他本地服务。
//     因此只允许 http/https，且拒绝环回、私网、链路本地、保留地址与 .internal。
//  2. **错误信息不得泄露密钥**。上游 4xx 的响应体常把请求头回显出来，
//     直接透出等于把 API Key 写进界面和日志。这里只保留分类后的一句话。
package modelprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

const (
	probeTimeout      = 20 * time.Second
	maxResponseBytes  = 1 << 20
	maxModelsReturned = 200
)

// ErrBlockedAddress 地址被 SSRF 策略拒绝。
var ErrBlockedAddress = errors.New("服务地址不被允许")

// Client 是探测用的 HTTP 客户端。
type Client struct {
	http *http.Client
	// validate 是地址策略。生产用 ValidateBaseURL；测试可替换以验证请求形状
	// （httptest 监听环回地址，会被真实策略拒绝）。默认值不可为空。
	validate func(string) error
}

// New 构造探测客户端。allowPrivate 为 true 时放行私网与环回地址。
// 默认（false）拒绝一切非公网地址。设为 true 只应发生在**明确的内网部署**：
// 企业的模型网关就在同一内网时，禁止私网等于这个功能不可用。这是一个显式开关，
// 不是自动探测——默认锁死，需要的人自己开，并且为此负责。
func New(allowPrivate bool) *Client {
	return &Client{
		http: &http.Client{
			Timeout: probeTimeout,
			// 不跟随跳转：一个公网地址 302 到 169.254.169.254 就绕过了上面的校验。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		validate: func(raw string) error { return ValidateBaseURL(raw, allowPrivate) },
	}
}

// checkAddress 走注入的策略；未注入时退回真实策略，避免出现"没有校验"的分支。
func (c *Client) checkAddress(raw string) error {
	if c.validate != nil {
		return c.validate(raw)
	}
	return ValidateBaseURL(raw, false)
}

// AllowPrivateUpstream 读取环境开关：是否放行私网/环回地址作为模型上游。
//
// 默认 false（拒绝）。仅当企业的模型网关确实部署在内网时才需要打开，
// 否则这个功能在纯内网部署下不可用。开关必须显式设置，不做任何自动推断。
func AllowPrivateUpstream() bool {
	value := strings.TrimSpace(os.Getenv("DBGUARD_MODEL_ALLOW_PRIVATE_UPSTREAM"))
	return strings.EqualFold(value, "1") || strings.EqualFold(value, "true")
}

// ValidateBaseURL 校验服务地址是否允许访问。
//
// 空地址返回 error：调用方必须显式要求一个地址，不能默认成某个固定端点。
// allowPrivate=false 时拒绝环回、私网、链路本地与云元数据端点。
func ValidateBaseURL(raw string, allowPrivate bool) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("服务地址不能为空")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("服务地址无法解析: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("服务地址只支持 http/https，当前为 %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("服务地址缺少主机名")
	}
	// .internal/.local 常用于内网服务发现，公开部署不应对其发起请求。
	lowered := strings.ToLower(host)
	if !allowPrivate && (strings.HasSuffix(lowered, ".internal") || strings.HasSuffix(lowered, ".local")) {
		return fmt.Errorf("%w：%s", ErrBlockedAddress, host)
	}
	// 先按字面量判断（覆盖 127.0.0.1、10.x、169.254.x 等常见写法），
	// 能解析的再解析一遍（覆盖 localhost、指向内网的 DNS 名）。
	if ip := net.ParseIP(host); ip != nil {
		if !addressAllowed(ip, allowPrivate) {
			return fmt.Errorf("%w：%s", ErrBlockedAddress, host)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("服务地址无法解析主机 %s", host)
	}
	if len(ips) == 0 {
		return fmt.Errorf("服务地址没有解析到任何地址: %s", host)
	}
	for _, ip := range ips {
		if !addressAllowed(ip, allowPrivate) {
			return fmt.Errorf("%w：%s 解析到非公开地址", ErrBlockedAddress, host)
		}
	}
	return nil
}

// addressAllowed 判断单个 IP 是否允许作为模型上游。
//
// 与 isPublicIP 的区别：allowPrivate=true 时放行私网与环回（内网网关），
// 但**链路本地永远拒绝**——169.254.169.254 这类云元数据端点不是模型服务，
// 放行它等于给 SSRF 留了一个固定目标。
func addressAllowed(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if allowPrivate {
		// 只放行环回与私网；多播、未指定、保留段仍然拒绝。
		return ip.IsLoopback() || ip.IsPrivate() || isPublicIP(ip)
	}
	return isPublicIP(ip)
}

// isPublicIP 判断是否为可公开访问的 IP。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// 云元数据端点与其他保留段。
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0:
			return false
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // CGNAT
			return false
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2: // TEST-NET-1
			return false
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 基准测试
			return false
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100: // TEST-NET-3
			return false
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113: // TEST-NET-2
			return false
		case ip4[0] >= 240: // 保留
			return false
		}
	}
	return true
}

// Ping 发一个最小的 chat/completions 请求，确认地址、Key 与模型名可用。
//
// max_tokens=1：只为验证链路可达，不产生实质消耗。
func (c *Client) Ping(cfg model.ModelConnectionTest) (string, error) {
	if err := c.checkAddress(cfg.BaseURL); err != nil {
		return "", err
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", errors.New("API Key 不能为空")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return "", errors.New("模型名不能为空")
	}
	endpoint, err := chatCompletionsURL(cfg.BaseURL, cfg.Provider)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"model":      strings.TrimSpace(cfg.Model),
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	status, _, err := c.post(endpoint, cfg.APIKey, cfg.Provider, raw)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		return "连接成功", nil
	}
	return "", classifyFailure(status)
}

// ListModels 拉取上游模型列表。
func (c *Client) ListModels(cfg model.ModelConnectionTest) ([]model.UpstreamModel, error) {
	if err := c.checkAddress(cfg.BaseURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("API Key 不能为空")
	}
	endpoint, err := modelsURL(cfg.BaseURL, cfg.Provider)
	if err != nil {
		return nil, err
	}
	status, payload, err := c.get(endpoint, cfg.APIKey, cfg.Provider)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, classifyFailure(status)
	}
	var parsed struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("上游返回的模型列表无法解析: %w", err)
	}
	out := make([]model.UpstreamModel, 0, len(parsed.Data))
	seen := make(map[string]bool, len(parsed.Data))
	for _, item := range parsed.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, model.UpstreamModel{ID: id, OwnedBy: strings.TrimSpace(item.OwnedBy)})
		if len(out) >= maxModelsReturned {
			break
		}
	}
	if len(out) == 0 {
		return nil, errors.New("上游没有返回任何可用模型")
	}
	return out, nil
}

// chatCompletionsURL 按接口形态推导对话补全端点。
//
// 刻意不做"猜路径"：OpenAI 兼容固定 /chat/completions，Anthropic 固定 /v1/messages。
// 用户填了带路径的地址时保留其路径，只补缺失的一段。
func chatCompletionsURL(baseURL string, provider model.ModelProviderKind) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("服务地址无法解析: %w", err)
	}
	if provider == model.ModelProviderAnthropic {
		if strings.HasSuffix(parsed.Path, "/v1/messages") {
			return trimmed, nil
		}
		if strings.HasSuffix(parsed.Path, "/v1") {
			return trimmed + "/messages", nil
		}
		return trimmed + "/v1/messages", nil
	}
	if strings.HasSuffix(parsed.Path, "/chat/completions") {
		return trimmed, nil
	}
	if strings.HasSuffix(parsed.Path, "/v1") {
		return trimmed + "/chat/completions", nil
	}
	return trimmed + "/v1/chat/completions", nil
}

// modelsURL 按接口形态推导模型列表端点。Anthropic 没有公开的 /models，
// 因此明确报错，而不是发一个必然 404 的请求。
func modelsURL(baseURL string, provider model.ModelProviderKind) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("服务地址无法解析: %w", err)
	}
	if provider == model.ModelProviderAnthropic {
		return "", errors.New("Anthropic 接口不提供模型列表，请手动填写模型名")
	}
	if strings.HasSuffix(parsed.Path, "/models") {
		return trimmed, nil
	}
	if strings.HasSuffix(parsed.Path, "/v1") {
		return trimmed + "/models", nil
	}
	return trimmed + "/v1/models", nil
}

func (c *Client) post(endpoint, apiKey string, provider model.ModelProviderKind, body []byte) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	applyHeaders(request, apiKey, provider)
	return c.do(request)
}

func (c *Client) get(endpoint, apiKey string, provider model.ModelProviderKind) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	applyHeaders(request, apiKey, provider)
	return c.do(request)
}

func applyHeaders(request *http.Request, apiKey string, provider model.ModelProviderKind) {
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if provider == model.ModelProviderAnthropic {
		request.Header.Set("x-api-key", apiKey)
		request.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
}

func (c *Client) do(request *http.Request) (int, []byte, error) {
	response, err := c.http.Do(request)
	if err != nil {
		// 不做 url.Error 拆解：那里可能带上完整 URL 与查询串。
		return 0, nil, errors.New("无法连接到上游服务，请检查地址与网络")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return response.StatusCode, nil, errors.New("读取上游响应失败")
	}
	return response.StatusCode, payload, nil
}

// classifyFailure 把上游错误压成一句不含密钥、不含上游原文的话。
//
// 上游 4xx 的响应体经常回显请求头（含 Authorization），透出等于泄露 Key；
// 因此这里只按状态码分类，丢弃响应体。
func classifyFailure(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errors.New("上游拒绝了凭据（401/403）：请检查 API Key 是否有效、是否有该模型权限")
	case status == http.StatusNotFound:
		return errors.New("上游返回 404：请检查服务地址是否需要带 /v1，或该接口是否提供此端点")
	case status == http.StatusTooManyRequests:
		return errors.New("上游限流（429）：请稍后重试或提升配额")
	case status >= 500:
		return fmt.Errorf("上游服务错误（%d）：请稍后重试", status)
	default:
		return fmt.Errorf("上游返回了非预期状态 %d", status)
	}
}
