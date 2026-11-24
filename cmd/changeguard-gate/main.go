package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/integration"
	"github.com/kyfd/changeguard/internal/model"
)

type manifest struct {
	Version          int                `json:"version"`
	Environment      string             `json:"environment"`
	ChangeType       string             `json:"change_type"`
	Artifacts        []manifestArtifact `json:"artifacts"`
	SQLPath          string             `json:"sql_path"`
	RollbackSQLPath  string             `json:"rollback_sql_path"`
	RollbackPlan     string             `json:"rollback_plan"`
	RollbackPlanPath string             `json:"rollback_plan_path"`
}

type manifestArtifact struct {
	Kind     model.ArtifactKind `json:"kind"`
	Name     string             `json:"name"`
	Source   string             `json:"source"`
	Language string             `json:"language"`
	Path     string             `json:"path"`
}

type digestResult struct {
	ArtifactSHA256 string `json:"artifact_sha256"`
	Environment    string `json:"environment"`
	ChangeType     string `json:"change_type"`
}

const (
	maxManifestBytes      int64 = 1 << 20
	maxArtifactFileBytes  int64 = 10 << 20
	maxArtifactTotalBytes int64 = 25 << 20
)

type fileBudget struct {
	total int64
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "changeguard-gate:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: changeguard-gate <digest|verify|consume> -manifest .changeguard.json")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", ".changeguard.json", "change manifest path")
	baseURL := flags.String("url", strings.TrimRight(os.Getenv("CHANGEGUARD_URL"), "/"), "ChangeGuard base URL")
	token := flags.String("token", os.Getenv("CHANGEGUARD_TOKEN"), "one-time passport token")
	consumer := flags.String("consumer", os.Getenv("CI_JOB_ID"), "CI job identity")
	gitLabJob := flags.Bool("gitlab-job", false, "prove the executing GitLab job using CI_JOB_TOKEN")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	result, err := digestFromManifest(*manifestPath)
	if err != nil {
		return err
	}
	if command == "digest" {
		return json.NewEncoder(output).Encode(result)
	}
	if command != "verify" && command != "consume" {
		return fmt.Errorf("unknown command %q", command)
	}
	if strings.TrimSpace(*baseURL) == "" || strings.TrimSpace(*token) == "" {
		return errors.New("-url/CHANGEGUARD_URL and -token/CHANGEGUARD_TOKEN are required")
	}
	gateRequest := model.GateRequest{ArtifactSHA256: result.ArtifactSHA256, Environment: result.Environment, Consumer: strings.TrimSpace(*consumer)}
	payload, err := json.Marshal(gateRequest)
	if err != nil {
		return err
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(*baseURL), "/"))
	if err != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") ||
		base.User != nil || base.ForceQuery || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("invalid ChangeGuard base URL")
	}
	jobToken := ""
	expectedConsumer := gateRequest.Consumer
	if expectedConsumer == "" {
		expectedConsumer = "ci-gate"
	}
	if *gitLabJob {
		jobToken = strings.TrimSpace(os.Getenv("CI_JOB_TOKEN"))
		if jobToken == "" || base.Scheme != "https" {
			return errors.New("-gitlab-job requires CI_JOB_TOKEN and an HTTPS ChangeGuard URL")
		}
		if command == "consume" {
			// The backend replaces the caller's label with the independently proved job ID.
			jobID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CI_JOB_ID")), 10, 64)
			if err != nil || jobID <= 0 {
				return errors.New("-gitlab-job consume requires a valid CI_JOB_ID")
			}
			expectedConsumer = "gitlab-job:" + strconv.FormatInt(jobID, 10)
		}
	}
	endpoint := base.String() + "/api/gate/" + command
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("could not construct the Gate request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*token))
	if *gitLabJob {
		request.Header.Set(integration.GitLabJobTokenHeader, jobToken)
	}
	client := &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Gate request failed; check the configured URL and TLS connection")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return fmt.Errorf("gate returned HTTP %d: redirect refused", response.StatusCode)
	}
	const maxGateResponse = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGateResponse+1))
	if err != nil {
		return errors.New("could not read the Gate response")
	}
	if len(body) > maxGateResponse {
		return errors.New("Gate response exceeds the size limit")
	}
	var gateResult model.GateResult
	decodeErr := json.Unmarshal(body, &gateResult)
	if response.StatusCode != http.StatusOK {
		if decodeErr == nil && gateResult.Code != "" {
			return fmt.Errorf("gate returned HTTP %d: %s: %s", response.StatusCode,
				redactGateCredentials(gateResult.Code, *token, jobToken), redactGateCredentials(gateResult.Reason, *token, jobToken))
		}
		return fmt.Errorf("gate returned HTTP %d", response.StatusCode)
	}
	if decodeErr != nil || !gateResult.Allowed || gateResult.Code != "GATE_ALLOWED" || !hasUniqueJSONKeys(body) ||
		!matchesGatePassport(gateResult.Passport, gateRequest, command, expectedConsumer) {
		return errors.New("Gate returned no explicit authorization")
	}
	_, err = io.WriteString(output, redactGateCredentials(string(body), *token, jobToken))
	return err
}

func matchesGatePassport(passport *model.Passport, request model.GateRequest, command, expectedConsumer string) bool {
	if passport == nil || strings.TrimSpace(passport.ID) == "" || strings.TrimSpace(passport.ChangeID) == "" ||
		!strings.EqualFold(passport.ArtifactSHA256, request.ArtifactSHA256) ||
		!strings.EqualFold(strings.TrimSpace(passport.Environment), request.Environment) ||
		passport.RevokedAt != nil || passport.RevokedByID != "" {
		return false
	}
	if command == "verify" {
		return passport.Status == model.PassportActive && passport.ConsumedAt == nil && passport.ConsumedBy == ""
	}
	// Initial consumption and a same-consumer retry return the same persisted record.
	return command == "consume" && passport.Status == model.PassportConsumed &&
		passport.ConsumedAt != nil && !passport.ConsumedAt.IsZero() && passport.ConsumedBy == expectedConsumer
}

// Unmarshal accepts duplicate (including case-variant) keys. Don't authorize a
// response that another JSON reader could interpret differently.
func hasUniqueJSONKeys(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var scanValue func() bool
	scanValue = func() bool {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		var seen map[string]struct{}
		if delim == '{' {
			seen = make(map[string]struct{})
		}
		for decoder.More() {
			if seen != nil {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok {
					return false
				}
				key = strings.ToUpper(strings.ToLower(key))
				if _, exists := seen[key]; exists {
					return false
				}
				seen[key] = struct{}{}
			}
			if !scanValue() {
				return false
			}
		}
		_, err = decoder.Token()
		return err == nil
	}
	return scanValue()
}

func redactGateCredentials(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			encoded, _ := json.Marshal(secret)
			value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "[redacted]")
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func digestFromManifest(filename string) (digestResult, error) {
	absoluteManifest, err := filepath.Abs(filename)
	if err != nil {
		return digestResult{}, err
	}
	manifestFilename, err := filepath.EvalSymlinks(absoluteManifest)
	if err != nil {
		return digestResult{}, err
	}
	content, err := readFileLimited(manifestFilename, maxManifestBytes)
	if err != nil {
		return digestResult{}, err
	}
	var input manifest
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return digestResult{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return digestResult{}, errors.New("manifest must contain exactly one JSON object")
	}
	if input.Version != 1 || strings.TrimSpace(input.Environment) == "" || strings.TrimSpace(input.ChangeType) == "" {
		return digestResult{}, errors.New("manifest version=1, environment and change_type are required")
	}
	base := filepath.Dir(manifestFilename)
	budget := &fileBudget{}
	artifacts := make([]model.ChangeArtifact, 0, len(input.Artifacts)+1)
	hasDatabase := false
	for index, item := range input.Artifacts {
		if strings.TrimSpace(item.Path) == "" || item.Kind == "" {
			return digestResult{}, fmt.Errorf("artifact %d requires kind and path", index+1)
		}
		if !supportedArtifactKind(item.Kind) {
			return digestResult{}, fmt.Errorf("artifact %d kind %q is unsupported; v1 supports DATABASE, CONFIG and KUBERNETES", index+1, item.Kind)
		}
		raw, err := readManifestFile(base, item.Path, budget)
		if err != nil {
			return digestResult{}, fmt.Errorf("read artifact %s: %w", item.Path, err)
		}
		artifact := model.ChangeArtifact{Kind: item.Kind, Name: strings.TrimSpace(item.Name), Source: strings.TrimSpace(item.Source), Language: strings.TrimSpace(item.Language), Content: raw}
		if artifact.Name == "" {
			artifact.Name = string(artifact.Kind) + " 变更证据"
		}
		artifact = changegate.PrepareArtifact(artifact)
		artifacts = append(artifacts, artifact)
		if artifact.Kind == model.ArtifactDatabase {
			hasDatabase = true
		}
	}
	sql, err := readOptionalFile(base, input.SQLPath, budget)
	if err != nil {
		return digestResult{}, err
	}
	rollbackSQL, err := readOptionalFile(base, input.RollbackSQLPath, budget)
	if err != nil {
		return digestResult{}, err
	}
	rollbackPlan := input.RollbackPlan
	if strings.TrimSpace(input.RollbackPlanPath) != "" {
		if rollbackPlan != "" {
			return digestResult{}, errors.New("use rollback_plan or rollback_plan_path, not both")
		}
		rollbackPlan, err = readOptionalFile(base, input.RollbackPlanPath, budget)
		if err != nil {
			return digestResult{}, err
		}
	}
	if hasDatabase && strings.TrimSpace(sql) == "" {
		return digestResult{}, errors.New("DATABASE artifact requires sql_path so CI hashes the executable SQL used by the shadow runner")
	}
	if strings.TrimSpace(sql) != "" && !hasDatabase {
		artifacts = append(artifacts, changegate.PrepareArtifact(model.ChangeArtifact{Kind: model.ArtifactDatabase, Name: "数据库 SQL", Source: "变更单", Language: "SQL", Content: sql}))
	}
	if len(artifacts) == 0 {
		return digestResult{}, errors.New("manifest must reference at least one DATABASE, CONFIG or KUBERNETES artifact")
	}
	digest := changegate.ChangeDigest(input.Environment, input.ChangeType, artifacts, changegate.SHA256(sql), changegate.SHA256(rollbackSQL), rollbackPlan)
	return digestResult{ArtifactSHA256: digest, Environment: strings.TrimSpace(input.Environment), ChangeType: strings.TrimSpace(input.ChangeType)}, nil
}

func supportedArtifactKind(kind model.ArtifactKind) bool {
	switch kind {
	case model.ArtifactDatabase, model.ArtifactConfig, model.ArtifactKubernetes:
		return true
	default:
		return false
	}
}

func readOptionalFile(base, name string, budget *fileBudget) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	content, err := readManifestFile(base, name, budget)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return content, nil
}

func readManifestFile(base, name string, budget *fileBudget) (string, error) {
	trimmed := strings.TrimSpace(name)
	localName := filepath.FromSlash(trimmed)
	if filepath.IsAbs(localName) || filepath.VolumeName(localName) != "" {
		return "", errors.New("artifact paths must be relative to the manifest")
	}
	baseResolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(baseResolved, filepath.Clean(localName)))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	inside, err := pathInside(baseResolved, resolved)
	if err != nil {
		return "", err
	}
	if !inside {
		return "", errors.New("artifact path escapes the manifest directory")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("artifact path must identify a regular file")
	}
	if info.Size() > maxArtifactFileBytes {
		return "", fmt.Errorf("artifact exceeds %d byte limit", maxArtifactFileBytes)
	}
	content, err := readFileLimited(resolved, maxArtifactFileBytes)
	if err != nil {
		return "", err
	}
	if budget.total+int64(len(content)) > maxArtifactTotalBytes {
		return "", fmt.Errorf("artifact total exceeds %d byte limit", maxArtifactTotalBytes)
	}
	budget.total += int64(len(content))
	return string(content), nil
}

func pathInside(base, candidate string) (bool, error) {
	relative, err := filepath.Rel(base, candidate)
	if err != nil {
		return false, err
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative), nil
}

func readFileLimited(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%s exceeds %d byte limit", filepath.Base(filename), limit)
	}
	return content, nil
}
