package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kyfd/changeguard/internal/model"
)

// These tables are authoritative. Generic dbguard_state snapshot saves never
// project to them, so a stale instance cannot restore a prior plan or binding.
const postgresJointReleaseMigration = `
CREATE TABLE IF NOT EXISTS changeguard_joint_releases (
  organization_id text NOT NULL,
  id text NOT NULL,
  seq bigint GENERATED ALWAYS AS IDENTITY,
  payload jsonb NOT NULL,
  PRIMARY KEY (organization_id, id)
);
CREATE INDEX IF NOT EXISTS changeguard_joint_releases_org_seq
  ON changeguard_joint_releases (organization_id, seq);
CREATE TABLE IF NOT EXISTS changeguard_joint_release_members (
  organization_id text NOT NULL,
  change_id text NOT NULL REFERENCES changeguard_changes(id),
  release_id text NOT NULL,
  PRIMARY KEY (organization_id, change_id),
  FOREIGN KEY (organization_id, release_id) REFERENCES changeguard_joint_releases(organization_id, id)
);
CREATE TABLE IF NOT EXISTS changeguard_deployment_jobs (
  source text NOT NULL,
  job_id bigint NOT NULL,
  project_id bigint NOT NULL,
  pipeline_id bigint NOT NULL,
  organization_id text NOT NULL,
  passport_id text NOT NULL UNIQUE REFERENCES changeguard_passports(id),
  release_id text,
  deployment_id bigint,
  PRIMARY KEY (source, job_id),
  UNIQUE (source, project_id, deployment_id),
  FOREIGN KEY (organization_id, release_id) REFERENCES changeguard_joint_releases(organization_id, id)
);
`

func lockReleaseOrganization(ctx context.Context, tx pgx.Tx, org string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 421548014))`, org)
	return err
}

// Plan creation checks grants and application mappings; hold their snapshot
// stable until membership is written. Acquire this before the org lock and
// normalized rows. Gate does not acquire this lock.
func lockReleaseSettings(ctx context.Context, tx pgx.Tx) error {
	var id int
	return tx.QueryRow(ctx, `SELECT id FROM dbguard_state WHERE id=1 FOR SHARE`).Scan(&id)
}

func releaseAccessData(ctx context.Context, tx pgx.Tx, org string) (state, error) {
	var raw []byte
	var data state
	err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
		'organizations', (SELECT jsonb_agg(v) FROM jsonb_array_elements(NULLIF(payload->'organizations','null'::jsonb)) v WHERE v->>'id'=$1),
		'users', (SELECT jsonb_agg(v) FROM jsonb_array_elements(NULLIF(payload->'users','null'::jsonb)) v WHERE v->>'organization_id'=$1),
		'applications', (SELECT jsonb_agg(v) FROM jsonb_array_elements(NULLIF(payload->'applications','null'::jsonb)) v WHERE v->>'organization_id'=$1),
		'application_grants', (SELECT jsonb_agg(v) FROM jsonb_array_elements(NULLIF(payload->'application_grants','null'::jsonb)) v WHERE v->>'organization_id'=$1)
	) FROM dbguard_state WHERE id=1`, org).Scan(&raw)
	if err != nil {
		return data, err
	}
	return data, json.Unmarshal(raw, &data)
}

func releasePlansTx(ctx context.Context, tx pgx.Tx, org, id string) ([]model.JointRelease, error) {
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_joint_releases
		WHERE organization_id=$1 AND ($2='' OR id=$2) ORDER BY seq`, org, id)
	if err != nil {
		return nil, err
	}
	return collectReleaseRows[model.JointRelease](rows)
}

func collectReleaseRows[T any](rows pgx.Rows) ([]T, error) {
	defer rows.Close()
	var items []T
	for rows.Next() {
		var raw []byte
		var item T
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type releaseChangeLock string

const (
	releaseChangeRead   releaseChangeLock = ""
	releaseChangeShare  releaseChangeLock = " FOR SHARE"
	releaseChangeUpdate releaseChangeLock = " FOR UPDATE"
)

func releaseChangesTx(ctx context.Context, tx pgx.Tx, org string, ids []string, lock releaseChangeLock) ([]model.ChangeRequest, error) {
	rows, err := tx.Query(ctx, `SELECT payload FROM changeguard_changes
		WHERE organization_id=$1 AND id=ANY($2) ORDER BY id COLLATE "C"`+string(lock), org, ids)
	if err != nil {
		return nil, err
	}
	return collectReleaseRows[model.ChangeRequest](rows)
}

func releaseChangeIDs(plans ...model.JointRelease) []string {
	var ids []string
	for _, plan := range plans {
		for _, step := range plan.Steps {
			ids = append(ids, step.ChangeID)
		}
	}
	return ids
}

func updateReleaseTx(ctx context.Context, tx pgx.Tx, plan model.JointRelease) error {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE changeguard_joint_releases SET payload=$3 WHERE organization_id=$1 AND id=$2`, plan.OrganizationID, plan.ID, encoded)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrReleaseBlocked
	}
	return err
}

func (b *postgresBackend) createJointRelease(ctx context.Context, actorID, org string, input model.JointReleaseInput, source model.ReleaseSource) (model.JointReleaseView, error) {
	var empty model.JointReleaseView
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockReleaseSettings(ctx, tx); err != nil {
		return empty, err
	}
	if err = lockReleaseOrganization(ctx, tx, org); err != nil {
		return empty, err
	}
	data, err := releaseAccessData(ctx, tx, org)
	if err != nil {
		return empty, err
	}
	plans, err := releasePlansTx(ctx, tx, org, "")
	if err != nil {
		return empty, err
	}
	snapshot := releaseSnapshot{data: data, plans: plans}
	ids := releaseChangeIDs(snapshot.plan(org, strings.TrimSpace(input.Supersedes)))
	for _, pin := range input.Steps {
		ids = append(ids, pin.ChangeID)
	}
	snapshot.data.Changes, err = releaseChangesTx(ctx, tx, org, ids, releaseChangeShare)
	if err != nil {
		return empty, err
	}
	snapshot.data.Policies, err = b.loadOrgPoliciesTx(ctx, tx, org)
	if err != nil {
		return empty, err
	}
	plan, old, err := snapshot.newPlan(actorID, org, input, source)
	if err != nil {
		return empty, err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO changeguard_joint_releases (organization_id,id,payload) VALUES ($1,$2,$3)`, org, plan.ID, encoded); err != nil {
		return empty, err
	}
	if old != nil {
		if err := updateReleaseTx(ctx, tx, *old); err != nil {
			return empty, err
		}
	}
	for _, step := range plan.Steps {
		if step.InheritedFrom != "" {
			continue // the historical member still belongs to its executing plan
		}
		tag, err := tx.Exec(ctx, `INSERT INTO changeguard_joint_release_members (organization_id,change_id,release_id)
			VALUES ($1,$2,$3) ON CONFLICT (organization_id,change_id) DO UPDATE SET release_id=EXCLUDED.release_id
			WHERE changeguard_joint_release_members.release_id=$4`, org, step.ChangeID, plan.ID, plan.Supersedes)
		if err != nil {
			return empty, err
		}
		if tag.RowsAffected() != 1 {
			return empty, ErrReleaseBlocked
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return snapshot.view(plan), nil
}

func (b *postgresBackend) jointReleasesFor(ctx context.Context, actorID, org, id string) ([]model.JointReleaseView, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	data, err := releaseAccessData(ctx, tx, org)
	if err != nil {
		return nil, err
	}
	plans, err := releasePlansTx(ctx, tx, org, id)
	if err != nil {
		return nil, err
	}
	data.Changes, err = releaseChangesTx(ctx, tx, org, releaseChangeIDs(plans...), releaseChangeRead)
	if err != nil {
		return nil, err
	}
	data.Policies, err = b.loadOrgPoliciesTx(ctx, tx, org)
	if err != nil {
		return nil, err
	}
	return (releaseSnapshot{data: data, plans: plans}).visible(actorID, org, id)
}

func postgresReleaseMembership(ctx context.Context, tx pgx.Tx, org, change string) (model.JointRelease, int, error) {
	var plan model.JointRelease
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT r.payload FROM changeguard_joint_release_members m
		JOIN changeguard_joint_releases r ON r.organization_id=m.organization_id AND r.id=m.release_id
		WHERE m.organization_id=$1 AND m.change_id=$2`, org, change).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return plan, -1, nil
	}
	if err != nil {
		return plan, -1, err
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return plan, -1, err
	}
	for i, step := range plan.Steps {
		if step.ChangeID == change && step.InheritedFrom == "" && plan.OrganizationID == org {
			return plan, i, nil
		}
	}
	return plan, -1, ErrReleaseBlocked
}

func loadJobBindingTx(ctx context.Context, tx pgx.Tx, predicate string, args ...any) (deploymentJobBinding, error) {
	var b deploymentJobBinding
	err := tx.QueryRow(ctx, `SELECT source,job_id,project_id,pipeline_id,organization_id,passport_id,
		COALESCE(release_id,''), COALESCE(deployment_id,0) FROM changeguard_deployment_jobs WHERE `+predicate, args...).
		Scan(&b.Source, &b.JobID, &b.ProjectID, &b.PipelineID, &b.OrganizationID, &b.PassportID, &b.ReleaseID, &b.DeploymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, nil
	}
	return b, err
}

func insertJobBindingTx(ctx context.Context, tx pgx.Tx, b deploymentJobBinding) error {
	_, err := tx.Exec(ctx, `INSERT INTO changeguard_deployment_jobs
		(source,job_id,project_id,pipeline_id,organization_id,passport_id,release_id)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))`, b.Source, b.JobID, b.ProjectID, b.PipelineID, b.OrganizationID, b.PassportID, b.ReleaseID)
	if postgresErrorIsUnique(err) {
		return ErrPassportReplay
	}
	return err
}

func (b *postgresBackend) recordGitLabDeployment(ctx context.Context, event model.GitLabDeployment) (bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = lockReleaseOrganization(ctx, tx, event.OrganizationID); err != nil {
		return false, err
	}
	binding, err := loadJobBindingTx(ctx, tx, "source=$1 AND job_id=$2 AND organization_id=$3", event.Source, event.JobID, event.OrganizationID)
	if err != nil {
		return false, err
	}
	if !binding.matchesEvent(event) || binding.ReleaseID == "" {
		return false, ErrReleaseEvidence
	}
	plans, err := releasePlansTx(ctx, tx, binding.OrganizationID, binding.ReleaseID)
	if err != nil {
		return false, err
	}
	if len(plans) != 1 {
		return false, ErrReleaseEvidence
	}
	plan := plans[0]
	var ids []string
	for _, step := range plan.Steps {
		if step.Consumption != nil && step.Consumption.PassportID == binding.PassportID {
			ids = append(ids, step.ChangeID)
			break
		}
	}
	changes, err := releaseChangesTx(ctx, tx, binding.OrganizationID, ids, releaseChangeShare)
	if err != nil {
		return false, err
	}
	updated, changed, err := (releaseSnapshot{data: state{Changes: changes}}).recordDeployment(plan, binding, event)
	if err != nil || !changed {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE changeguard_deployment_jobs SET deployment_id=$3
		WHERE source=$1 AND job_id=$2 AND deployment_id IS NULL`, binding.Source, binding.JobID, event.DeploymentID)
	if postgresErrorIsUnique(err) {
		return false, ErrReleaseEvidence
	}
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrReleaseEvidence
	}
	if err := updateReleaseTx(ctx, tx, updated); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
