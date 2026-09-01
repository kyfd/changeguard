# PostgreSQL backup and isolated restore

The production overlay installs a daily PostgreSQL custom-format dump job. Before applying it, create `dbguard-backup-secrets` outside Git (External Secrets, Sealed Secrets, or your secret manager) with `DBGUARD_BACKUP_DSN`. Use a dedicated database role with `CONNECT` and read-only access to the ChangeGuard database. The example secret is a schema-only placeholder and is not included by Kustomize.

The default PVC is cluster-local operational protection, not the only durable copy. Configure the storage class or an external replication/export process so encrypted backup objects are retained in a separate failure domain. Restrict backup-volume access, monitor CronJob failures, and periodically verify checksums and restoration.

## Isolated restore drill

Never restore over the production database as a test.

1. Select a dump and verify it against the adjacent SHA-256 file.
2. Provision an isolated PostgreSQL instance in a dedicated namespace or account with no route to production. Deny application and Internet ingress; permit access only from the restore job/operator workstation.
3. Create a fresh empty database and a short-lived restore credential from the secret manager. Do not reuse the production application or backup credential.
4. Restore with the matching PostgreSQL major-version client:

   ```sh
   createdb --dbname="$ISOLATED_ADMIN_DSN" dbguard_restore
   pg_restore --dbname="$ISOLATED_RESTORE_DSN" --no-owner --no-acl --exit-on-error /backup/dbguard-YYYYMMDDTHHMMSSZ.dump
   ```

5. Run integrity checks: connect with a read-only account, confirm expected schema/table counts, inspect `dbguard_state`, and start a temporary ChangeGuard instance with workers disabled (`DBGUARD_WORKERS=0`) against only the isolated database. Exercise `/health/ready` and a representative read-only API flow.
6. Record dump timestamp, checksum, PostgreSQL versions, restore duration, validation results, RPO/RTO, and operator. Treat restored data as production-sensitive.
7. Delete the temporary application, credentials, database, namespace, and any copied dump after the drill's approved retention window. Confirm deletion through the infrastructure and secret-manager audit logs.

A successful `pg_restore --list` in the CronJob detects archive corruption but does not replace a scheduled isolated restore drill.
