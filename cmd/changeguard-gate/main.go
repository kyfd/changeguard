package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
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
	endpoint := strings.TrimRight(*baseURL, "/") + "/api/gate/" + command
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*token))
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("gate returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	_, err = output.Write(body)
	return err
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
