package runtimeconfig

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	ProfileDevelopment = "development"
	ProfileStaging     = "staging"
	ProfileProduction  = "production"
)

var environmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Summary struct {
	Path        string
	Explicit    bool
	Profile     string
	Assignments int
}

type assignment struct {
	line  int
	value string
}

// Load reads the canonical environment file before application components are
// initialized. An explicitly configured file is strict: duplicate keys,
// malformed assignments, missing files and conflicting inherited overrides all
// fail closed. The default local .env remains optional for developer workflows.
func Load() (Summary, error) {
	path := strings.TrimSpace(os.Getenv("DBGUARD_ENV_FILE"))
	explicit := path != ""
	if path == "" {
		path = ".env"
	}

	assignments, err := readEnvironmentFile(path)
	if err != nil {
		if !(errors.Is(err, os.ErrNotExist) && !explicit) {
			return Summary{}, fmt.Errorf("load canonical environment file: %w", err)
		}
		assignments = map[string]assignment{}
	}

	for key, item := range assignments {
		if inherited, exists := os.LookupEnv(key); exists {
			if explicit && inherited != item.value {
				return Summary{}, fmt.Errorf("canonical environment conflict for %s: inherited value differs from file", key)
			}
			continue
		}
		if err := os.Setenv(key, item.value); err != nil {
			return Summary{}, fmt.Errorf("apply canonical environment key %s: %w", key, err)
		}
	}

	profile := strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_ENV_PROFILE")))
	if profile == "" {
		profile = ProfileDevelopment
	}
	if profile == ProfileProduction && !explicit {
		return Summary{}, errors.New("production profile requires an explicit DBGUARD_ENV_FILE")
	}
	if err := ValidateProfile(profile); err != nil {
		return Summary{}, err
	}
	return Summary{Path: path, Explicit: explicit, Profile: profile, Assignments: len(assignments)}, nil
}

func readEnvironmentFile(path string) (map[string]assignment, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	result := make(map[string]assignment)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		raw := scanner.Text()
		if strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\r') {
			return nil, fmt.Errorf("line %d contains a forbidden control character", lineNumber)
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			return nil, fmt.Errorf("line %d uses unsupported export syntax", lineNumber)
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d is not a KEY=VALUE assignment", lineNumber)
		}
		key = strings.TrimSpace(key)
		if !environmentKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("line %d has an invalid environment key", lineNumber)
		}
		if previous, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate environment key %s on lines %d and %d", key, previous.line, lineNumber)
		}
		value, err := parseEnvironmentValue(rawValue)
		if err != nil {
			return nil, fmt.Errorf("line %d value for %s is invalid: %w", lineNumber, key, err)
		}
		result[key] = assignment{line: lineNumber, value: value}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	return result, nil
}

func parseEnvironmentValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if value[0] != '\'' && value[0] != '"' {
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != value[0] {
		return "", errors.New("quoted value is not terminated")
	}
	if value[0] == '\'' {
		return value[1 : len(value)-1], nil
	}
	unquoted, err := strconv.Unquote(value)
	if err != nil {
		return "", errors.New("double-quoted value has invalid escaping")
	}
	return unquoted, nil
}

// ValidateProfile enforces invariants that must be true before the core may
// start under a named deployment profile. Development and staging retain their
// current flexibility; production opts into the enterprise failure-closed set.
func ValidateProfile(profile string) error {
	switch profile {
	case ProfileDevelopment, ProfileStaging:
		return nil
	case ProfileProduction:
		return validateProductionEnvironment()
	default:
		return fmt.Errorf("unsupported DBGUARD_ENV_PROFILE %q", profile)
	}
}

func validateProductionEnvironment() error {
	if err := requireFalseOrUnset("DBGUARD_ENABLE_DEMO_ACCOUNTS"); err != nil {
		return err
	}
	if err := requireFalseOrUnset("DBGUARD_ENABLE_DEMO_DATA"); err != nil {
		return err
	}
	if err := requireExact("DBGUARD_AUTH_SECURE_COOKIE", "true"); err != nil {
		return err
	}
	if err := requireExact("DBGUARD_TRUST_PROXY_HEADERS", "true"); err != nil {
		return err
	}
	if err := requireURL("DBGUARD_PUBLIC_URL", "https"); err != nil {
		return err
	}

	authMode := strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_AUTH_MODE")))
	switch authMode {
	case "local":
	case "oidc", "hybrid":
		if err := requireURL("DBGUARD_OIDC_ISSUER", "https"); err != nil {
			return err
		}
		if err := requireURL("DBGUARD_OIDC_REDIRECT_URL", "https"); err != nil {
			return err
		}
		if err := requireNonEmpty("DBGUARD_OIDC_CLIENT_ID"); err != nil {
			return err
		}
		if err := requireSecret("DBGUARD_OIDC_CLIENT_SECRET", 16); err != nil {
			return err
		}
	default:
		return errors.New("DBGUARD_AUTH_MODE must be local, oidc, or hybrid in production")
	}

	if err := requireExact("DBGUARD_SESSION_MODE", "redis"); err != nil {
		return err
	}
	if err := requireURL("DBGUARD_REDIS_URL", "redis", "rediss"); err != nil {
		return err
	}
	if err := requireExact("DBGUARD_EXPERIMENT_MODE", "postgres"); err != nil {
		return err
	}
	if err := requireURL("DBGUARD_SHADOW_DSN", "postgres", "postgresql"); err != nil {
		return err
	}
	if err := requireSecret("DBGUARD_PASSPORT_HMAC_SECRET", 32); err != nil {
		return err
	}
	if err := requireSecret("DBGUARD_METRICS_TOKEN", 32); err != nil {
		return err
	}
	if err := requireSecret("DBGUARD_OPERATIONS_WEBHOOK_TOKEN", 32); err != nil {
		return err
	}
	if err := requireNonEmpty("DBGUARD_OPERATIONS_ORGANIZATION_ID"); err != nil {
		return err
	}

	storeMode := strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_STORE_MODE")))
	if storeMode == "" {
		storeMode = "file"
	}
	switch storeMode {
	case "file":
		dataPath := strings.TrimSpace(os.Getenv("DBGUARD_DATA_FILE"))
		witnessPath := strings.TrimSpace(os.Getenv("DBGUARD_MIGRATION_WITNESS_FILE"))
		if !isAbsoluteDeploymentPath(dataPath) {
			return errors.New("DBGUARD_DATA_FILE must be an explicit absolute path in production file mode")
		}
		if !isAbsoluteDeploymentPath(witnessPath) {
			return errors.New("DBGUARD_MIGRATION_WITNESS_FILE must be an explicit absolute path in production file mode")
		}
		if filepath.Clean(dataPath) == filepath.Clean(witnessPath) {
			return errors.New("DBGUARD_MIGRATION_WITNESS_FILE must differ from DBGUARD_DATA_FILE")
		}
	case "postgres":
		if err := requireURL("DBGUARD_PRIMARY_DSN", "postgres", "postgresql"); err != nil {
			return err
		}
	default:
		return errors.New("DBGUARD_STORE_MODE must be file or postgres in production")
	}

	workers, err := strconv.Atoi(strings.TrimSpace(os.Getenv("DBGUARD_WORKERS")))
	if err != nil || workers < 1 || workers > 64 {
		return errors.New("DBGUARD_WORKERS must be an integer from 1 to 64 in production")
	}
	return nil
}

func requireExact(key, expected string) error {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(key)), expected) {
		return fmt.Errorf("%s must be %s in production", key, expected)
	}
	return nil
}

func requireFalseOrUnset(key string) error {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "", "0", "false", "no", "off":
		return nil
	case "1", "true", "yes", "on":
		return fmt.Errorf("%s must be disabled in production", key)
	default:
		return fmt.Errorf("%s must be a valid false boolean in production", key)
	}
}

func requireNonEmpty(key string) error {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fmt.Errorf("%s is required in production", key)
	}
	if looksLikePlaceholder(value) {
		return fmt.Errorf("%s must not use a placeholder value in production", key)
	}
	return nil
}

func requireSecret(key string, minimumBytes int) error {
	value := strings.TrimSpace(os.Getenv(key))
	if len([]byte(value)) < minimumBytes {
		return fmt.Errorf("%s must contain at least %d bytes in production", key, minimumBytes)
	}
	if looksLikePlaceholder(value) {
		return fmt.Errorf("%s must not use a placeholder value in production", key)
	}
	return nil
}

func requireURL(key string, schemes ...string) error {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fmt.Errorf("%s is required in production", key)
	}
	if strings.ContainsAny(raw, "<>") || looksLikePlaceholder(raw) {
		return fmt.Errorf("%s must not use a placeholder value in production", key)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be a valid absolute URL in production", key)
	}
	for _, scheme := range schemes {
		if strings.EqualFold(parsed.Scheme, scheme) {
			return nil
		}
	}
	return fmt.Errorf("%s must use an approved scheme in production", key)
}

func looksLikePlaceholder(value string) bool {
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	for _, marker := range []string{"replaceme", "changeme", "placeholder", "insertsecret", "todo"} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func isAbsoluteDeploymentPath(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && (filepath.IsAbs(value) || strings.HasPrefix(value, "/"))
}
