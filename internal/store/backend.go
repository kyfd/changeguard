package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConcurrentWrite = errors.New("concurrent state update")

type stateBackend interface {
	Load(context.Context) ([]byte, int64, error)
	Save(context.Context, []byte, int64) (int64, error)
	Health(context.Context) error
	Close()
	Mode() string
}

type postgresBackend struct {
	pool *pgxpool.Pool
}

const postgresStateMigration = "CREATE TABLE IF NOT EXISTS dbguard_state (" +
	"id smallint PRIMARY KEY CHECK (id = 1)," +
	"version bigint NOT NULL," +
	"payload jsonb NOT NULL," +
	"updated_at timestamptz NOT NULL DEFAULT now()" +
	")"

func NewFromEnvironment(ctx context.Context, filePath string) (*Store, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_STORE_MODE")))
	if mode == "" || mode == "file" {
		return New(filePath)
	}
	if mode != "postgres" {
		return nil, fmt.Errorf("unsupported DBGUARD_STORE_MODE %q", mode)
	}
	dsn := strings.TrimSpace(os.Getenv("DBGUARD_PRIMARY_DSN"))
	if dsn == "" {
		return nil, errors.New("DBGUARD_PRIMARY_DSN is required in postgres mode")
	}
	return NewPostgres(ctx, dsn)
}

func NewPostgres(ctx context.Context, dsn string) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	config.MaxConns = int32(environmentInt("DBGUARD_DB_MAX_CONNS", 20))
	config.MinConns = int32(environmentInt("DBGUARD_DB_MIN_CONNS", 2))
	config.MaxConnLifetime = environmentDuration("DBGUARD_DB_CONN_MAX_LIFETIME", 30*time.Minute)
	config.MaxConnIdleTime = environmentDuration("DBGUARD_DB_CONN_MAX_IDLE", 5*time.Minute)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	backend := &postgresBackend{pool: pool}
	if err := backend.Health(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, postgresStateMigration); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	s := &Store{backend: backend}
	payload, version, err := backend.Load(ctx)
	if errors.Is(err, ErrNotFound) {
		s.data = initialState()
		normalizeState(&s.data)
		if err := s.saveLocked(); err != nil && !errors.Is(err, ErrConcurrentWrite) {
			pool.Close()
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := json.Unmarshal(payload, &s.data); err != nil {
		pool.Close()
		return nil, fmt.Errorf("decode postgres state: %w", err)
	}
	s.persisted = append([]byte(nil), payload...)
	s.version = version
	normalizeState(&s.data)
	if err := s.saveLocked(); err != nil && !errors.Is(err, ErrConcurrentWrite) {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (b *postgresBackend) Load(ctx context.Context) ([]byte, int64, error) {
	var payload []byte
	var version int64
	err := b.pool.QueryRow(ctx, "SELECT payload, version FROM dbguard_state WHERE id = 1").Scan(&payload, &version)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no rows") {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	return payload, version, nil
}

func (b *postgresBackend) Save(ctx context.Context, payload []byte, expectedVersion int64) (int64, error) {
	if expectedVersion == 0 {
		result, err := b.pool.Exec(ctx, "INSERT INTO dbguard_state(id, version, payload, updated_at) VALUES (1, 1, $1, now()) ON CONFLICT (id) DO NOTHING", string(payload))
		if err != nil {
			return 0, err
		}
		if result.RowsAffected() != 1 {
			return 0, ErrConcurrentWrite
		}
		return 1, nil
	}
	result, err := b.pool.Exec(ctx, "UPDATE dbguard_state SET version = version + 1, payload = $1, updated_at = now() WHERE id = 1 AND version = $2", string(payload), expectedVersion)
	if err != nil {
		return 0, err
	}
	if result.RowsAffected() != 1 {
		return 0, ErrConcurrentWrite
	}
	return expectedVersion + 1, nil
}

func (b *postgresBackend) Health(ctx context.Context) error { return b.pool.Ping(ctx) }
func (b *postgresBackend) Close()                           { b.pool.Close() }
func (b *postgresBackend) Mode() string                     { return "postgres" }

func (s *Store) Health(ctx context.Context) error {
	if s.backend == nil {
		return nil
	}
	return s.backend.Health(ctx)
}
func (s *Store) Close() {
	if s.backend != nil {
		s.backend.Close()
	}
}
func (s *Store) Mode() string {
	if s.backend != nil {
		return s.backend.Mode()
	}
	if s.path == "" {
		return "memory"
	}
	return "file"
}

func environmentInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
func environmentDuration(key string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s *Store) StartRefresh(ctx context.Context, interval time.Duration) {
	if s.backend == nil {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				payload, version, err := s.backend.Load(ctx)
				if err != nil {
					continue
				}
				s.mu.Lock()
				if version > s.version {
					var latest state
					if err := json.Unmarshal(payload, &latest); err == nil {
						normalizeState(&latest)
						s.data = latest
						s.version = version
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}
