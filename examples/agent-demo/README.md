# Agent 演示合成数据

**这些数据全部是手工合成的示例，不包含任何真实业务数据、生产表结构或凭据。**
用途是给"数据库变更准备 Agent"提供可复现的输入与检索素材。

## 演示场景

用户用一句话提需求：

> 给订单表按用户和创建时间查询的场景准备一个索引变更，目标 PostgreSQL，安排在周五晚上。

Agent 需要补齐的信息：应用、环境、表结构、现有索引、实际查询 SQL、明确的发布时间。
其中"表结构"和"现有索引"来自本目录的**快照**，不接生产库实时探索。

## 目录

| 路径 | 内容 | 用途 |
| --- | --- | --- |
| `schema/orders.sql` | 合成订单表结构与现有索引快照 | 表结构工具的数据来源 |
| `queries/slow-orders.sql` | 实际慢查询 SQL | 判断需要哪种索引 |
| `norms/sql-change-standards.md` | SQL 变更规范 | RAG 规范检索 |
| `norms/rollback-handbook.md` | 回滚手册 | RAG 回滚方案依据 |
| `cases/case-001-add-index-online.md` | 成功案例：并发加索引 | 历史案例检索 |
| `cases/case-002-lock-incident.md` | 事故案例：非并发加索引导致写入阻塞 | 历史案例检索 |

## 可用的"正确结论"（供评估集参考答案）

针对上面的场景，期望 Agent 得出的要点：

1. 需要的是 **复合索引** `(user_id, created_at DESC)`，单个 `user_id` 索引无法覆盖排序；
2. 必须使用 **`CREATE INDEX CONCURRENTLY`**，因为表是热表；
3. **`CONCURRENTLY` 不能在事务里执行**，所以变更编排方式与普通 DDL 不同；
4. 回滚是 `DROP INDEX CONCURRENTLY`，且必须验证索引确实被使用后再决定保留；
5. 规范要求设置 `lock_timeout` 与 `statement_timeout`，避免长时间持锁；
6. 必须给出明确的带时区时间，而不是"周五晚上"。

> 这 6 条是**合成数据下的参考答案**，用于评估集打分，不代表任何生产环境结论。
