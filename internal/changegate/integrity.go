package changegate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kyfd/changeguard/internal/model"
)

var (
	secretKeyPattern     = regexp.MustCompile("(?i)(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|signing[_-]?key|credential)")
	assignmentPattern    = regexp.MustCompile("^([[:space:]]*[^#:=]+[[:space:]]*[:=][[:space:]]*)(.*)$")
	jsonSecretPattern    = regexp.MustCompile("(?i)(\\\"(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|signing[_-]?key|credential)\\\"[[:space:]]*:[[:space:]]*)\\\"[^\\\"]*\\\"")
	bearerPattern        = regexp.MustCompile("(?i)(authorization[[:space:]]*[:=][[:space:]]*bearer[[:space:]]+)[^[:space:]\\\"']+")
	uriCredentialPattern = regexp.MustCompile("([a-zA-Z][a-zA-Z0-9+.-]*://[^/@[:space:]]+:)[^/@[:space:]]+(@)")
	privateKeyPattern    = regexp.MustCompile("(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----")
	awsAccessKeyPattern  = regexp.MustCompile("\\b(AKIA|ASIA)[A-Z0-9]{16}\\b")
)

func SHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Redact removes credential values while preserving enough structure for a
// reviewer to understand what changed. Kubernetes Secret blocks are handled
// separately so ordinary ConfigMap data is not destroyed.
func Redact(value string) string {
	value = redactInlineSecrets(value)

	lines := strings.Split(value, "\n")
	for index, line := range lines {
		match := assignmentPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		key := strings.TrimSpace(strings.Trim(match[1], " :=\t\"'"))
		if secretKeyPattern.MatchString(key) && !isSecretReference(match[2]) {
			lines[index] = match[1] + "[REDACTED]"
		}
	}
	return strings.Join(lines, "\n")
}

func redactInlineSecrets(value string) string {
	value = privateKeyPattern.ReplaceAllString(value, "[REDACTED_PRIVATE_KEY]")
	value = bearerPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = uriCredentialPattern.ReplaceAllString(value, "$1[REDACTED]$2")
	value = awsAccessKeyPattern.ReplaceAllString(value, "[REDACTED_AWS_ACCESS_KEY]")
	return jsonSecretPattern.ReplaceAllString(value, "$1\"[REDACTED]\"")
}

func isSecretReference(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(strings.Trim(value, "\"'")))
	return lower == "" || strings.Contains(lower, "$"+"{") || strings.Contains(lower, "{{") ||
		strings.Contains(lower, "secretkeyref") || strings.Contains(lower, "valuefrom") ||
		strings.HasPrefix(lower, "vault:") || strings.HasPrefix(lower, "env:") ||
		lower == "[redacted]" || lower == "null" || lower == "~"
}

func leadingSpaces(value string) int {
	count := 0
	for _, current := range value {
		if current == ' ' {
			count++
			continue
		}
		if current == '\t' {
			count += 2
			continue
		}
		break
	}
	return count
}

func PrepareArtifact(item model.ChangeArtifact) model.ChangeArtifact {
	raw := item.Content
	item.ContentSHA256 = SHA256(raw)
	if item.Kind == model.ArtifactKubernetes {
		item.Content = redactKubernetesSecrets(redactInlineSecrets(raw))
	} else {
		item.Content = Redact(raw)
	}
	item.Name = strings.TrimSpace(item.Name)
	item.Source = strings.TrimSpace(item.Source)
	item.Language = strings.TrimSpace(item.Language)
	return item
}

// PrepareStoredArtifact redacts persisted content without replacing the hash
// that binds the original pre-redaction bytes. Fresh API input must continue
// to use PrepareArtifact so clients cannot supply or retain a stale digest.
func PrepareStoredArtifact(item model.ChangeArtifact) model.ChangeArtifact {
	storedSHA256 := strings.ToLower(strings.TrimSpace(item.ContentSHA256))
	prepared := PrepareArtifact(item)
	if len(storedSHA256) == 64 {
		if _, err := hex.DecodeString(storedSHA256); err == nil {
			prepared.ContentSHA256 = storedSHA256
		}
	}
	return prepared
}

func redactKubernetesSecrets(value string) string {
	documents := splitYAMLDocuments(value)
	for documentIndex, document := range documents {
		if !regexp.MustCompile("(?im)^[[:space:]]*kind:[[:space:]]*Secret[[:space:]]*$").MatchString(document) {
			continue
		}
		lines := strings.Split(document, "\n")
		secretBlockIndent := -1
		for lineIndex, line := range lines {
			trimmed := strings.TrimSpace(line)
			indent := leadingSpaces(line)
			if secretBlockIndent >= 0 {
				if trimmed != "" && !strings.HasPrefix(trimmed, "#") && indent <= secretBlockIndent {
					secretBlockIndent = -1
				} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
					if match := assignmentPattern.FindStringSubmatch(line); len(match) == 3 {
						lines[lineIndex] = match[1] + "[REDACTED]"
						continue
					}
				}
			}
			lower := strings.ToLower(strings.TrimSuffix(trimmed, ":"))
			if lower == "data" || lower == "stringdata" {
				secretBlockIndent = indent
			}
		}
		documents[documentIndex] = strings.Join(lines, "\n")
	}
	return strings.Join(documents, "\n---\n")
}

func splitYAMLDocuments(value string) []string {
	lines := strings.Split(value, "\n")
	documents := []string{}
	current := []string{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "---" {
			documents = append(documents, strings.Join(current, "\n"))
			current = nil
			continue
		}
		current = append(current, line)
	}
	documents = append(documents, strings.Join(current, "\n"))
	return documents
}

type digestArtifact struct {
	Kind          model.ArtifactKind `json:"kind"`
	Name          string             `json:"name"`
	Source        string             `json:"source"`
	Language      string             `json:"language"`
	ContentSHA256 string             `json:"content_sha256"`
}

type digestEnvelope struct {
	Environment        string           `json:"environment"`
	ChangeType         string           `json:"change_type"`
	Artifacts          []digestArtifact `json:"artifacts"`
	SQLSHA256          string           `json:"sql_sha256"`
	RollbackSQLSHA256  string           `json:"rollback_sql_sha256"`
	RollbackPlanSHA256 string           `json:"rollback_plan_sha256"`
}

func ChangeDigest(environment, changeType string, artifacts []model.ChangeArtifact, sqlSHA256, rollbackSHA256, rollbackPlan string) string {
	envelope := digestEnvelope{
		Environment: strings.TrimSpace(environment), ChangeType: strings.TrimSpace(changeType),
		SQLSHA256: sqlSHA256, RollbackSQLSHA256: rollbackSHA256,
		RollbackPlanSHA256: SHA256(rollbackPlan),
	}
	for _, artifact := range artifacts {
		envelope.Artifacts = append(envelope.Artifacts, digestArtifact{
			Kind: artifact.Kind, Name: strings.TrimSpace(artifact.Name), Source: strings.TrimSpace(artifact.Source),
			Language: strings.TrimSpace(artifact.Language), ContentSHA256: artifact.ContentSHA256,
		})
	}
	payload, _ := json.Marshal(envelope)
	return SHA256(string(payload))
}

func RuleSetVersion(policies []model.RiskPolicy) string {
	items := make([]string, 0, len(policies))
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		items = append(items, fmt.Sprintf("%s|%d|%t|%s|%s|%s|%s", policy.Code, policy.Version, policy.Blocking,
			policy.Pattern, strings.Join(policy.Environments, ","), strings.Join(policy.ChangeTypes, ","), strings.Join(policy.ArtifactKinds, ",")))
	}
	sort.Strings(items)
	return "sha256:" + SHA256(strings.Join(items, "\n"))
}
