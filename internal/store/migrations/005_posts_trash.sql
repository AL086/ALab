-- +goose Up
-- 文章也进回收站：删除只打标记，30 天后由清理任务物理删除（级联带走 revisions 和 post_tags）。
-- 和文件侧的口径保持一致，避免"删错了就没了"。
ALTER TABLE posts ADD COLUMN deleted_at TIMESTAMPTZ;

CREATE INDEX idx_posts_deleted ON posts (deleted_at);

-- +goose Down
DROP INDEX IF EXISTS idx_posts_deleted;
ALTER TABLE posts DROP COLUMN IF EXISTS deleted_at;
