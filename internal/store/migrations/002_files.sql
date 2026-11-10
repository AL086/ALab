-- +goose Up
-- 文件仓库。引用计数口径在这里定死：
--   objects.ref_count == file_versions 里该 sha256 的行数
--   file_nodes.storage_key 只是"当前版本"的冗余指针，不参与计数
-- 因此"替换版本"和"回滚版本"都不改 ref_count（版本行本来就在），
-- 只有"彻底删除某个版本"才 ref_count - 1。

-- 内容寻址对象表
CREATE TABLE objects (
    sha256     TEXT PRIMARY KEY,
    size       BIGINT NOT NULL,
    ref_count  INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 目录 / 文件统一节点
CREATE TABLE file_nodes (
    id          BIGSERIAL PRIMARY KEY,
    parent_id   BIGINT REFERENCES file_nodes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL CHECK (type IN ('dir', 'file')),
    size        BIGINT NOT NULL DEFAULT 0,
    sha256      TEXT,
    mime        TEXT,
    storage_key TEXT,
    category_id BIGINT REFERENCES categories(id) ON DELETE SET NULL,
    visibility  TEXT NOT NULL DEFAULT 'private'
                CHECK (visibility IN ('public', 'private', 'unlisted')),
    exec_public BOOLEAN NOT NULL DEFAULT FALSE,
    sort        INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

-- 同目录下不允许重名，但有两个坑必须一次处理掉：
-- 1) parent_id 为 NULL 时 PostgreSQL 默认不参与比较，根目录会出现无数同名节点
--    → 用 NULLS NOT DISTINCT
-- 2) 软删除的行还留在表里，如果唯一约束覆盖全部行，删掉的文件名就再也用不了
--    → 用部分索引 WHERE deleted_at IS NULL，只约束"活着的"节点
CREATE UNIQUE INDEX idx_files_unique_name
    ON file_nodes (parent_id, name, type) NULLS NOT DISTINCT
    WHERE deleted_at IS NULL;

CREATE INDEX idx_files_parent ON file_nodes (parent_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_files_sha ON file_nodes (sha256);

-- 文件版本：替换/覆盖都新增一行，回滚只是把 file_nodes 的指针指回旧版本
CREATE TABLE file_versions (
    id          BIGSERIAL PRIMARY KEY,
    file_id     BIGINT NOT NULL REFERENCES file_nodes(id) ON DELETE CASCADE,
    version_no  INT NOT NULL,
    sha256      TEXT NOT NULL,
    size        BIGINT NOT NULL,
    storage_key TEXT NOT NULL,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (file_id, version_no)
);

-- 下载 / 直调审计（硬删文件时不能让外键挡住，所以是 SET NULL）
CREATE TABLE download_logs (
    id         BIGSERIAL PRIMARY KEY,
    file_id    BIGINT REFERENCES file_nodes(id) ON DELETE SET NULL,
    ip         INET,
    user_agent TEXT,
    kind       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_download_logs_file ON download_logs (file_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS download_logs;
DROP TABLE IF EXISTS file_versions;
DROP TABLE IF EXISTS file_nodes;
DROP TABLE IF EXISTS objects;
