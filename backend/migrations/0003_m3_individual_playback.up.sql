CREATE TABLE session_state (
    user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    queue jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(queue) = 'array'),
    queue_index integer NOT NULL DEFAULT 0 CHECK (queue_index >= 0),
    current_media_id text,
    position_ms integer NOT NULL DEFAULT 0 CHECK (position_ms BETWEEN 0 AND 86400000),
    volume numeric(4,3) NOT NULL DEFAULT 1.0 CHECK (volume BETWEEN 0 AND 1),
    shuffle boolean NOT NULL DEFAULT false,
    playback_status text NOT NULL DEFAULT 'paused' CHECK (playback_status IN ('paused', 'playing')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (jsonb_array_length(queue) <= 200),
    CHECK (
        (jsonb_array_length(queue) = 0 AND queue_index = 0 AND current_media_id IS NULL
            AND position_ms = 0 AND playback_status = 'paused')
        OR (jsonb_array_length(queue) > 0 AND queue_index < jsonb_array_length(queue)
            AND current_media_id IS NOT NULL AND btrim(current_media_id) <> ''
            AND queue -> queue_index ->> 'mediaId' IS NOT NULL
            AND current_media_id = queue -> queue_index ->> 'mediaId')
    )
);

INSERT INTO session_state(user_id) SELECT id FROM users;

CREATE FUNCTION create_individual_session() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO session_state(user_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_create_individual_session
    AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION create_individual_session();
