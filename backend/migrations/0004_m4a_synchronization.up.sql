CREATE TABLE settings (
    id smallint PRIMARY KEY CHECK (id = 1),
    mode text NOT NULL DEFAULT 'individual' CHECK (mode IN ('individual', 'synchronized')),
    sync_drift_threshold_ms integer NOT NULL DEFAULT 2000 CHECK (sync_drift_threshold_ms > 0),
    sync_heartbeat_interval_ms integer NOT NULL DEFAULT 5000 CHECK (sync_heartbeat_interval_ms >= 2000),
    dj_grace_period_s integer NOT NULL DEFAULT 90 CHECK (dj_grace_period_s = 90),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO settings(id) VALUES (1);

CREATE TABLE shared_sessions (
    id bigserial PRIMARY KEY,
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    event_seq bigint NOT NULL DEFAULT 1 CHECK (event_seq >= 1),
    state text NOT NULL CHECK (state IN ('active', 'paused', 'ended')),
    dj_user_id bigint NOT NULL REFERENCES users(id),
    dj_lease_generation bigint NOT NULL DEFAULT 0 CHECK (dj_lease_generation >= 0),
    dj_lease_token_hash bytea,
    dj_connection_id uuid,
    dj_connected boolean NOT NULL DEFAULT false,
    dj_disconnected_at timestamptz,
    dj_grace_deadline timestamptz,
    current_media_id text NOT NULL CHECK (btrim(current_media_id) <> ''),
    position_ms bigint NOT NULL DEFAULT 0 CHECK (position_ms BETWEEN 0 AND 86400000),
    ref_ts timestamptz NOT NULL,
    queue jsonb NOT NULL CHECK (jsonb_typeof(queue) = 'array' AND jsonb_array_length(queue) BETWEEN 1 AND 200),
    queue_index integer NOT NULL CHECK (queue_index >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    end_reason text CHECK (end_reason IN ('admin', 'platform-restart', 'grace-expiry')),
    CHECK (queue_index < jsonb_array_length(queue)),
    CHECK (current_media_id = queue ->> queue_index),
    CHECK (dj_lease_token_hash IS NULL OR octet_length(dj_lease_token_hash) = 32),
    CHECK (NOT dj_connected OR (dj_connection_id IS NOT NULL AND dj_lease_token_hash IS NOT NULL)),
    CHECK ((dj_disconnected_at IS NULL) = (dj_grace_deadline IS NULL)),
    CHECK (dj_grace_deadline IS NULL OR (state = 'paused' AND NOT dj_connected)),
    CHECK ((state = 'ended') = (ended_at IS NOT NULL)),
    CHECK ((state = 'ended') = (end_reason IS NOT NULL)),
    CHECK (state <> 'ended' OR (NOT dj_connected AND dj_grace_deadline IS NULL))
);

CREATE UNIQUE INDEX shared_sessions_one_open
    ON shared_sessions ((state <> 'ended')) WHERE state <> 'ended';
CREATE INDEX shared_sessions_state ON shared_sessions(state);

CREATE TABLE sync_events (
    id bigserial PRIMARY KEY,
    shared_session_id bigint NOT NULL REFERENCES shared_sessions(id) ON DELETE CASCADE,
    event_seq bigint NOT NULL CHECK (event_seq >= 1),
    version bigint NOT NULL CHECK (version >= 0),
    type text NOT NULL CHECK (type IN (
        'session-started', 'state-changed', 'dj-disconnected', 'dj-reconnected', 'session-ended'
    )),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    server_ts timestamptz NOT NULL,
    UNIQUE(shared_session_id, event_seq)
);

CREATE INDEX sync_events_session_order
    ON sync_events(shared_session_id, event_seq DESC);
