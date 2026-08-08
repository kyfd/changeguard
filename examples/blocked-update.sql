-- 该语句会被确定性规则直接阻断
UPDATE orders SET status = 'archived';

-- 正确方式应明确影响范围
UPDATE orders
SET status = 'archived'
WHERE created_at < now() - interval '2 years'
  AND status = 'closed';
