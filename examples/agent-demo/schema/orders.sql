-- 合成表结构快照（示例数据，非生产）
-- 用途：供变更准备 Agent 了解表结构与现有索引；本文件是快照，不是实时查询结果。
-- 快照信息：captured_at=2026-09-14T02:00:00Z  row_estimate=48000000  table_size=23GB

CREATE TABLE orders (
    id              bigserial    PRIMARY KEY,
    user_id         bigint       NOT NULL,
    status          text         NOT NULL,
    amount_cents    bigint       NOT NULL,
    currency        char(3)      NOT NULL DEFAULT 'CNY',
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    remark          text
);

-- 现有索引（快照）
-- 1) 主键
--    orders_pkey            PRIMARY KEY (id)
-- 2) 单列索引：只覆盖 user_id 过滤，无法覆盖 created_at 排序
--    idx_orders_user_id     (user_id)
-- 3) 单列索引：只覆盖时间范围，选择性差
--    idx_orders_created_at  (created_at)
-- 4) 状态过滤
--    idx_orders_status      (status)

-- 表特征（用于生成变更建议）
-- - 日均写入约 120 万行，属于热表
-- - 长事务较少，但存在分钟级批处理任务
-- - 曾因非并发建索引导致写入阻塞（见 cases/case-002-lock-incident.md）
