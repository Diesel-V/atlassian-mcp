CREATE TABLE oauth_tokens (
    username      TEXT        NOT NULL,
    provider      TEXT        NOT NULL,
    access_token  TEXT        NOT NULL, -- AES-GCM, base64
    refresh_token TEXT        NOT NULL, -- AES-GCM, base64
    expiry        TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (username, provider)
);

CREATE TABLE api_keys (
    id           TEXT        PRIMARY KEY,
    username     TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    secret_hash  TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);
CREATE INDEX api_keys_username_idx ON api_keys (username);

CREATE TABLE agents (
    client_id   TEXT        PRIMARY KEY,
    name        TEXT        NOT NULL,
    owner       TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL,
    credentials TEXT        NOT NULL, -- AES-GCM over JSON {provider: pat}, base64
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
    id           BIGSERIAL   PRIMARY KEY,
    ts           TIMESTAMPTZ NOT NULL,
    subject_kind TEXT        NOT NULL,
    principal    TEXT        NOT NULL,
    key_id       TEXT        NOT NULL DEFAULT '',
    tool         TEXT        NOT NULL,
    params       JSONB       NOT NULL,
    results      INTEGER     NOT NULL,
    status       TEXT        NOT NULL,
    duration_ms  BIGINT      NOT NULL,
    error        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_principal_ts_idx ON audit_log (principal, ts);
CREATE INDEX audit_log_ts_idx ON audit_log (ts);
