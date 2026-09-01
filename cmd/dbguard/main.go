package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kyfd/changeguard/internal/agent"
	"github.com/kyfd/changeguard/internal/auth"
	"github.com/kyfd/changeguard/internal/buildinfo"
	"github.com/kyfd/changeguard/internal/experiment"
	"github.com/kyfd/changeguard/internal/httpapi"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/runtimeconfig"
	"github.com/kyfd/changeguard/internal/service"
	"github.com/kyfd/changeguard/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "[ChangeGuard] ", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("build %s", buildinfo.Current())
	configuration, err := runtimeconfig.Load()
	if err != nil {
		logger.Fatalf("environment configuration rejected: %v", err)
	}
	logger.Printf("environment profile=%s canonical_file=%s assignments=%d", configuration.Profile, configuration.Path, configuration.Assignments)
	if len(configuration.LegacyKeys) > 0 {
		logger.Printf("deprecated %s; prefer CHANGEGUARD_* (DBGUARD_* will be removed in v4.0)", strings.Join(configuration.LegacyKeys, ", "))
	}
	checkOnly, err := startupMode(os.Args[1:])
	if err != nil {
		logger.Fatalf("invalid command line: %v", err)
	}
	if checkOnly {
		logger.Printf("configuration check passed")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataPath := envOr("DBGUARD_DATA_FILE", "./data/dbguard.json")
	data, err := store.NewFromEnvironment(ctx, dataPath)
	if err != nil {
		logger.Fatalf("初始化数据存储失败: %v", err)
	}
	defer data.Close()
	if witness := data.MigrationWitnessStatus(); witness.Enabled {
		logger.Printf("rollback migration witness reconciliation=%s restored_changes=%d restored_artifacts=%d interrupted_save=%t",
			witness.Reconciliation, witness.RestoredChanges, witness.RestoredArtifacts, witness.InterruptedSaveUsed)
	}
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

	address, err := listenAddressFromEnvironment()
	if err != nil {
		logger.Fatalf("invalid HTTP listener configuration: %v", err)
	}
	server := &http.Server{
		Addr: address, Handler: httpapi.New(app, authManager, logger, analyzer, data),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second,
		MaxHeaderBytes: 1 << 20,
		WriteTimeout:   60 * time.Second, IdleTimeout: 90 * time.Second,
	}
	go func() {
		logger.Printf("HTTP listener started address=%s", address)
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

func listenAddressFromEnvironment() (string, error) {
	address := strings.TrimSpace(os.Getenv("DBGUARD_LISTEN_ADDRESS"))
	if address == "" {
		address = ":" + strings.TrimSpace(envOr("PORT", "8080"))
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return "", errors.New("DBGUARD_LISTEN_ADDRESS or PORT must resolve to a host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return "", errors.New("HTTP listener port must be an integer from 0 to 65535")
	}
	return address, nil
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

func startupMode(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	if len(arguments) == 1 && arguments[0] == "--check-config" {
		return true, nil
	}
	return false, fmt.Errorf("supported invocation is dbguard [--check-config]")
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
