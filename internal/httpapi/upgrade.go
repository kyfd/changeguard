package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/buildinfo"
)

// 升级包上传与状态查询。
// Go 服务以受限用户运行，仅负责：接收上传、校验、写入共享待处理目录。
// root 升级 watcher 轮询该目录执行安装/切换/重启，并回写状态文件。
//
// 目录约定（由 watcher 与 web 服务共享）：
//   UPGRADE_DIR/
//     pending/          待处理升级包（changeguard-<version>.tar.gz）
//     status.json       升级状态（go 读、watcher 写）
//     history.json      升级历史
const (
	envUpgradeDir = "DBGUARD_UPGRADE_DIR"

	upgradeStateIdle     = "idle"
	upgradeStatePending  = "pending"
	upgradeStateApplying = "applying"
	upgradeStateSuccess  = "success"
	upgradeStateFailed   = "failed"
	upgradeStateRollback = "rollback"
)

type upgradeStatus struct {
	State             string    `json:"state"`
	Version           string    `json:"version,omitempty"`
	ArchiveName       string    `json:"archive_name,omitempty"`
	ArchiveSHA256     string    `json:"archive_sha256,omitempty"`
	Message           string    `json:"message,omitempty"`
	AppliedAt         time.Time `json:"applied_at,omitempty"`
	PreviousVersion   string    `json:"previous_version,omitempty"`
	HealthAfterUpdate string    `json:"health_after_update,omitempty"`
	RollbackTo        string    `json:"rollback_to,omitempty"`
}

type upgradeHistoryEntry struct {
	Version         string    `json:"version"`
	ArchiveSHA256   string    `json:"archive_sha256"`
	State           string    `json:"state"`
	Message         string    `json:"message"`
	AppliedAt       time.Time `json:"applied_at"`
	PreviousVersion string    `json:"previous_version,omitempty"`
}

func upgradeRoot() string {
	dir := strings.TrimSpace(os.Getenv(envUpgradeDir))
	if dir == "" {
		dir = "/opt/changeguard/upgrades"
	}
	return dir
}

func pendingDir() string  { return filepath.Join(upgradeRoot(), "pending") }
func statusPath() string  { return filepath.Join(upgradeRoot(), "status.json") }
func historyPath() string { return filepath.Join(upgradeRoot(), "history.json") }

func readUpgradeStatus() upgradeStatus {
	var status upgradeStatus
	content, err := os.ReadFile(statusPath())
	if err == nil {
		_ = json.Unmarshal(content, &status)
	}
	if status.State == "" {
		status.State = upgradeStateIdle
	}
	return status
}

func writeUpgradeStatus(status upgradeStatus) error {
	if err := os.MkdirAll(upgradeRoot(), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	tmp := statusPath() + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statusPath())
}

func readUpgradeHistory() []upgradeHistoryEntry {
	var history []upgradeHistoryEntry
	content, err := os.ReadFile(historyPath())
	if err == nil {
		_ = json.Unmarshal(content, &history)
	}
	return history
}

func appendUpgradeHistory(entry upgradeHistoryEntry) error {
	history := readUpgradeHistory()
	history = append([]upgradeHistoryEntry{entry}, history...)
	if len(history) > 20 {
		history = history[:20]
	}
	content, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	tmp := historyPath() + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, historyPath())
}

// handleUpgradeStatus GET /api/upgrade/status
func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status := readUpgradeStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"current":  buildinfo.Current(),
		"status":   status,
		"history":  readUpgradeHistory(),
		"dir":      upgradeRoot(),
		"enabled":  os.Getenv(envUpgradeDir) != "" || filepath.IsAbs(upgradeRoot()),
	})
}

// handleUpgradeUpload POST /api/upgrade/upload (multipart, 字段名 archive)
// 仅企业管理员/技术负责人可上传；校验文件名、大小、SHA256 后写入 pending 目录。
func (s *Server) handleUpgradeUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	actor := actorID(r)
	if s.service == nil {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以上传升级包")
		return
	}
	user, err := s.service.ActorFor(actor)
	if err != nil || (!user.EnterpriseAdmin && user.Role != "技术负责人") {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以上传升级包")
		return
	}

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "请求体过大或格式不正确（最大 64MB）")
		return
	}
	file, header, err := r.FormFile("archive")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少升级包文件（字段名 archive）")
		return
	}
	defer file.Close()

	name := filepath.Base(header.Filename)
	if !strings.HasPrefix(name, "changeguard-") || !strings.HasSuffix(name, ".tar.gz") {
		writeError(w, http.StatusBadRequest, "文件名必须为 changeguard-<version>.tar.gz")
		return
	}
	if header.Size > 512<<20 {
		writeError(w, http.StatusBadRequest, "升级包超过 512MB 限制")
		return
	}

	// 正在升级中禁止覆盖
	current := readUpgradeStatus()
	if current.State == upgradeStateApplying || current.State == upgradeStatePending {
		writeError(w, http.StatusConflict, "已有升级任务进行中，请等待完成")
		return
	}

	if err := os.MkdirAll(pendingDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "无法创建升级目录")
		return
	}
	target := filepath.Join(pendingDir(), name)
	tmp := target + ".uploading"
	out, err := os.Create(tmp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法保存升级包")
		return
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hash), file)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "写入升级包失败")
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		writeError(w, http.StatusInternalServerError, "保存升级包失败")
		return
	}

	archiveSHA := hex.EncodeToString(hash.Sum(nil))
	version := strings.TrimSuffix(strings.TrimPrefix(name, "changeguard-"), ".tar.gz")
	status := upgradeStatus{
		State:         upgradeStatePending,
		Version:       version,
		ArchiveName:   name,
		ArchiveSHA256: archiveSHA,
		Message:       "升级包已上传，等待系统应用",
	}
	if err := writeUpgradeStatus(status); err != nil {
		writeError(w, http.StatusInternalServerError, "写入升级状态失败")
		return
	}
	s.logger.Printf("upgrade package uploaded name=%s size=%d sha256=%s actor=%s", name, written, archiveSHA, actor)
	writeJSON(w, http.StatusOK, status)
}

// handleUpgradeApply POST /api/upgrade/apply
// 触发 watcher 应用已上传的升级包（通过创建触发标记文件）。
func (s *Server) handleUpgradeApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	actor := actorID(r)
	if s.service == nil {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以上传升级包")
		return
	}
	user, err := s.service.ActorFor(actor)
	if err != nil || (!user.EnterpriseAdmin && user.Role != "技术负责人") {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以触发升级")
		return
	}
	current := readUpgradeStatus()
	if current.State != upgradeStatePending {
		writeError(w, http.StatusConflict, fmt.Sprintf("当前状态 %s，没有待应用的升级包", current.State))
		return
	}
	if err := os.MkdirAll(upgradeRoot(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "无法创建升级目录")
		return
	}
	trigger := filepath.Join(upgradeRoot(), "apply.requested")
	if err := os.WriteFile(trigger, []byte(current.ArchiveName), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "触发升级失败")
		return
	}
	s.logger.Printf("upgrade apply requested archive=%s actor=%s", current.ArchiveName, actor)
	writeJSON(w, http.StatusOK, map[string]any{"triggered": true, "message": "升级已触发，系统将自动应用并重启"})
}

// handleUpgradeAbort POST /api/upgrade/abort
// 取消待应用的升级（仅 pending 状态）。
func (s *Server) handleUpgradeAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	actor := actorID(r)
	if s.service == nil {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以上传升级包")
		return
	}
	user, err := s.service.ActorFor(actor)
	if err != nil || (!user.EnterpriseAdmin && user.Role != "技术负责人") {
		writeError(w, http.StatusForbidden, "只有企业管理员或技术负责人可以取消升级")
		return
	}
	current := readUpgradeStatus()
	if current.State != upgradeStatePending {
		writeError(w, http.StatusConflict, "当前没有待应用的升级包")
		return
	}
	if current.ArchiveName != "" {
		_ = os.Remove(filepath.Join(pendingDir(), current.ArchiveName))
	}
	_ = os.Remove(filepath.Join(upgradeRoot(), "apply.requested"))
	_ = writeUpgradeStatus(upgradeStatus{State: upgradeStateIdle, Message: "升级已取消"})
	s.logger.Printf("upgrade aborted actor=%s", actor)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}
