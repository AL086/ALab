-- +goose Up
-- 文章封面：存的是可直接放进 <img src> 的地址（一般是 /raw/<id>，也允许外链 https://…）。
-- 不存 file_nodes 外键：封面只是展示用，文件被回收站带走时封面自然失效即可，不该拦着删文件。
-- 留空 = 没有封面，列表里就不给缩略图位，不做占位图。
-- 用 IF NOT EXISTS：这列之前手工加过一次，迁移要能在已有列上安全落地。
ALTER TABLE posts ADD COLUMN IF NOT EXISTS cover_url TEXT;

-- +goose Down
ALTER TABLE posts DROP COLUMN IF EXISTS cover_url;
