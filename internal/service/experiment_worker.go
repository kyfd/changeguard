package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

func (s *Service) worker(ctx context.Context, workerID string) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.queue:
		case <-ticker.C:
		}
		for s.processOutboxEvent(ctx, workerID) {
		}
	}
}

func (s *Service) processOutboxEvent(ctx context.Context, workerID string) bool {
	const lease = 2 * time.Minute
	event, err := s.store.ClaimOutbox(workerID, lease)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		return false
	}
	processingCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-processingCtx.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				if renewErr := s.store.RenewOutbox(event.ID, workerID, event.LeaseGeneration, lease); renewErr != nil {
					renewDone <- renewErr
					cancel()
					return
				}
			}
		}
	}()
	var processErr error
	switch event.EventType {
	case "experiment.requested":
		processErr = s.runExperiment(processingCtx, event, workerID)
	default:
		processErr = fmt.Errorf("unsupported outbox event %s", event.EventType)
	}
	cancel()
	if renewErr := <-renewDone; processErr == nil && renewErr != nil {
		processErr = fmt.Errorf("续租异步任务失败: %w", renewErr)
	}
	if processErr != nil {
		_ = s.store.FailOutbox(event.ID, workerID, event.LeaseGeneration, processErr)
	} else {
		_ = s.store.CompleteOutbox(event.ID, workerID, event.LeaseGeneration)
	}
	return true
}

func retryConcurrentWrite(action func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = action()
		if !errors.Is(err, store.ErrConcurrentWrite) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return err
}

func (s *Service) runExperiment(ctx context.Context, event model.OutboxEvent, workerID string) error {
	change, err := s.store.Change(event.AggregateID)
	if err != nil {
		return err
	}
	if event.InputSHA256 == "" || event.InputSHA256 != change.ArtifactSHA256 {
		return fmt.Errorf("演练输入摘要与当前制品不一致")
	}
	if event.ResultDigest != "" {
		// FINALIZE and the business report were committed atomically. A restart
		// only needs to complete the outbox envelope; it must not rerun APPLY.
		return nil
	}
	if change.Status != model.StatusExperimentQueued && change.Status != model.StatusExperimentRunning {
		return nil
	}
	if err := s.store.CheckpointExperimentOutbox(event.ID, workerID, event.LeaseGeneration, model.OutboxStagePrepare, event.InputSHA256); err != nil {
		return err
	}
	systemActor := model.User{ID: "system_worker", OrganizationID: change.OrganizationID, Name: "演练 Worker", Role: "系统", Active: true}
	running, err := s.store.UpdateChange(event.AggregateID, func(item *model.ChangeRequest) error {
		if item.ArtifactSHA256 != event.InputSHA256 {
			return fmt.Errorf("演练输入摘要与当前制品不一致")
		}
		if item.Status != model.StatusExperimentQueued && item.Status != model.StatusExperimentRunning {
			return ErrInvalidState
		}
		if item.Status == model.StatusExperimentQueued {
			item.Status = model.StatusExperimentRunning
			item.Timeline = append(item.Timeline, timeline(model.StatusExperimentRunning, "开始预发布验证", "执行单体隔离影子事务；中断后由新 lease generation 从 APPLY 重跑", systemActor.Name))
		}
		return nil
	}, auditExperiment(audit(systemActor, event.AggregateID, "EXPERIMENT_START", fmt.Sprintf("Worker 开始执行 attempt=%s generation=%d", event.AttemptID, event.LeaseGeneration)), event.ID, event.AttemptID))
	if err != nil {
		return err
	}
	s.publish(running, "预发布验证进行中")
	if strings.TrimSpace(change.SQL) == "" {
		return fmt.Errorf("非 SQL 变更不执行数据库影子演练")
	}
	if err := s.store.CheckpointExperimentOutbox(event.ID, workerID, event.LeaseGeneration, model.OutboxStageApply, event.InputSHA256); err != nil {
		return err
	}
	report := s.runner.Run(ctx, change)
	enrichReleaseReport(&report, change)
	report.AttemptID = event.AttemptID
	report.LeaseGeneration = event.LeaseGeneration
	report.InputSHA256 = event.InputSHA256
	if err := ctx.Err(); err != nil {
		return err
	}
	latest, err := s.store.Change(event.AggregateID)
	if err != nil {
		return err
	}
	if latest.ArtifactSHA256 != event.InputSHA256 {
		return fmt.Errorf("演练完成时输入摘要已变化")
	}
	latest.Experiment = &report
	analysis := s.analyzer.Analyze(ctx, latest)
	normalizeAdvisoryRisk(&analysis)
	if err := ctx.Err(); err != nil {
		return err
	}
	next := model.StatusWaitingApproval
	title := "预发布验证完成，等待审批"
	detail := "规则、数据库演练、制品检查、发布策略和智能分析证据已汇总"
	if !trustedSQLExperiment(&report, change) {
		next = model.StatusCheckFailed
		title = "预发布验证失败"
		detail = defaultString(report.ExecutionError, "验证未通过，请修正后重新提交")
	}
	digestInput, err := json.Marshal(report)
	if err != nil {
		return err
	}
	resultDigest := changegate.SHA256(string(digestInput))
	report.ResultDigest = resultDigest
	updated, err := s.store.FinalizeExperimentOutbox(event.ID, workerID, event.LeaseGeneration, event.AttemptID, event.InputSHA256, resultDigest, func(item *model.ChangeRequest) error {
		if item.ArtifactSHA256 != event.InputSHA256 {
			return fmt.Errorf("演练完成时输入摘要已变化")
		}
		item.Experiment = &report
		item.Analysis = &analysis
		item.Status = next
		item.Timeline = append(item.Timeline, timeline(next, title, detail, systemActor.Name))
		return nil
	}, auditExperiment(audit(systemActor, event.AggregateID, "EXPERIMENT_FINISH", fmt.Sprintf("%s；attempt=%s generation=%d", detail, event.AttemptID, event.LeaseGeneration)), event.ID, event.AttemptID))
	if err != nil {
		return err
	}
	s.publish(updated, detail)
	return nil
}

func releaseValidationReport(change model.ChangeRequest) model.ExperimentReport {
	now := time.Now()
	return model.ExperimentReport{
		ID: store.NewID("validation_"), Kind: "STATIC_GATE", Mode: "DETERMINISTIC",
		Status: "NOT_RUN", StartedAt: now, FinishedAt: now,
		ExecutionError: "非 SQL 变更仅执行确定性静态门禁，不伪造运行时演练结果",
	}
}

func enrichReleaseReport(report *model.ExperimentReport, change model.ChangeRequest) {
	if report.ID == "" {
		report.ID = store.NewID("validation_")
	}
	if report.Kind == "" {
		if strings.TrimSpace(change.SQL) != "" {
			report.Kind = "DATABASE_AND_RELEASE_VALIDATION"
		} else {
			report.Kind = "RELEASE_VALIDATION"
		}
	}
	if report.Mode == "" {
		report.Mode = "DETERMINISTIC"
	}
	report.Strategy = change.ReleasePlan.Strategy
	report.CanaryPercent = change.ReleasePlan.CanaryPercent
	report.ObservationMinutes = change.ReleasePlan.ObservationMinutes
	report.ChecksTotal = len(change.Artifacts) + 3
	if report.Status == "PASSED" && strings.EqualFold(report.Mode, "POSTGRES") {
		report.ChecksPassed = report.ChecksTotal
	} else {
		report.ChecksPassed = 0
	}
	now := time.Now()
	kinds := make([]string, 0, len(change.Artifacts))
	seen := make(map[model.ArtifactKind]bool)
	for _, artifact := range change.Artifacts {
		if !seen[artifact.Kind] {
			seen[artifact.Kind] = true
			kinds = append(kinds, string(artifact.Kind))
		}
	}
	report.Evidence = append(report.Evidence,
		model.Evidence{ID: store.NewID("ev_artifact_"), Kind: "artifact", Title: "变更制品清单", Value: fmt.Sprintf("%d 项：%s", len(change.Artifacts), strings.Join(kinds, " / ")), Source: "统一变更单", ObservedAt: now},
		model.Evidence{ID: store.NewID("ev_release_"), Kind: "release", Title: "发布策略", Value: fmt.Sprintf("%s，灰度 %d%%，观察 %d 分钟", change.ReleasePlan.Strategy, change.ReleasePlan.CanaryPercent, change.ReleasePlan.ObservationMinutes), Source: "发布计划", ObservedAt: now},
		model.Evidence{ID: store.NewID("ev_metric_"), Kind: "metric", Title: "成功判定指标", Value: strings.Join(change.ReleasePlan.SuccessMetrics, " / "), Source: "发布计划", ObservedAt: now},
	)
}

func (s *Service) OperationsFor(actorID string) (model.OperationsSummary, []model.OutboxEvent, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return model.OperationsSummary{}, nil, err
	}
	if !actor.EnterpriseAdmin && actor.Role != model.RoleOwner {
		return model.OperationsSummary{}, nil, ErrForbidden
	}
	return s.store.OperationsSummary(actor.OrganizationID), s.store.OutboxByOrganization(actor.OrganizationID, false, 100), nil
}

func (s *Service) RetryOutbox(id, actorID string) error {
	actor, err := s.activeActor(actorID)
	if err != nil || (!actor.EnterpriseAdmin && actor.Role != model.RoleOwner) {
		return ErrForbidden
	}
	if err := s.store.RetryOutbox(actor.OrganizationID, id, model.AuditEvent{
		ID: store.NewID("audit_"), ActorID: actor.ID, ActorName: actor.Name,
		Action: "RETRY_OUTBOX", Detail: "人工重试死信事件 " + id, CreatedAt: time.Now(),
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return fmt.Errorf("%w：当前事件不是死信状态", ErrInvalidState)
	}
	select {
	case s.queue <- id:
	default:
	}
	return nil
}
