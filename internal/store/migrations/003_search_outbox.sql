-- +goose Up
-- 搜索同步用事务性 outbox：业务写库和"待同步"记录在同一个事务里提交，
-- 后台任务再去推 Meilisearch。服务器没有 Redis，这是文档里两条路线中唯一可行的。
CREATE TABLE search_outbox (
    id          BIGSERIAL PRIMARY KEY,
    entity_type TEXT NOT NULL DEFAULT 'post'
                CHECK (entity_type IN ('post')),
    entity_id   BIGINT NOT NULL,
    op          TEXT NOT NULL CHECK (op IN ('upsert', 'delete')),
    attempts    INT NOT NULL DEFAULT 0,
    last_error  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);

-- 只给未处理的行建索引：表会一直增长，但待处理集合始终很小
CREATE INDEX idx_outbox_pending ON search_outbox (id) WHERE processed_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS search_outbox;
