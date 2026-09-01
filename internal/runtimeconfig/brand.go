package runtimeconfig

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	brandPrefix  = "CHANGEGUARD_"
	legacyPrefix = "DBGUARD_"
)

func envFilePath() (string, bool) {
	if path := strings.TrimSpace(os.Getenv(brandPrefix + "ENV_FILE")); path != "" {
		return path, true
	}
	if path := strings.TrimSpace(os.Getenv(legacyPrefix + "ENV_FILE")); path != "" {
		return path, true
	}
	return ".env", false
}

func profileName() string {
	if profile := strings.ToLower(strings.TrimSpace(os.Getenv(brandPrefix + "ENV_PROFILE"))); profile != "" {
		return profile
	}
	if profile := strings.ToLower(strings.TrimSpace(os.Getenv(legacyPrefix + "ENV_PROFILE"))); profile != "" {
		return profile
	}
	return ProfileDevelopment
}

// applyBrandAliases copies CHANGEGUARD_* onto unset DBGUARD_* keys so the rest
// of the process can keep reading the historical names. Conflicting values fail
// closed. Legacy-only keys are returned for a single deprecation warning.
func applyBrandAliases() ([]string, error) {
	brand := map[string]string{}
	legacy := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(key, brandPrefix):
			brand[strings.TrimPrefix(key, brandPrefix)] = value
		case strings.HasPrefix(key, legacyPrefix):
			legacy[strings.TrimPrefix(key, legacyPrefix)] = value
		}
	}

	for suffix, value := range brand {
		legacyKey := legacyPrefix + suffix
		if existing, ok := legacy[suffix]; ok && existing != value {
			return nil, fmt.Errorf("conflicting %s and %s values", brandPrefix+suffix, legacyKey)
		}
		if _, ok := legacy[suffix]; ok {
			continue
		}
		if err := os.Setenv(legacyKey, value); err != nil {
			return nil, fmt.Errorf("apply %s: %w", legacyKey, err)
		}
	}

	var unused []string
	for suffix := range legacy {
		if _, ok := brand[suffix]; ok {
			continue
		}
		unused = append(unused, legacyPrefix+suffix)
	}
	sort.Strings(unused)
	return unused, nil
}
