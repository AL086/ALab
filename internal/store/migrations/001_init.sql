-- +goose Up
-- 第一批只建内容侧的表。文件仓库（file_nodes/file_versions/objects）留到
-- 002 迁移，等引用计数口径定死再建，避免建了又改。

-- 用户（v1 单用户，role 预留多用户）
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,                      -- argon2id
    email         TEXT UNIQUE,
    role          TEXT NOT NULL DEFAULT 'owner',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 分类（树形）
CREATE TABLE categories (
    id         BIGSERIAL PRIMARY KEY,
    parent_id  BIGINT REFERENCES categories(id) ON DELETE SET NULL,
    name       TEXT NOT NULL,
    slug       TEXT UNIQUE NOT NULL,
    sort       INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 标签（扁平）
CREATE TABLE tags (
    id   BIGSERIAL PRIMARY KEY,
    name TEXT UNIQUE NOT NULL,
    slug TEXT UNIQUE NOT NULL
);

-- 文章：正文只存 Markdown，渲染结果后续可加缓存字段
CREATE TABLE posts (
    id           BIGSERIAL PRIMARY KEY,
    author_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    slug         TEXT UNIQUE NOT NULL,
    summary      TEXT,
    content_md   TEXT NOT NULL,
    cover_url    TEXT,
    category_id  BIGINT REFERENCES categories(id) ON DELETE SET NULL,
    status       TEXT NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'published', 'archived')),
    visibility   TEXT NOT NULL DEFAULT 'public'
                 CHECK (visibility IN ('public', 'private', 'unlisted')),
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_posts_status ON posts (status, published_at DESC);
CREATE INDEX idx_posts_category ON posts (category_id);

-- 文章版本历史：发布时产生一条，回滚也是新增版本
CREATE TABLE post_revisions (
    id         BIGSERIAL PRIMARY KEY,
    post_id    BIGINT NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    version_no INT NOT NULL,
    title      TEXT NOT NULL,
    content_md TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    note       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (post_id, version_no)
);

CREATE INDEX idx_post_revisions_post ON post_revisions (post_id, version_no DESC);

-- +goose Down
DROP TABLE IF EXISTS post_revisions;
DROP TABLE IF EXISTS posts;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS users;
