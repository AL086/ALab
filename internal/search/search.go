// Package search 把文章同步到 Meilisearch，并提供查询入口。
//
// 这里刻意不用官方 SDK，直接用 REST：服务器上是 1.4.0（2023 年的版本），
// 官方 Go SDK 对服务端版本有要求，换成裸 HTTP 之后 1.4 和以后的版本都能用，
// 升级服务端不用改代码。
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"blog-server/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Client 是 Meilisearch 的最小 REST 封装，只做项目真正用到的几个动作。
type Client struct {
	base  string
	key   string
	index string
	http  *http.Client
}

func New(cfg *config.Config) *Client {
	return &Client{
		base:  strings.TrimRight(cfg.MeiliHost, "/"),
		key:   cfg.MeiliAPIKey,
		index: cfg.MeiliIndex,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meilisearch %s %s 返回 %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// Ensure 建索引并设置字段，幂等，每次启动跑一遍。
func (c *Client) Ensure(ctx context.Context) error {
	if _, err := c.do(ctx, http.MethodPost, "/indexes", map[string]any{
		"uid":        c.index,
		"primaryKey": "id",
	}); err != nil {
		// 已存在时 Meilisearch 返回 400 index_already_exists，属正常
		if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "400") {
			return err
		}
	}

	settings := map[string]any{
		"searchableAttributes": []string{"title", "summary", "content", "slug"},
		"filterableAttributes": []string{"status", "visibility", "categoryId"},
		"sortableAttributes":   []string{"publishedAt", "updatedAt"},
		"displayedAttributes":  []string{"id", "title", "slug", "summary", "content", "status", "visibility", "publishedAt", "coverUrl"},
	}
	if _, err := c.do(ctx, http.MethodPatch, "/indexes/"+url.PathEscape(c.index)+"/settings", settings); err != nil {
		return err
	}
	return nil
}

// Upsert 覆盖写文档（同 id 会替换）。
func (c *Client) Upsert(ctx context.Context, doc map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "/indexes/"+url.PathEscape(c.index)+"/documents",
		[]map[string]any{doc})
	return err
}

// DeleteDoc 删除文档。
func (c *Client) DeleteDoc(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete,
		"/indexes/"+url.PathEscape(c.index)+"/documents/"+url.PathEscape(id), nil)
	return err
}

// Search 查询。filter 为空表示不过滤。
func (c *Client) Search(ctx context.Context, q string, limit, offset int, filter string) (map[string]any, error) {
	body := map[string]any{
		"q":                     q,
		"limit":                 limit,
		"offset":                offset,
		"attributesToHighlight": []string{"title", "summary"},
	}
	if filter != "" {
		body["filter"] = filter
	}
	raw, err := c.do(ctx, http.MethodPost, "/indexes/"+url.PathEscape(c.index)+"/search", body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- outbox 同步 ----------

// Worker 轮询 search_outbox，把变更推到 Meilisearch。
// 单实例部署，所以不需要分布式锁；真要扩容时这里换成 SELECT ... FOR UPDATE SKIP LOCKED。
type Worker struct {
	Pool *pgxpool.Pool
	Cli  *Client
}

// Run 阻塞运行，直到 ctx 取消。启动时先补一轮，之后每 2 秒一次。
func (w *Worker) Run(ctx context.Context) {
	w.process(ctx)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.process(ctx)
		}
	}
}

func (w *Worker) process(ctx context.Context) {
	for i := 0; i < 5; i++ { // 每轮最多 5 批，避免长时间占用
		n, err := w.batch(ctx)
		if err != nil {
			log.Printf("[search] 同步失败: %v", err)
			return
		}
		if n == 0 {
			return
		}
	}
}

func (w *Worker) batch(ctx context.Context) (int, error) {
	rows, err := w.Pool.Query(ctx, `
		SELECT id, entity_id, op FROM search_outbox
		WHERE processed_at IS NULL AND attempts < 5
		ORDER BY id LIMIT 50`)
	if err != nil {
		return 0, err
	}
	type task struct {
		id       int64
		entityID int64
		op       string
	}
	tasks := make([]task, 0, 50)
	for rows.Next() {
		var t task
		if err := rows.Scan(&t.id, &t.entityID, &t.op); err != nil {
			rows.Close()
			return 0, err
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if len(tasks) == 0 {
		return 0, nil
	}

	done := 0
	for _, t := range tasks {
		if err := w.handle(ctx, t.entityID, t.op); err != nil {
			// 失败记录留在表里，attempts 到 5 就停手，等人工排查
			_, _ = w.Pool.Exec(ctx, `
				UPDATE search_outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`,
				t.id, err.Error())
			log.Printf("[search] outbox#%d post=%d 同步失败: %v", t.id, t.entityID, err)
			continue
		}
		if _, err := w.Pool.Exec(ctx,
			`UPDATE search_outbox SET processed_at = now() WHERE id = $1`, t.id); err != nil {
			return done, err
		}
		done++
	}
	return len(tasks), nil
}

// handle 按文章当前状态决定 upsert 还是 delete：只有"已发布且公开"才进索引。
func (w *Worker) handle(ctx context.Context, postID int64, op string) error {
	if op == "delete" {
		return w.Cli.DeleteDoc(ctx, fmt.Sprintf("%d", postID))
	}

	var (
		title, slug, status, visibility string
		summary, contentMD, coverURL    *string
		publishedAt                     *time.Time
		updatedAt                       time.Time
		categoryID                      *int64
	)
	err := w.Pool.QueryRow(ctx, `
		SELECT title, slug, status, visibility, summary, content_md, cover_url,
		       published_at, updated_at, category_id
		FROM posts WHERE id = $1`, postID,
	).Scan(&title, &slug, &status, &visibility, &summary, &contentMD, &coverURL,
		&publishedAt, &updatedAt, &categoryID)
	if err != nil {
		// 文章被删了：当成删除文档
		if strings.Contains(err.Error(), "no rows") {
			return w.Cli.DeleteDoc(ctx, fmt.Sprintf("%d", postID))
		}
		return err
	}

	if status != "published" || visibility == "private" {
		return w.Cli.DeleteDoc(ctx, fmt.Sprintf("%d", postID))
	}

	doc := map[string]any{
		"id":          postID,
		"title":       title,
		"slug":        slug,
		"summary":     deref(summary),
		"coverUrl":    deref(coverURL),
		"content":     mdToPlain(deref(contentMD)),
		"status":      status,
		"visibility":  visibility,
		"categoryId":  categoryID,
		"publishedAt": derefTime(publishedAt),
		"updatedAt":   updatedAt.Unix(),
	}
	if err := w.Cli.Upsert(ctx, doc); err != nil {
		return err
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

var (
	reFence   = regexp.MustCompile("(?s)```.*?```")
	reImage   = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reLink    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reHeading = regexp.MustCompile(`(?m)^#{1,6}\s*`)
	reMarks   = regexp.MustCompile("[*_>`~]")
	reSpaces  = regexp.MustCompile(`\s+`)
)

// mdToPlain 粗略去掉 Markdown 标记，只留可检索的文字。
// 代码块整段丢掉：代码里的符号会污染检索结果。
func mdToPlain(md string) string {
	s := reFence.ReplaceAllString(md, " ")
	s = reImage.ReplaceAllString(s, " ")
	s = reLink.ReplaceAllString(s, "$1")
	s = reHeading.ReplaceAllString(s, "")
	s = reMarks.ReplaceAllString(s, "")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
