-- IDs of pages and issues a call found or opened, for investigations
-- ("who saw page 12345"). Content is never stored.
ALTER TABLE audit_log ADD COLUMN objects text[] NOT NULL DEFAULT '{}';
CREATE INDEX audit_log_objects_idx ON audit_log USING gin (objects);
