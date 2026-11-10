package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type postRow struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Slug        string     `json:"slug"`
	Summary     *string    `json:"summary"`
	ContentMD   string     `json:"contentMd"`
	CoverURL    *string    `json:"coverUrl"`
	Status      string     `json:"status"`
	Visibility  string     `json:"visibility"`
	CategoryID  *int64     `json:"categoryId"`
	Tags        []string   `json:"tags"`
	PublishedAt *time.Time `json:"publishedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// tagsAgg 是取标签数组的子查询，列表和详情共用。
// 用 array_agg 而不是 LEFT JOIN，避免一条 SQL 里 GROUP BY 一堆字段。
const tagsAgg = `COALESCE((SELECT array_agg(t.name ORDER BY t.name)
	                     FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
	                     WHERE pt.post_id = posts.id), '{}'::text[])`

// createPost 新建草稿。slug 不传就按标题生成，冲突自动加序号。
func (h *handlers) createPost(c *gin.Context) {
	var in struct {
		Title      string `json:"title"`
		Slug       string `json:"slug"`
		Summary    string `json:"summary"`
		ContentMD  string `json:"contentMd"`
		CoverURL   string `json:"coverUrl"`
		CategoryID *int64 `json:"categoryId"`
		Visibility string `json:"visibility"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.ContentMD) == "" {
		response.Fail(c, 400, response.CodeBadRequest, "title 与 contentMd 不能为空")
		return
	}
	if msg := checkCoverURL(in.CoverURL); msg != "" {
		response.Fail(c, 400, response.CodeBadRequest, msg)
		return
	}

	visibility := in.Visibility
	if visibility == "" {
		visibility = "public"
	}
	// 0 表示"不挂分类"；不这么归一化会撞外键（categories 里没有 id=0）
	if in.CategoryID != nil && *in.CategoryID == 0 {
		in.CategoryID = nil
	}

	ctx := c.Request.Context()
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = slugify(in.Title)
	}

	var (
		row postRow
		err error
	)
	for i := 1; i <= 5; i++ {
		err = h.Pool.QueryRow(ctx, `
			INSERT INTO posts (title, slug, summary, content_md, cover_url, category_id, visibility, status)
			VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), $6, $7, 'draft')
			RETURNING id, title, slug, summary, content_md, cover_url, status, visibility,
			          category_id, published_at, created_at, updated_at`,
			in.Title, slug, in.Summary, in.ContentMD, in.CoverURL, in.CategoryID, visibility,
		).Scan(&row.ID, &row.Title, &row.Slug, &row.Summary, &row.ContentMD, &row.CoverURL, &row.Status,
			&row.Visibility, &row.CategoryID, &row.PublishedAt, &row.CreatedAt, &row.UpdatedAt)
		if err == nil {
			break
		}
		// slug 唯一冲突：换个后缀重试
		if isUniqueViolation(err) {
			slug = slugify(in.Title) + "-" + strconv.Itoa(i+1)
			continue
		}
		break
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			response.Fail(c, 400, response.CodeBadRequest, "categoryId 指向的分类不存在")
			return
		}
		internalErr(c, "createPost", err)
		return
	}
	row.Tags = []string{} // 新建还没挂标签，返回空数组而不是 null
	response.OK(c, row)
}

// listPosts 分页列出文章。未登录只能看到"已发布 + 公开"，登录后可按状态过滤。
// 回收站里的文章默认不出现：登录后带 trash=1 才列已删除的。
// tag 支持逗号分隔的 slug，多个标签取交集（AND）。
func (h *handlers) listPosts(c *gin.Context) {
	page, pageSize := paging(c)
	status := c.Query("status")

	ctx := c.Request.Context()
	_, authed := h.Auth.Soft(c)

	var total int64
	args := []any{}
	where := "WHERE deleted_at IS NULL"
	if authed && c.Query("trash") != "" {
		where = "WHERE deleted_at IS NOT NULL"
	}
	if authed {
		if status != "" {
			args = append(args, status)
			where += " AND status = $" + strconv.Itoa(len(args))
		}
	} else {
		where += " AND status = 'published' AND visibility = 'public'"
	}
	for _, slug := range tagSlugs(c.Query("tag")) {
		args = append(args, slug)
		where += ` AND EXISTS (SELECT 1 FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		                     WHERE pt.post_id = posts.id AND (t.slug = $` + strconv.Itoa(len(args)) + ` OR t.name = $` + strconv.Itoa(len(args)) + `))`
	}
	if err := h.Pool.QueryRow(ctx, "SELECT count(*) FROM posts "+where, args...).Scan(&total); err != nil {
		internalErr(c, "listPosts.count", err)
		return
	}

	args = append(args, pageSize, (page-1)*pageSize)
	sql := `SELECT id, title, slug, summary, content_md, cover_url, status, visibility,
	               category_id, published_at, created_at, updated_at, ` + tagsAgg + `
	        FROM posts ` + where + `
	        ORDER BY created_at DESC
	        LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		internalErr(c, "listPosts.query", err)
		return
	}
	defer rows.Close()

	items := make([]postRow, 0, pageSize)
	for rows.Next() {
		var p postRow
		if err := rows.Scan(&p.ID, &p.Title, &p.Slug, &p.Summary, &p.ContentMD, &p.CoverURL, &p.Status,
			&p.Visibility, &p.CategoryID, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt, &p.Tags); err != nil {
			internalErr(c, "listPosts.scan", err)
			return
		}
		items = append(items, p)
	}
	response.OK(c, response.List{Items: items, Total: total, Page: page, PageSize: pageSize})
}

func (h *handlers) getPost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var p postRow
	err := h.Pool.QueryRow(c.Request.Context(), `
		SELECT id, title, slug, summary, content_md, cover_url, status, visibility,
		       category_id, published_at, created_at, updated_at, `+tagsAgg+`
		FROM posts WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&p.ID, &p.Title, &p.Slug, &p.Summary, &p.ContentMD, &p.CoverURL, &p.Status,
		&p.Visibility, &p.CategoryID, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt, &p.Tags)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在")
		return
	}
	if err != nil {
		internalErr(c, "getPost", err)
		return
	}

	// 匿名请求读不到草稿和私有文章，但统一回 404，不泄露"存在但不可见"
	if _, authed := h.Auth.Soft(c); !authed {
		if p.Status != "published" || p.Visibility == "private" {
			response.Fail(c, 404, response.CodeNotFound, "文章不存在")
			return
		}
	}
	response.OK(c, p)
}

// updatePost 编辑正文（草稿自动保存走这里，不产生 revision）。
func (h *handlers) updatePost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		Title      *string `json:"title"`
		Summary    *string `json:"summary"`
		ContentMD  *string `json:"contentMd"`
		CoverURL   *string `json:"coverUrl"` // 传 "" 表示清空封面
		Slug       *string `json:"slug"`
		CategoryID *int64  `json:"categoryId"` // 0 表示清空分类
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.CoverURL != nil {
		if msg := checkCoverURL(*in.CoverURL); msg != "" {
			response.Fail(c, 400, response.CodeBadRequest, msg)
			return
		}
	}

	var p postRow
	ctx := c.Request.Context()

	// categoryId 传 0 表示清空分类，传别的值表示挂到该分类
	var category *int64
	if in.CategoryID != nil && *in.CategoryID != 0 {
		category = in.CategoryID
	}

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "updatePost.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		UPDATE posts SET
		    title       = COALESCE($2, title),
		    summary     = COALESCE($3, summary),
		    content_md  = COALESCE($4, content_md),
		    slug        = COALESCE($5, slug),
		    category_id = CASE WHEN $6::boolean THEN $7::bigint ELSE category_id END,
		    cover_url   = CASE WHEN $8::boolean THEN NULLIF($9, '') ELSE cover_url END,
		    updated_at  = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING id, title, slug, summary, content_md, cover_url, status, visibility,
		          category_id, published_at, created_at, updated_at`,
		id, in.Title, in.Summary, in.ContentMD, in.Slug, in.CategoryID != nil, category,
		in.CoverURL != nil, coverValue(in.CoverURL),
	).Scan(&p.ID, &p.Title, &p.Slug, &p.Summary, &p.ContentMD, &p.CoverURL, &p.Status,
		&p.Visibility, &p.CategoryID, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在")
		return
	}
	if isForeignKeyViolation(err) {
		response.Fail(c, 400, response.CodeBadRequest, "categoryId 指向的分类不存在")
		return
	}
	if err != nil {
		internalErr(c, "updatePost", err)
		return
	}
	if names, terr := h.postTagNames(ctx, id); terr == nil {
		p.Tags = names
	} else {
		log.Printf("[warn] updatePost 取标签失败 id=%d: %v", id, terr)
	}

	// 只有已发布的文章才需要进索引；草稿改了不必惊动搜索引擎
	if p.Status == "published" {
		if err := enqueueSearch(ctx, tx, id, "upsert"); err != nil {
			internalErr(c, "updatePost.enqueue", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "updatePost.commit", err)
		return
	}
	response.OK(c, p)
}

// publishPost 发布：产生一条 revision，状态置 published。整件事一个事务。
func (h *handlers) publishPost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "publishPost.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	versionNo, err := publishInTx(ctx, tx, id, "")
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在")
		return
	}
	if errors.Is(err, errPostArchived) {
		response.Fail(c, 409, response.CodeBadRequest, errPostArchived.Error())
		return
	}
	if err != nil {
		internalErr(c, "publishPost", err)
		return
	}
	// 和发布同一个事务：只要提交了，就一定有一条待同步记录
	if err := enqueueSearch(ctx, tx, id, "upsert"); err != nil {
		internalErr(c, "publishPost.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "publishPost.commit", err)
		return
	}

	response.OK(c, gin.H{"id": id, "versionNo": versionNo, "status": "published"})
}

// rollbackPost 回滚到某个版本。回滚也是"新增一个版本"，不改历史。
func (h *handlers) rollbackPost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		VersionNo int `json:"versionNo"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.VersionNo <= 0 {
		response.Fail(c, 400, response.CodeBadRequest, "需要 versionNo")
		return
	}

	ctx := c.Request.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "rollbackPost.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	var title, content string
	err = tx.QueryRow(ctx,
		`SELECT title, content_md FROM post_revisions WHERE post_id = $1 AND version_no = $2`,
		id, in.VersionNo).Scan(&title, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "该版本不存在")
		return
	}
	if err != nil {
		internalErr(c, "rollbackPost.select", err)
		return
	}

	var nextNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM post_revisions WHERE post_id = $1`,
		id).Scan(&nextNo); err != nil {
		internalErr(c, "rollbackPost.nextNo", err)
		return
	}

	sum := sha256.Sum256([]byte(content))
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_revisions (post_id, version_no, title, content_md, sha256, note)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, nextNo, title, content, hex.EncodeToString(sum[:]), "rollback to v"+strconv.Itoa(in.VersionNo)); err != nil {
		internalErr(c, "rollbackPost.insert", err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE posts SET title = $2, content_md = $3, updated_at = now() WHERE id = $1`,
		id, title, content); err != nil {
		internalErr(c, "rollbackPost.update", err)
		return
	}
	// 回滚改了正文，索引也得跟着变
	if err := enqueueSearch(ctx, tx, id, "upsert"); err != nil {
		internalErr(c, "rollbackPost.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "rollbackPost.commit", err)
		return
	}

	response.OK(c, gin.H{"id": id, "versionNo": nextNo, "rollbackTo": in.VersionNo})
}

// setPostStatus 只处理"下线"这一类状态：published -> archived / draft。
// 重新上线必须走 POST /posts/:id/publish（会产生新版本、重新进索引）。
func (h *handlers) setPostStatus(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.Status != "draft" && in.Status != "archived" {
		response.Fail(c, 400, response.CodeBadRequest, "status 只支持 draft / archived，发布请用 /publish")
		return
	}

	ctx := c.Request.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "setPostStatus.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx,
		`UPDATE posts SET status = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING status`,
		id, in.Status).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在")
		return
	}
	if err != nil {
		internalErr(c, "setPostStatus", err)
		return
	}
	// 下线就从索引里摘掉；草稿本来也没进过索引，多删一次幂等无害
	if err := enqueueSearch(ctx, tx, id, "delete"); err != nil {
		internalErr(c, "setPostStatus.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "setPostStatus.commit", err)
		return
	}
	response.OK(c, gin.H{"id": id, "status": status})
}

// deletePost 把文章挪进回收站（软删）。30 天后清理任务会物理删除它，
// 连带 post_revisions 与 post_tags 一起走，所以删错了还有得救。
func (h *handlers) deletePost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "deletePost.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `
		UPDATE posts SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING status`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在或已在回收站里")
		return
	}
	if err != nil {
		internalErr(c, "deletePost", err)
		return
	}
	// 不管之前是什么状态，先把索引记录摘掉；真拉回来时再按状态补
	if err := enqueueSearch(ctx, tx, id, "delete"); err != nil {
		internalErr(c, "deletePost.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "deletePost.commit", err)
		return
	}
	response.OK(c, gin.H{"id": id, "deleted": true, "retentionDays": 30, "status": status})
}

// restorePost 从回收站拉回来。已发布的文章会重新进索引。
func (h *handlers) restorePost(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "restorePost.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `
		UPDATE posts SET deleted_at = NULL, updated_at = now()
		WHERE id = $1 AND deleted_at IS NOT NULL
		RETURNING status`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文章不在回收站里")
		return
	}
	if err != nil {
		internalErr(c, "restorePost", err)
		return
	}

	action := "delete"
	if status == "published" {
		action = "upsert"
	}
	if err := enqueueSearch(ctx, tx, id, action); err != nil {
		internalErr(c, "restorePost.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "restorePost.commit", err)
		return
	}
	response.OK(c, gin.H{"id": id, "restored": true, "status": status})
}

// tagSlugs 把 ?tag=a,b 拆成 slug 列表：去空、去重、最多 5 个，避免堆成一大串 EXISTS。
func tagSlugs(raw string) []string {
	if raw == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 5)
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) == 5 {
			break
		}
	}
	return out
}

type revisionRow struct {
	VersionNo int       `json:"versionNo"`
	Title     string    `json:"title"`
	SHA256    string    `json:"sha256"`
	Note      *string   `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
}

func (h *handlers) listRevisions(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	rows, err := h.Pool.Query(c.Request.Context(), `
		SELECT version_no, title, sha256, note, created_at
		FROM post_revisions WHERE post_id = $1
		ORDER BY version_no DESC`, id)
	if err != nil {
		internalErr(c, "listRevisions", err)
		return
	}
	defer rows.Close()

	items := make([]revisionRow, 0, 10)
	for rows.Next() {
		var r revisionRow
		if err := rows.Scan(&r.VersionNo, &r.Title, &r.SHA256, &r.Note, &r.CreatedAt); err != nil {
			internalErr(c, "listRevisions.scan", err)
			return
		}
		items = append(items, r)
	}
	response.OK(c, items)
}

// checkCoverURL 只做基本约束：空 = 没有封面；站内绝对路径（/raw/<id>）或 http(s) 外链。
// 不校验图片真的存在——文件可能被回收站带走，前端裂图时会换成占位块。
func checkCoverURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if len(u) > 512 {
		return "coverUrl 太长了（最多 512 字符）"
	}
	lower := strings.ToLower(u)
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		return ""
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return ""
	}
	return "coverUrl 只接受 / 开头的站内路径或 http(s) 外链"
}

func coverValue(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// errPostArchived 已归档的文章必须先"退回草稿"再发布，避免绕过归档直接上线。
var errPostArchived = errors.New("已归档的文章不能发布，先退回草稿再发布")

// publishInTx 抽出来给发布和"保存即发布"复用。调用方负责事务边界。
func publishInTx(ctx context.Context, tx pgx.Tx, id int64, note string) (int, error) {
	var (
		title, content, status string
	)
	if err := tx.QueryRow(ctx,
		`SELECT title, content_md, status FROM posts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id,
	).Scan(&title, &content, &status); err != nil {
		return 0, err
	}
	if status == "archived" {
		return 0, errPostArchived
	}

	var versionNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM post_revisions WHERE post_id = $1`, id,
	).Scan(&versionNo); err != nil {
		return 0, err
	}

	sum := sha256.Sum256([]byte(content))
	if _, err := tx.Exec(ctx, `
		INSERT INTO post_revisions (post_id, version_no, title, content_md, sha256, note)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
		id, versionNo, title, content, hex.EncodeToString(sum[:]), note); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE posts SET status = 'published',
		                 published_at = COALESCE(published_at, now()),
		                 updated_at = now()
		WHERE id = $1`, id); err != nil {
		return 0, err
	}
	return versionNo, nil
}
