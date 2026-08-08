package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/liufengxi/dbguard/internal/agent"
	"github.com/liufengxi/dbguard/internal/auth"
	"github.com/liufengxi/dbguard/internal/buildinfo"
	"github.com/liufengxi/dbguard/internal/experiment"
	"github.com/liufengxi/dbguard/internal/httpapi"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/service"
	"github.com/liufengxi/dbguard/internal/store"
)

func main() {
	_ = loadDotEnv(".env")
	logger := log.New(os.Stdout, "[DBGuard] ", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("build %s", buildinfo.Current())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataPath := envOr("DBGUARD_DATA_FILE", "./data/dbguard.json")
	data, err := store.NewFromEnvironment(ctx, dataPath)
	if err != nil {
		logger.Fatalf("初始化数据存储失败: %v", err)
	}
	defer data.Close()
	data.StartRefresh(ctx, 2*time.Second)
	analyzer := agent.NewFromEnvironment()
	analyzer.SetDataSource(storeAgentData{store: data})
	app := service.New(data, experiment.NewFromEnvironment(), analyzer)
	authManager := auth.New(auth.FromEnvironment(), data, logger)
	defer authManager.Close()
	healthCtx, healthCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := authManager.Health(healthCtx); err != nil {
		healthCancel()
		logger.Fatalf("初始化会话存储失败: %v", err)
	}
	healthCancel()
	workers, err := workerCountFromEnvironment()
	if err != nil {
		logger.Fatalf("invalid background worker configuration: %v", err)
	}
	if workers == 0 {
		logger.Printf("background workers disabled by DBGUARD_WORKERS=0")
	}
	app.Start(ctx, workers)

	address := ":" + envOr("PORT", "8080")
	server := &http.Server{
		Addr: address, Handler: httpapi.New(app, authManager, logger, analyzer),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		MaxHeaderBytes: 1 << 20,
		WriteTimeout:   60 * time.Second, IdleTimeout: 90 * time.Second,
	}
	go func() {
		logger.Printf("服务已启动: http://localhost%s", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()
	<-ctx.Done()
	logger.Println("收到退出信号，正在等待任务安全结束")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("优雅退出失败: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func workerCountFromEnvironment() (int, error) {
	raw := strings.TrimSpace(os.Getenv("DBGUARD_WORKERS"))
	if raw == "" {
		return 2, nil
	}
	workers, err := strconv.Atoi(raw)
	if err != nil || workers < 0 || workers > 64 {
		return 0, fmt.Errorf("DBGUARD_WORKERS must be an integer from 0 to 64")
	}
	return workers, nil
}

// storeAgentData exposes only the read-only business queries required by the
// allow-listed Agent tools. The runtime cannot mutate approvals or releases.
type storeAgentData struct {
	store *store.Store
}

func (d storeAgentData) Policies(organizationID string) []model.RiskPolicy {
	if d.store == nil {
		return nil
	}
	return d.store.PoliciesByOrganization(organizationID)
}

func (d storeAgentData) RecentChanges(organizationID, applicationID string, limit int) []model.ChangeRequest {
	if d.store == nil {
		return nil
	}
	if limit <= 0 {
		limit = 5
	}
	all := d.store.ChangesByOrganization(organizationID)
	out := make([]model.ChangeRequest, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		change := all[i]
		if applicationID != "" && change.ApplicationID != applicationID {
			continue
		}
		out = append(out, change)
	}
	return out
}

func (d storeAgentData) Application(organizationID, applicationID string) (model.Application, bool) {
	if d.store == nil || applicationID == "" {
		return model.Application{}, false
	}
	for _, application := range d.store.ApplicationsByOrganization(organizationID) {
		if application.ID == applicationID {
			return application, true
		}
	}
	return model.Application{}, false
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\""+"'")
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return scanner.Err()
}
