package store

import (
	"fmt"
	"io"
)

// WritePrometheus exposes aggregate pool health without query, tenant, or DSN labels.
func (s *Store) WritePrometheus(writer io.Writer) {
	backend, ok := s.backend.(*postgresBackend)
	if !ok || backend.pool == nil {
		return
	}
	stats := backend.pool.Stat()
	_, _ = fmt.Fprintln(writer, "# HELP dbguard_db_pool_acquired_connections Currently acquired PostgreSQL connections.")
	_, _ = fmt.Fprintln(writer, "# TYPE dbguard_db_pool_acquired_connections gauge")
	_, _ = fmt.Fprintf(writer, "dbguard_db_pool_acquired_connections %d\n", stats.AcquiredConns())
	_, _ = fmt.Fprintln(writer, "# HELP dbguard_db_pool_idle_connections Currently idle PostgreSQL connections.")
	_, _ = fmt.Fprintln(writer, "# TYPE dbguard_db_pool_idle_connections gauge")
	_, _ = fmt.Fprintf(writer, "dbguard_db_pool_idle_connections %d\n", stats.IdleConns())
	_, _ = fmt.Fprintln(writer, "# HELP dbguard_db_pool_max_connections Configured PostgreSQL connection limit.")
	_, _ = fmt.Fprintln(writer, "# TYPE dbguard_db_pool_max_connections gauge")
	_, _ = fmt.Fprintf(writer, "dbguard_db_pool_max_connections %d\n", stats.MaxConns())
	_, _ = fmt.Fprintln(writer, "# HELP dbguard_db_pool_empty_acquire_total PostgreSQL acquisitions that waited for a connection.")
	_, _ = fmt.Fprintln(writer, "# TYPE dbguard_db_pool_empty_acquire_total counter")
	_, _ = fmt.Fprintf(writer, "dbguard_db_pool_empty_acquire_total %d\n", stats.EmptyAcquireCount())
	_, _ = fmt.Fprintln(writer, "# HELP dbguard_db_pool_acquire_duration_seconds_total Total time blocked acquiring PostgreSQL connections.")
	_, _ = fmt.Fprintln(writer, "# TYPE dbguard_db_pool_acquire_duration_seconds_total counter")
	_, _ = fmt.Fprintf(writer, "dbguard_db_pool_acquire_duration_seconds_total %.6f\n", stats.AcquireDuration().Seconds())
}
