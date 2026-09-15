-- 合成慢查询（示例数据，非生产）
-- 场景：用户订单列表页，按用户过滤 + 时间倒序分页
-- 现状：走 idx_orders_user_id，然后对结果排序并丢弃；P99 约 1.8s

SELECT id, user_id, status, amount_cents, currency, created_at
FROM orders
WHERE user_id = $1
  AND created_at >= $2
  AND created_at <  $3
ORDER BY created_at DESC
LIMIT 20;

-- 期望的索引形态
--   CREATE INDEX CONCURRENTLY idx_orders_user_created
--     ON orders (user_id, created_at DESC);
--
-- 依据：
-- 1. 等值条件 user_id 在前，范围+排序条件 created_at 在后，符合 B-tree 最左前缀；
-- 2. DESC 与查询排序方向一致，避免额外 Sort 节点；
-- 3. 现有 idx_orders_user_id 只覆盖过滤，排序仍需回表 + 排序。
