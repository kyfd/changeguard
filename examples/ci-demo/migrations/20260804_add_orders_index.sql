CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_created_at
    ON orders (created_at);
