package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liufengxi/dbguard/internal/auth"
	"github.com/liufengxi/dbguard/internal/service"
	"github.com/liufengxi/dbguard/internal/store"
)

// 用临时升级目录隔离测试。
func withTempUpgradeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envUpgradeDir, dir)
	return dir
}

func newUpgradeTestServer() *Server {
	data := store.NewMemory()
	logger := log.New(io.Discard, "", 0)
	authManager := auth.New(auth.Config{Mode: "disabled"}, data, logger)
	return New(service.New(data, nil, nil), authManager, logger)
}

func TestUpgradeStatusReturnsIdleByDefault(t *testing.T) {
	withTempUpgradeDir(t)
	t.Setenv(envUpgradeApplyEnabled, "")
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/upgrade/status", nil)
	(&Server{}).handleUpgradeStatus(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Current struct {
			Version string `json:"version"`
		} `json:"current"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		ApplyEnabled bool `json:"apply_enabled"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Current.Version == "" {
		t.Fatal("build version missing")
	}
	if payload.Status.State != "idle" {
		t.Fatalf("expected idle state, got %s", payload.Status.State)
	}
	if payload.ApplyEnabled {
		t.Fatal("upgrade apply must be disabled by default")
	}
}

func TestUpgradeUploadWritesPendingAndStatus(t *testing.T) {
	withTempUpgradeDir(t)
	// 构造一个假升级包（内容任意，仅测上传链路）
	content := []byte("fake-upgrade-archive-content")
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("archive", "changeguard-2026.08.10.test.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/upgrade/upload", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	// 无用户上下文时应拒绝（未认证）
	(&Server{}).handleUpgradeUpload(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated upload must be forbidden, got %d", response.Code)
	}

	// 检查没有写入文件
	root := os.Getenv(envUpgradeDir)
	pending, err := os.ReadDir(filepath.Join(root, "pending"))
	if err == nil && len(pending) != 0 {
		t.Fatalf("no files should be written without auth, got %d", len(pending))
	}
}

func TestUpgradeStatusAndHistoryRoundTrip(t *testing.T) {
	withTempUpgradeDir(t)
	status := upgradeStatus{
		State:         upgradeStatePending,
		Version:       "2026.08.10.test",
		ArchiveName:   "changeguard-2026.08.10.test.tar.gz",
		ArchiveSHA256: "abcd1234",
		Message:       "测试状态",
	}
	if err := writeUpgradeStatus(status); err != nil {
		t.Fatal(err)
	}
	loaded := readUpgradeStatus()
	if loaded.State != upgradeStatePending || loaded.Version != status.Version {
		t.Fatalf("status round-trip failed: %+v", loaded)
	}

	entry := upgradeHistoryEntry{
		Version: "2026.08.10.test", State: upgradeStateSuccess, Message: "测试成功",
	}
	if err := appendUpgradeHistory(entry); err != nil {
		t.Fatal(err)
	}
	history := readUpgradeHistory()
	if len(history) != 1 || history[0].Version != entry.Version {
		t.Fatalf("history round-trip failed: %+v", history)
	}
}

func TestUpgradeApplyDisabledByDefault(t *testing.T) {
	root := withTempUpgradeDir(t)
	t.Setenv(envUpgradeApplyEnabled, "")
	server := newUpgradeTestServer()
	request := httptest.NewRequest(http.MethodPost, "/api/upgrade/apply", strings.NewReader(`{}`))
	request.Header.Set("X-Actor-ID", "usr_owner")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled apply status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != upgradeApplyDisabledCode || payload.Message != upgradeApplyDisabledMessage {
		t.Fatalf("unexpected disabled response: %+v", payload)
	}
	if _, err := os.Stat(filepath.Join(root, "apply.requested")); !os.IsNotExist(err) {
		t.Fatalf("disabled apply must not create trigger, stat error=%v", err)
	}
}

func TestUpgradeApplyExplicitlyEnabled(t *testing.T) {
	root := withTempUpgradeDir(t)
	t.Setenv(envUpgradeApplyEnabled, "true")
	status := upgradeStatus{
		State:       upgradeStatePending,
		Version:     "2026.08.13.test",
		ArchiveName: "changeguard-2026.08.13.test.tar.gz",
	}
	if err := writeUpgradeStatus(status); err != nil {
		t.Fatal(err)
	}
	server := newUpgradeTestServer()
	request := httptest.NewRequest(http.MethodPost, "/api/upgrade/apply", strings.NewReader(`{}`))
	request.Header.Set("X-Actor-ID", "usr_owner")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("enabled apply status=%d body=%s", response.Code, response.Body.String())
	}
	trigger, err := os.ReadFile(filepath.Join(root, "apply.requested"))
	if err != nil {
		t.Fatal(err)
	}
	if string(trigger) != status.ArchiveName {
		t.Fatalf("trigger=%q want=%q", trigger, status.ArchiveName)
	}
}

func TestUpgradeFrontendKeepsStatusAndHidesDisabledApply(t *testing.T) {
	content, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, marker := range []string{
		`api("/api/upgrade/status")`,
		`const upgradeApplyEnabled = upgradeInfo.apply_enabled === true;`,
		`upgradeApplyEnabled && upgradePending`,
		`在线升级执行默认关闭`,
	} {
		if !strings.Contains(script, marker) {
			t.Fatalf("upgrade frontend safety boundary missing %q", marker)
		}
	}
	if strings.Contains(script, `id="upgradeApplyButton" hidden`) {
		t.Fatal("upgrade apply button must not be rendered while disabled")
	}
}

func TestUpgradeApplyRejectsNonPending(t *testing.T) {
	withTempUpgradeDir(t)
	t.Setenv(envUpgradeApplyEnabled, "true")
	request := httptest.NewRequest(http.MethodPost, "/api/upgrade/apply", nil)
	response := httptest.NewRecorder()
	// 无认证直接拒绝
	(&Server{}).handleUpgradeApply(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated apply must be forbidden, got %d", response.Code)
	}
}
