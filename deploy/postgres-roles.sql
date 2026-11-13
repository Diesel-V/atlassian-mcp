-- Least-privilege roles for atlassian-mcp. Run once as a DBA, connected to the
-- service database. Set the passwords separately and keep them in Vault:
--   ALTER ROLE amcp_migrator PASSWORD '...';  ALTER ROLE amcp_app PASSWORD '...';
--
-- amcp_migrator owns the schema and is used only by "atlassian-mcp migrate"
--               (MIGRATE_DATABASE_URL), e.g. from the CI deploy job.
-- amcp_app      is the runtime role (DATABASE_URL, MIGRATE_ON_START=false):
--               reads and writes rows of the service tables, cannot change the schema.
--
-- amcp_app keeps DELETE because audit retention (AUDIT_RETENTION) deletes old
-- entries and tokens are deleted on disconnect/offboarding. If the audit log
-- must be append-only, set AUDIT_RETENTION=0, run retention as a DBA job and
-- REVOKE UPDATE, DELETE ON audit_log FROM amcp_app after the first migration.

CREATE ROLE amcp_migrator LOGIN;
CREATE ROLE amcp_app LOGIN;

DO $$
BEGIN
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO amcp_migrator, amcp_app', current_database());
END $$;

GRANT USAGE, CREATE ON SCHEMA public TO amcp_migrator;
GRANT USAGE ON SCHEMA public TO amcp_app;

-- Tables and sequences created by the migrator are usable by the runtime role.
ALTER DEFAULT PRIVILEGES FOR ROLE amcp_migrator IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO amcp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE amcp_migrator IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO amcp_app;
