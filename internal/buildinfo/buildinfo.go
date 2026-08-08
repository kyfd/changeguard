package buildinfo

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var (
	Version      = "dev"
	Commit       = "unknown"
	BuiltAt      = "unknown"
	SourceSHA256 = "unknown"
)

var hexadecimal = regexp.MustCompile(`^[0-9a-f]+$`)

type Info struct {
	Version            string `json:"version"`
	Commit             string `json:"commit"`
	BuiltAt            string `json:"built_at"`
	SourceSHA256       string `json:"source_sha256"`
	GoVersion          string `json:"go_version"`
	ProvenanceVerified bool   `json:"provenance_verified"`
}

func Current() Info {
	version := valueOr(strings.TrimSpace(Version), "dev")
	commit := strings.ToLower(valueOr(strings.TrimSpace(Commit), "unknown"))
	builtAt := valueOr(strings.TrimSpace(BuiltAt), "unknown")
	sourceSHA256 := strings.ToLower(valueOr(strings.TrimSpace(SourceSHA256), "unknown"))
	return Info{
		Version: version, Commit: commit, BuiltAt: builtAt, SourceSHA256: sourceSHA256,
		GoVersion:          runtime.Version(),
		ProvenanceVerified: version != "dev" && validHex(commit, 7, 64) && validHex(sourceSHA256, 64, 64) && validRFC3339(builtAt),
	}
}

func (info Info) String() string {
	return fmt.Sprintf("version=%s commit=%s source_sha256=%s built_at=%s go=%s provenance_verified=%t",
		info.Version, info.Commit, info.SourceSHA256, info.BuiltAt, info.GoVersion, info.ProvenanceVerified)
}

func validHex(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && hexadecimal.MatchString(value)
}

func validRFC3339(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
