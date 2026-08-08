package changegate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Detection struct {
	Code     string
	Detail   string
	Evidence string
}

var (
	secretKeyPatternForCheck = regexp.MustCompile("(?i)(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|signing[_-]?key|credential)")
	documentKindPattern      = regexp.MustCompile("(?im)^[[:space:]]*kind:[[:space:]]*([A-Za-z]+)[[:space:]]*$")
	k8sLatestImage           = regexp.MustCompile("(?i)^[[:space:]]*image:[[:space:]]*[^#[:space:]]+:latest([[:space:]#]|$)")
	k8sPrivileged            = regexp.MustCompile("(?i)^[[:space:]]*privileged:[[:space:]]*true\\b")
	k8sHostNamespace         = regexp.MustCompile("(?i)^[[:space:]]*(hostnetwork|hostpid):[[:space:]]*true\\b")
	k8sEscalation            = regexp.MustCompile("(?i)^[[:space:]]*allowprivilegeescalation:[[:space:]]*true\\b")
	k8sRoot                  = regexp.MustCompile("(?i)^[[:space:]]*(runasuser:[[:space:]]*0\\b|runasnonroot:[[:space:]]*false\\b)")
	k8sHostPath              = regexp.MustCompile("(?i)^[[:space:]]*hostpath:[[:space:]]*$")
	k8sReplicas              = regexp.MustCompile("(?im)^[[:space:]]*replicas:[[:space:]]*([0-9]+)\\b")
)

func IsProduction(environment string) bool {
	lower := strings.ToLower(strings.TrimSpace(environment))
	return strings.Contains(lower, "prod") || strings.Contains(environment, "生产")
}

func CheckConfig(content, name string, production bool) []Detection {
	lines := strings.Split(content, "\n")
	var findings []Detection
	authSectionIndent := -1
	for index, raw := range lines {
		trimmedLine := strings.TrimSpace(raw)
		indent := leadingSpaces(raw)
		if authSectionIndent >= 0 && trimmedLine != "" && !strings.HasPrefix(trimmedLine, "#") && indent <= authSectionIndent {
			authSectionIndent = -1
		}
		key, value, ok := parseAssignment(raw)
		if !ok {
			continue
		}
		normalizedKey := normalizeKey(key)
		normalizedValue := normalizeValue(value)
		if (normalizedKey == "auth" || normalizedKey == "authentication" || normalizedKey == "authorization" || normalizedKey == "security") && normalizedValue == "" {
			authSectionIndent = indent
		}
		if secretKeyPatternForCheck.MatchString(key) && (isRedactedMarker(value) || !isSecretReference(value)) {
			findings = append(findings, Detection{Code: "CONFIG_SECRET_EXPOSURE", Detail: "配置中存在疑似明文凭据；原值已在持久化前脱敏。", Evidence: safeLineEvidence(name, index+1, key+"=[REDACTED]")})
		}
		if !production {
			continue
		}
		if isDebugKey(normalizedKey) && isTruthyOrDebug(normalizedValue) {
			findings = append(findings, Detection{Code: "CONFIG_DEBUG_ENABLED", Detail: "生产配置启用了 debug、trace、dev 或详细诊断模式。", Evidence: safeLineEvidence(name, index+1, key+"=enabled")})
		}
		if authSettingUnsafe(normalizedKey, normalizedValue) || (authSectionIndent >= 0 && indent > authSectionIndent && normalizedKey == "enabled" && isFalse(normalizedValue)) {
			findings = append(findings, Detection{Code: "CONFIG_AUTH_DISABLED", Detail: "生产配置关闭了认证/授权或开放了匿名访问。", Evidence: safeLineEvidence(name, index+1, key+"=unsafe")})
		}
		if tlsSettingUnsafe(normalizedKey, normalizedValue) {
			findings = append(findings, Detection{Code: "CONFIG_TLS_VERIFY_DISABLED", Detail: "生产配置跳过了 TLS/证书校验，可能遭受中间人攻击。", Evidence: safeLineEvidence(name, index+1, key+"=unsafe")})
		}
	}
	if production {
		lower := strings.ToLower(content)
		if regexp.MustCompile("(?m)^[[:space:]]+(skip[_-]?verify|insecure[_-]?skip[_-]?verify):[[:space:]]*(true|1|on|yes)\\b").MatchString(lower) && !containsDetection(findings, "CONFIG_TLS_VERIFY_DISABLED") {
			findings = append(findings, Detection{Code: "CONFIG_TLS_VERIFY_DISABLED", Detail: "生产配置跳过了 TLS/证书校验，可能遭受中间人攻击。", Evidence: safeLineEvidence(name, 0, "tls verification disabled")})
		}
	}
	return uniqueDetections(findings)
}

func CheckKubernetes(content, name string, production bool) []Detection {
	var findings []Detection
	for _, document := range splitYAMLDocuments(content) {
		kind := kubernetesKind(document)
		if kind != "" && !isPodSpecKind(kind) {
			continue
		}
		// A pasted PodSpec fragment can omit apiVersion/kind. It must still be
		// scanned instead of silently producing a clean result.
		if kind == "" {
			kind = "PodSpecFragment"
		}
		lines := strings.Split(document, "\n")
		checks := []struct {
			pattern *regexp.Regexp
			code    string
			detail  string
			label   string
		}{
			{k8sLatestImage, "K8S_LATEST_IMAGE", "Kubernetes 清单使用 latest 镜像，无法绑定不可变制品。", "image=:latest"},
			{k8sPrivileged, "K8S_PRIVILEGED", "容器启用了 privileged 特权模式。", "privileged=true"},
			{k8sHostNamespace, "K8S_HOST_NAMESPACE", "工作负载共享宿主机网络或 PID 命名空间。", "host namespace=true"},
			{k8sEscalation, "K8S_PRIVILEGE_ESCALATION", "容器允许进程提升权限。", "allowPrivilegeEscalation=true"},
			{k8sRoot, "K8S_RUN_AS_ROOT", "容器被显式配置为 root 或允许 root 身份运行。", "runAsRoot=true"},
			{k8sHostPath, "K8S_HOST_PATH", "工作负载挂载宿主机 hostPath，突破了容器文件系统隔离。", "hostPath volume"},
		}
		for _, check := range checks {
			if line, ok := firstMatchingLine(lines, check.pattern); ok {
				findings = append(findings, Detection{Code: check.code, Detail: check.detail, Evidence: safeLineEvidence(name, line, check.label)})
			}
		}

		blocks := containerBlocks(lines)
		if len(blocks) == 0 {
			findings = append(findings, Detection{Code: "K8S_RESOURCE_LIMITS", Detail: "工作负载未发现可审计的容器 resources 配置。", Evidence: safeLineEvidence(name, 0, kind+" containers missing")})
		}
		for index, block := range blocks {
			lower := strings.ToLower(strings.Join(block, "\n"))
			containerName := fmt.Sprintf("container#%d", index+1)
			if parsed := containerBlockName(block); parsed != "" {
				containerName = parsed
			}
			if !strings.Contains(lower, "resources:") || !strings.Contains(lower, "requests:") || !strings.Contains(lower, "limits:") {
				findings = append(findings, Detection{Code: "K8S_RESOURCE_LIMITS", Detail: "容器没有同时声明 CPU/内存 requests 与 limits。", Evidence: safeLineEvidence(name, 0, containerName+" resources incomplete")})
			}
			if requiresHealthProbes(kind) && (!strings.Contains(lower, "readinessprobe:") || !strings.Contains(lower, "livenessprobe:")) {
				findings = append(findings, Detection{Code: "K8S_HEALTH_PROBES", Detail: "长期运行容器缺少 readinessProbe 或 livenessProbe。", Evidence: safeLineEvidence(name, 0, containerName+" health probes incomplete")})
			}
		}
		if production && requiresReplicaSafety(kind) {
			match := k8sReplicas.FindStringSubmatch(document)
			replicas := 1
			if len(match) == 2 {
				replicas, _ = strconv.Atoi(match[1])
			}
			if replicas <= 1 {
				findings = append(findings, Detection{Code: "K8S_SINGLE_REPLICA", Detail: "生产工作负载仅有一个副本，升级或节点故障会直接中断服务。", Evidence: safeLineEvidence(name, 0, fmt.Sprintf("%s replicas=%d", kind, replicas))})
			}
		}
	}
	return uniqueDetections(findings)
}

func parseAssignment(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	separator := -1
	for _, current := range []string{"=", ":"} {
		if index := strings.Index(trimmed, current); index >= 0 && (separator < 0 || index < separator) {
			separator = index
		}
	}
	if separator <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(strings.Trim(trimmed[:separator], "\\\"'"))
	value := strings.TrimSpace(strings.SplitN(trimmed[separator+1:], "#", 2)[0])
	return key, value, key != ""
}

func normalizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(value)
}

func isRedactedMarker(value string) bool {
	normalized := normalizeValue(value)
	return normalized == "[redacted]" || normalized == "[redacted_private_key]" || normalized == "[redacted_aws_access_key]"
}

func normalizeValue(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(value, "\\\"'")))
}

func isTruthyOrDebug(value string) bool {
	switch value {
	case "true", "1", "on", "yes", "debug", "trace", "dev", "development":
		return true
	default:
		return false
	}
}

func isFalse(value string) bool {
	switch value {
	case "false", "0", "off", "no", "disabled":
		return true
	default:
		return false
	}
}

func isDebugKey(key string) bool {
	switch key {
	case "debug", "trace", "dev", "devmode", "development", "developmentmode", "profiling", "verbose":
		return true
	default:
		return false
	}
}

func authSettingUnsafe(key, value string) bool {
	switch key {
	case "auth", "authenabled", "authentication", "authenticationenabled", "authorization", "authorizationenabled", "security", "securityenabled":
		return isFalse(value)
	case "disableauth", "authdisabled", "allowanonymous", "anonymousaccess", "anonymousenabled":
		return isTruthyOrDebug(value)
	default:
		return false
	}
}

func tlsSettingUnsafe(key, value string) bool {
	switch key {
	case "skiptlsverify", "skipverify", "insecureskipverify", "tlsinsecure":
		return isTruthyOrDebug(value)
	case "verifyssl", "verifytls", "tlsverify", "rejectunauthorized":
		return isFalse(value)
	case "sslmode":
		return value == "disable" || value == "insecure" || value == "allow"
	default:
		return false
	}
}

func kubernetesKind(document string) string {
	match := documentKindPattern.FindStringSubmatch(document)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func isPodSpecKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "deployment", "statefulset", "replicaset", "daemonset", "job", "cronjob", "pod":
		return true
	default:
		return false
	}
}

func requiresHealthProbes(kind string) bool {
	switch strings.ToLower(kind) {
	case "deployment", "statefulset", "replicaset", "daemonset", "pod":
		return true
	default:
		return false
	}
}

func requiresReplicaSafety(kind string) bool {
	switch strings.ToLower(kind) {
	case "deployment", "statefulset", "replicaset":
		return true
	default:
		return false
	}
}

func containerBlocks(lines []string) [][]string {
	var blocks [][]string
	inContainers := false
	sectionIndent := -1
	itemIndent := -1
	var current []string
	flush := func() {
		if len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := leadingSpaces(line)
		lower := strings.ToLower(strings.TrimSuffix(trimmed, ":"))
		if lower == "containers" || lower == "initcontainers" {
			flush()
			inContainers = true
			sectionIndent = indent
			itemIndent = -1
			continue
		}
		if !inContainers {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && indent >= sectionIndent {
			if itemIndent < 0 {
				itemIndent = indent
			}
			if indent == itemIndent {
				flush()
				current = []string{strings.TrimPrefix(trimmed, "- ")}
				continue
			}
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && indent <= sectionIndent {
			flush()
			inContainers = false
			continue
		}
		if current != nil {
			current = append(current, line)
		}
	}
	flush()
	return blocks
}

func containerBlockName(block []string) string {
	for _, line := range block {
		key, value, ok := parseAssignment(line)
		if ok && strings.EqualFold(key, "name") {
			return strings.Trim(value, "\\\"'")
		}
	}
	return ""
}

func firstMatchingLine(lines []string, pattern *regexp.Regexp) (int, bool) {
	for index, raw := range lines {
		if pattern.MatchString(strings.TrimSpace(raw)) {
			return index + 1, true
		}
	}
	return 0, false
}

func safeLineEvidence(name string, line int, detail string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "artifact"
	}
	if line <= 0 {
		return fmt.Sprintf("%s: %s", name, detail)
	}
	return fmt.Sprintf("%s line %d: %s", name, line, detail)
}

func containsDetection(items []Detection, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func uniqueDetections(items []Detection) []Detection {
	seen := make(map[string]bool, len(items))
	result := make([]Detection, 0, len(items))
	for _, item := range items {
		key := item.Code + "|" + item.Evidence
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, item)
	}
	return result
}
