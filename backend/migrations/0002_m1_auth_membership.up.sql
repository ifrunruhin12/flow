CREATE TABLE credential_key_versions (
    version integer PRIMARY KEY,
    state text NOT NULL CHECK (state IN ('active', 'decrypt_only', 'retired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz,
    reencrypted_at timestamptz
);

CREATE UNIQUE INDEX credential_key_versions_one_active
    ON credential_key_versions ((state))
    WHERE state = 'active';

CREATE TABLE users (
    id bigserial PRIMARY KEY,
    username text NOT NULL,
    display_name text NOT NULL,
    email text,
    password_hash text NOT NULL,
    must_change_password boolean NOT NULL DEFAULT false,
    role text NOT NULL CHECK (role IN ('admin', 'member')),
    status text NOT NULL CHECK (status IN ('active', 'suspended', 'deleted')),
    provisioning_state text NOT NULL CHECK (
        provisioning_state IN ('not_started', 'credential_ready', 'creating_nd_user', 'linking', 'complete', 'failed')
    ),
    provisioning_failed_step text,
    navidrome_username text,
    navidrome_user_id text,
    navidrome_password_enc bytea,
    navidrome_cred_key_version integer REFERENCES credential_key_versions(version),
    is_navidrome_admin boolean NOT NULL DEFAULT false,
    navidrome_cleanup_state text NOT NULL DEFAULT 'not_requested' CHECK (
        navidrome_cleanup_state IN ('not_requested', 'pending', 'complete', 'failed')
    ),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_active_at timestamptz,
    CHECK (btrim(username) <> ''),
    CHECK (btrim(display_name) <> ''),
    CHECK ((provisioning_state = 'failed') = (provisioning_failed_step IS NOT NULL)),
    CHECK (provisioning_state <> 'complete' OR (navidrome_username IS NOT NULL AND navidrome_user_id IS NOT NULL)),
    CHECK (
        provisioning_state NOT IN ('credential_ready', 'creating_nd_user', 'linking', 'complete')
        OR (navidrome_username IS NOT NULL AND navidrome_password_enc IS NOT NULL AND navidrome_cred_key_version IS NOT NULL)
    ),
    CHECK (NOT is_navidrome_admin OR role = 'admin'),
    CHECK (NOT is_navidrome_admin OR provisioning_state = 'complete'),
    CHECK (status = 'deleted' OR navidrome_cleanup_state = 'not_requested')
);

CREATE UNIQUE INDEX users_username_unique_ci ON users (lower(username));
CREATE UNIQUE INDEX users_email_unique_ci ON users (lower(email)) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_one_admin ON users ((role)) WHERE role = 'admin';
CREATE INDEX users_lifecycle ON users (status, role, provisioning_state);

CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz,
    user_agent_hint text,
    CHECK (octet_length(token_hash) = 32),
    CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE membership_requests (
    id bigserial PRIMARY KEY,
    display_name text NOT NULL,
    email text NOT NULL,
    desired_username text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    reject_note text,
    admin_note text,
    user_id bigint REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    decided_by bigint REFERENCES users(id),
    CHECK (
        (status = 'pending' AND decided_at IS NULL AND decided_by IS NULL AND user_id IS NULL)
        OR (status = 'approved' AND decided_at IS NOT NULL AND decided_by IS NOT NULL AND user_id IS NOT NULL)
        OR (status = 'rejected' AND decided_at IS NOT NULL AND decided_by IS NOT NULL AND user_id IS NULL)
    )
);

CREATE UNIQUE INDEX membership_requests_one_pending_email
    ON membership_requests (lower(email))
    WHERE status = 'pending';
CREATE INDEX membership_requests_status_created ON membership_requests (status, created_at);

CREATE TABLE audit_log (
    id bigserial PRIMARY KEY,
    correlation_id text NOT NULL,
    actor_user_id bigint REFERENCES users(id),
    action text NOT NULL,
    entity text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('ok', 'error', 'in-progress')),
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_correlation ON audit_log (correlation_id);
CREATE INDEX audit_log_created ON audit_log (created_at DESC);
