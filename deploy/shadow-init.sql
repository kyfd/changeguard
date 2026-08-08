DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dbguard_runner') THEN
        CREATE ROLE dbguard_runner LOGIN PASSWORD 'dbguard_runner'
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION;
    END IF;
END
$$;

GRANT CONNECT ON DATABASE shadow TO dbguard_runner;
ALTER SCHEMA public OWNER TO dbguard_runner;
GRANT USAGE, CREATE ON SCHEMA public TO dbguard_runner;

SET ROLE dbguard_runner;

CREATE TABLE IF NOT EXISTS orders (
    id bigserial PRIMARY KEY,
    request_id varchar(64),
    status varchar(24) NOT NULL DEFAULT 'created',
    created_at timestamptz NOT NULL DEFAULT now(),
    note varchar(255)
);

CREATE TABLE IF NOT EXISTS inventory_logs (
    id bigserial PRIMARY KEY,
    sku_id bigint NOT NULL,
    biz_no varchar(64) NOT NULL,
    quantity integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO orders(request_id, status)
SELECT 'seed-' || value, CASE WHEN value % 3 = 0 THEN 'paid' ELSE 'created' END
FROM generate_series(1, 10000) AS value
ON CONFLICT DO NOTHING;

RESET ROLE;
