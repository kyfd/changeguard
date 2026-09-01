CREATE TABLE e2e_config_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    config_key text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);
