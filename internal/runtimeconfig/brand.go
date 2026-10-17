package runtimeconfig

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// 4.0 removes the historical brand aliases. Every key is read under the
// canonical prefix only, and a host that still carries any legacy variable
// refuses to start rather than running on a half-migrated configuration.
const (
	brandPrefix  = "CHANGEGUARD_"
	legacyPrefix = "DBGUARD_"
)

// legacyMigrationHint is appended to every legacy rejection so operators get
// the remediation instead of a silently ignored key.
const legacyMigrationHint = "migrate the environment file with deploy/production/changeguard-migrate-env.sh, then restart"

func envFilePath() (string, bool) {
	if path := strings.TrimSpace(os.Getenv(brandPrefix + "ENV_FILE")); path != "" {
		return path, true
	}
	return ".env", false
}

func profileName() string {
	if profile := strings.ToLower(strings.TrimSpace(os.Getenv(brandPrefix + "ENV_PROFILE"))); profile != "" {
		return profile
	}
	return ProfileDevelopment
}

// rejectLegacyBrandEnvironment fails closed when any legacy variable is still
// present in the process environment. Key names are reported; values never are.
func rejectLegacyBrandEnvironment() error {
	keys := legacyBrandKeys(os.Environ())
	if len(keys) == 0 {
		return nil
	}
	return fmt.Errorf("%s variables are no longer accepted in 4.0: %s; %s",
		strings.TrimSuffix(legacyPrefix, "_"), strings.Join(keys, ", "), legacyMigrationHint)
}

// legacyBrandKeys returns sorted legacy key names from KEY=VALUE entries. The
// environment file reader reuses it so a half-migrated file fails the same way.
func legacyBrandKeys(entries []string) []string {
	var keys []string
	for _, entry := range entries {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, legacyPrefix) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
