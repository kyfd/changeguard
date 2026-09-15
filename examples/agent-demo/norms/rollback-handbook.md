# 回滚手册（合成示例）

> 示例文档，用于演示"回滚方案必须可执行"。文档版本：v1.1　适用范围：PostgreSQL 生产库

## 1. 索引类变更的回滚

### 1.1 删除并发创建的索引

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_orders_user_created;
```

注意：

- `DROP INDEX CONCURRENTLY` 同样**不能在事务中执行**；
- 加 `IF EXISTS` 以支持重复执行；
- 删除大索引会带来 IO 压力，建议在低峰窗口进行。

### 1.2 处理残留的无效索引

并发建索引失败会留下 `INVALID` 状态的索引。查询方式：

```sql
SELECT indexrelid::regclass AS index_name, indisvalid
FROM pg_index
WHERE NOT indisvalid;
```

处理方式：先 `DROP INDEX CONCURRENTLY`，再重新创建。

## 2. 回滚的验证标准

| 项目 | 验证方式 |
| --- | --- |
| 索引已删除 | 查询 `pg_indexes` 确认不再存在 |
| 无残留无效索引 | `SELECT ... WHERE NOT indisvalid` 返回空 |
| 查询仍可用 | 慢查询功能验证通过（允许性能回退到变更前水平） |
| 写入未受影响 | 监控写入延迟回到基线 |

## 3. 回滚决策原则

1. **变更未造成故障时不建议立即回滚**：先观察，避免"回滚本身成为二次故障"。
2. **回滚窗口与变更窗口等长**：窗口结束后发现的问题按常规故障流程处理。
3. **回滚不等于恢复**：数据变更类操作的回滚需要独立的数据补偿方案，不能只靠结构回滚。
