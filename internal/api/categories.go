package api

import (
	"errors"
	"strconv"
	"strings"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type categoryRow struct {
	ID       int64  `json:"id"`
	ParentID *int64 `json:"parentId"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Sort     int    `json:"sort"`
}

type categoryNode struct {
	categoryRow
	Children []*categoryNode `json:"children,omitempty"`
}

// createCategory 建分类。slug 不传就按名字生成。
func (h *handlers) createCategory(c *gin.Context) {
	var in struct {
		ParentID *int64 `json:"parentId"`
		Name     string `json:"name"`
		Slug     string `json:"slug"`
		Sort     int    `json:"sort"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := validateName(in.Name); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, err.Error())
		return
	}
	if in.ParentID != nil && *in.ParentID == 0 {
		in.ParentID = nil
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = slugify(in.Name)
	}

	ctx := c.Request.Context()
	if in.ParentID != nil {
		var ok bool
		if err := h.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM categories WHERE id = $1)`, *in.ParentID).Scan(&ok); err != nil {
			internalErr(c, "createCategory.parent", err)
			return
		}
		if !ok {
			response.Fail(c, 400, response.CodeBadRequest, "父分类不存在")
			return
		}
	}

	var r categoryRow
	err := h.Pool.QueryRow(ctx, `
		INSERT INTO categories (parent_id, name, slug, sort)
		VALUES ($1, $2, $3, $4)
		RETURNING id, parent_id, name, slug, sort`,
		in.ParentID, strings.TrimSpace(in.Name), slug, in.Sort,
	).Scan(&r.ID, &r.ParentID, &r.Name, &r.Slug, &r.Sort)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "slug 已存在")
		return
	}
	if err != nil {
		internalErr(c, "createCategory", err)
		return
	}
	response.OK(c, r)
}

// listCategories 默认平铺返回；?tree=1 时组装成树。
func (h *handlers) listCategories(c *gin.Context) {
	rows, err := h.Pool.Query(c.Request.Context(),
		`SELECT id, parent_id, name, slug, sort FROM categories ORDER BY sort, id`)
	if err != nil {
		internalErr(c, "listCategories", err)
		return
	}
	defer rows.Close()

	flat := make([]categoryRow, 0, 50)
	for rows.Next() {
		var r categoryRow
		if err := rows.Scan(&r.ID, &r.ParentID, &r.Name, &r.Slug, &r.Sort); err != nil {
			internalErr(c, "listCategories.scan", err)
			return
		}
		flat = append(flat, r)
	}

	if c.Query("tree") != "1" {
		response.OK(c, flat)
		return
	}

	byID := make(map[int64]*categoryNode, len(flat))
	roots := make([]*categoryNode, 0)
	for i := range flat {
		byID[flat[i].ID] = &categoryNode{categoryRow: flat[i]}
	}
	for i := range flat {
		n := byID[flat[i].ID]
		if n.ParentID != nil {
			if p, ok := byID[*n.ParentID]; ok {
				p.Children = append(p.Children, n)
				continue
			}
		}
		roots = append(roots, n)
	}
	response.OK(c, roots)
}

// updateCategory 改名 / 改 slug / 改排序 / 换父分类，换父之前查环。
func (h *handlers) updateCategory(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		Name     *string `json:"name"`
		Slug     *string `json:"slug"`
		Sort     *int    `json:"sort"`
		ParentID *int64  `json:"parentId"` // 0 表示移到根
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			response.Fail(c, 400, response.CodeBadRequest, err.Error())
			return
		}
	}

	ctx := c.Request.Context()
	var parent *int64
	if in.ParentID != nil && *in.ParentID != 0 {
		parent = in.ParentID
		// 同一个防环手法：沿目标父节点往上走，遇到自己就拒绝
		var cycle bool
		if err := h.Pool.QueryRow(ctx, `
			WITH RECURSIVE up AS (
			    SELECT id, parent_id FROM categories WHERE id = $2
			    UNION ALL
			    SELECT c.id, c.parent_id FROM categories c JOIN up ON c.id = up.parent_id
			)
			SELECT EXISTS (SELECT 1 FROM up WHERE id = $1)`, id, *parent).Scan(&cycle); err != nil {
			internalErr(c, "updateCategory.cycle", err)
			return
		}
		if cycle {
			response.Fail(c, 400, response.CodeBadRequest, "不能把分类移到自己的子分类下")
			return
		}
	}

	var r categoryRow
	err := h.Pool.QueryRow(ctx, `
		UPDATE categories SET
		    name      = COALESCE($2, name),
		    slug      = COALESCE($3, slug),
		    sort      = COALESCE($4, sort),
		    parent_id = CASE WHEN $5::boolean THEN $6::bigint ELSE parent_id END
		WHERE id = $1
		RETURNING id, parent_id, name, slug, sort`,
		id, in.Name, in.Slug, in.Sort, in.ParentID != nil, parent,
	).Scan(&r.ID, &r.ParentID, &r.Name, &r.Slug, &r.Sort)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "分类不存在")
		return
	}
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "slug 已存在")
		return
	}
	if err != nil {
		internalErr(c, "updateCategory", err)
		return
	}
	response.OK(c, r)
}

// deleteCategory 删分类。有子分类时拒绝，逼着从叶子往上删，避免一次删掉一整棵树还找不回。
func (h *handlers) deleteCategory(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	var hasChildren bool
	if err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM categories WHERE parent_id = $1)`, id).Scan(&hasChildren); err != nil {
		internalErr(c, "deleteCategory.children", err)
		return
	}
	if hasChildren {
		response.Fail(c, 409, response.CodeBadRequest, "该分类下还有子分类，请先删子分类")
		return
	}

	// posts.category_id 是 ON DELETE SET NULL，文章不会跟着消失
	tag, err := h.Pool.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
	if err != nil {
		internalErr(c, "deleteCategory", err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.Fail(c, 404, response.CodeNotFound, "分类不存在")
		return
	}
	response.OK(c, gin.H{"deleted": id})
}

// ---------- 标签 ----------

type tagRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	PostCount int64  `json:"postCount"`
}

func (h *handlers) createTag(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := validateName(in.Name); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, err.Error())
		return
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = slugify(in.Name)
	}

	var r tagRow
	err := h.Pool.QueryRow(c.Request.Context(), `
		INSERT INTO tags (name, slug) VALUES ($1, $2)
		RETURNING id, name, slug`,
		strings.TrimSpace(in.Name), slug).Scan(&r.ID, &r.Name, &r.Slug)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "标签名或 slug 已存在")
		return
	}
	if err != nil {
		internalErr(c, "createTag", err)
		return
	}
	response.OK(c, r)
}

func (h *handlers) listTags(c *gin.Context) {
	rows, err := h.Pool.Query(c.Request.Context(), `
		SELECT t.id, t.name, t.slug,
		       (SELECT count(*) FROM post_tags pt WHERE pt.tag_id = t.id)
		FROM tags t ORDER BY t.id`)
	if err != nil {
		internalErr(c, "listTags", err)
		return
	}
	defer rows.Close()

	items := make([]tagRow, 0, 50)
	for rows.Next() {
		var t tagRow
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.PostCount); err != nil {
			internalErr(c, "listTags.scan", err)
			return
		}
		items = append(items, t)
	}
	response.OK(c, items)
}

func (h *handlers) updateTag(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		Name *string `json:"name"`
		Slug *string `json:"slug"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}

	var r tagRow
	err := h.Pool.QueryRow(c.Request.Context(), `
		UPDATE tags SET name = COALESCE($2, name), slug = COALESCE($3, slug)
		WHERE id = $1
		RETURNING id, name, slug`, id, in.Name, in.Slug).Scan(&r.ID, &r.Name, &r.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "标签不存在")
		return
	}
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "标签名或 slug 已存在")
		return
	}
	if err != nil {
		internalErr(c, "updateTag", err)
		return
	}
	response.OK(c, r)
}

func (h *handlers) deleteTag(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	tag, err := h.Pool.Exec(c.Request.Context(), `DELETE FROM tags WHERE id = $1`, id)
	if err != nil {
		internalErr(c, "deleteTag", err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.Fail(c, 404, response.CodeNotFound, "标签不存在")
		return
	}
	response.OK(c, gin.H{"deleted": id})
}

// setPostTags 覆盖式设置文章的标签。
// 先全删再全插，放在一个事务里：中途失败不会留下半套标签。
func (h *handlers) setPostTags(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		TagIDs []int64 `json:"tagIds"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	ctx := c.Request.Context()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "setPostTags.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM posts WHERE id = $1)`, id).Scan(&exists); err != nil {
		internalErr(c, "setPostTags.post", err)
		return
	}
	if !exists {
		response.Fail(c, 404, response.CodeNotFound, "文章不存在")
		return
	}

	if _, err := tx.Exec(ctx, `DELETE FROM post_tags WHERE post_id = $1`, id); err != nil {
		internalErr(c, "setPostTags.clear", err)
		return
	}
	for _, tagID := range in.TagIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO post_tags (post_id, tag_id) VALUES ($1, $2)`, id, tagID); err != nil {
			if isForeignKeyViolation(err) {
				response.Fail(c, 400, response.CodeBadRequest, "标签不存在: "+strconv.FormatInt(tagID, 10))
				return
			}
			internalErr(c, "setPostTags.insert", err)
			return
		}
	}

	// 标签变了，索引里的文档也要更新（标签虽然没进可搜字段，但以后要按标签过滤）
	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (entity_id, op) VALUES ($1, 'upsert')`, id); err != nil {
		internalErr(c, "setPostTags.enqueue", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "setPostTags.commit", err)
		return
	}

	names, err := h.postTagNames(ctx, id)
	if err != nil {
		internalErr(c, "setPostTags.names", err)
		return
	}
	response.OK(c, gin.H{"id": id, "tags": names})
}
