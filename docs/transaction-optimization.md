# 数据库事务优化治理

ChangeGuard 把常见的 PostgreSQL 事务优化实践固化成**确定性规则 + 影子演练证据 + Evidence Navigator 整改建议**，而不是只靠人工经验。

## 规则（静态检查）

| 规则码 | 意图 | 默认级别 |
| --- | --- | --- |
| `UNBATCHED_LARGE_DML` | 条件 UPDATE/DELETE 缺少 LIMIT/分批边界，易形成大事务 | Medium |
| `FK_WITHOUT_NOT_VALID` | 外键未用 `NOT VALID` 分阶段，创建时整表校验持锁 | High / 阻断 |
| `SELECT_FOR_UPDATE_UNBOUNDED` | `FOR UPDATE` 无 LIMIT/主键范围，可能锁过量行 | Medium |
| `HEAVY_DDL_REWRITE` | `VACUUM FULL` / `CLUSTER` / `REINDEX` / `ALTER TYPE` 等重写型 DDL | High / 阻断 |
| `MISSING_LOCK_TIMEOUT` | 高锁风险变更未声明 `lock_timeout`/`statement_timeout` | Medium |
| `MIXED_DDL_DML_TRANSACTION` | 同一变更混用 DDL 与大批量 DML | Medium |
| `INDEX_NOT_CONCURRENT` | 索引创建未使用 `CONCURRENTLY` | Medium |
| `TRANSACTION_CONTROL` | 禁止用户 SQL 内 `BEGIN/COMMIT/ROLLBACK` 等，避免绕过影子事务 | High / 阻断 |

安全写法示例：

```sql
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
UPDATE member_points
SET status = 'EXPIRED'
WHERE expires_at < NOW() - INTERVAL '7 days'
  AND status = 'ACTIVE'
LIMIT 10000;
```

```sql
SET LOCAL lock_timeout = '2s';
ALTER TABLE inventory_reservation
  ADD CONSTRAINT fk_reservation_sku
  FOREIGN KEY (sku_id) REFERENCES sku(id) NOT VALID;
-- 低峰期再：
-- ALTER TABLE inventory_reservation VALIDATE CONSTRAINT fk_reservation_sku;
```

## 影子演练（运行时）

PostgreSQL 影子事务默认：

- `SET LOCAL lock_timeout = '2s'`
- `SET LOCAL statement_timeout = '45s'`
- `pg_advisory_xact_lock` 串行化同 DSN 演练
- `CONCURRENTLY` 在影子事务内规范化为普通索引 DDL（生产语句仍保留原意图）
- 采集：最慢语句耗时、事务内持锁数、缓冲命中增量、回滚同事务验证
- 失败分类：`LOCK_TIMEOUT` / `STATEMENT_TIMEOUT` / `DEADLOCK` / `SERIALIZATION_FAILURE`

## Evidence Navigator（变更证据助手）

变更证据助手在命中上述规则时，会按问题执行最小范围的只读查询，输出事务优化建议（分批、NOT VALID、CONCURRENTLY、超时声明、拆单等），并引用真实 finding / 演练证据。它不直接修改状态，也没有审批、签发通行证、部署、回滚或升级工具。

## 演示变更

- `chg_demo_points_archive`：正确的分批 DML + 超时声明
- `chg_demo_orders_unbatched`：缺少 LIMIT / lock_timeout
- `chg_demo_inventory_fk`：外键未 `NOT VALID` + 模拟 lock timeout
- `chg_demo_vacuum_full`：重写型 DDL 阻断
