CREATE TABLE IF NOT EXISTS dbguard_state (
    id smallint PRIMARY KEY CHECK (id = 1),
    version bigint NOT NULL,
    payload jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
