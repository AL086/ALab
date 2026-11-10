-- +goose Up
-- 文章与标签的多对多。posts 表只有 category_id（单分类），标签必须单独建联表。
CREATE TABLE post_tags (
    post_id BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    tag_id  BIGINT NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
    PRIMARY KEY (post_id, tag_id)
);

-- 按标签反查文章时走这个索引
CREATE INDEX idx_post_tags_tag ON post_tags (tag_id);

-- 分片上传会话：一次上传的中间状态。
-- parts 用 JSONB 存 [{part, etag, size}]，避免为几个分片再建一张表。
CREATE TABLE upload_sessions (
    id           TEXT PRIMARY KEY,               -- 应用侧生成，给客户端当 uploadId
    s3_upload_id TEXT,                           -- MinIO 的 multipart upload id，秒传时为 NULL
    object_key   TEXT NOT NULL,                  -- 临时 key：uploads/<id>/blob
    parent_id    BIGINT REFERENCES file_nodes(id) ON DELETE SET NULL,
    name         TEXT NOT NULL,
    size         BIGINT NOT NULL CHECK (size > 0),
    part_size    BIGINT NOT NULL,
    total_parts  INT NOT NULL,
    mime         TEXT,
    parts        JSONB NOT NULL DEFAULT '[]'::jsonb,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'completed', 'aborted')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 只给未完成的会话建索引，后台清理任务扫它
CREATE INDEX idx_upload_sessions_pending ON upload_sessions (created_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS post_tags;
