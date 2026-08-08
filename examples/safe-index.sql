-- 执行 SQL
CREATE INDEX CONCURRENTLY idx_orders_created_status
ON orders(created_at, status);

-- 回滚 SQL
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_created_status;
