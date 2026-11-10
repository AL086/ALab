package api

import (
	"strings"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
)

// pubFileRow 是前台匿名浏览用的一行：多带一个 path，因为匿名侧不让你进目录树。
type pubFileRow struct {
	ID         int64              `json:"id"`
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	Size       int64              `json:"size"`
	SHA256     string             `json:"sha256"`
	Mime       string             `json:"mime"`
	Visibility string             `json:"visibility"`
	ExecPublic bool               `json:"execPublic"`
	UpdatedAt  pgtype.Timestamptz `json:"updatedAt"`
}

// publicFiles 是前台匿名浏览用的"公开文件"列表：只返回 visibility='public' 的文件。
//
// 为什么不是按目录列：目录本身也有可见性，匿名侧顺着目录点进去很容易出现
// "目录是私有的、里面却有公开文件"的死角。前台就做成一层平铺的公开文件区，
// 每行带完整路径（文档/notes.txt），想看自己的完整目录树就登录后走 /files。
func (h *handlers) publicFiles(c *gin.Context) {
	page, pageSize := paging(c)
	if pageSize > 200 {
		pageSize = 200
	}
	ctx := c.Request.Context()

	var total int64
	if err := h.Pool.QueryRow(ctx, `
		SELECT count(*) FROM file_nodes
		WHERE deleted_at IS NULL AND type = 'file' AND visibility = 'public'`).
		Scan(&total); err != nil {
		internalErr(c, "publicFiles.count", err)
		return
	}

	rows, err := h.Pool.Query(ctx, `
		SELECT id, parent_id, name, size, sha256, mime, visibility, exec_public, updated_at
		FROM file_nodes
		WHERE deleted_at IS NULL AND type = 'file' AND visibility = 'public'
		ORDER BY updated_at DESC, name
		LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		internalErr(c, "publicFiles.query", err)
		return
	}
	defer rows.Close()

	type raw struct {
		id       int64
		parentID *int64
		name     string
		size     int64
		sha      string
		mime     string
		vis      string
		exec     bool
		updated  pgtype.Timestamptz
	}
	list := make([]raw, 0, pageSize)
	need := map[int64]bool{}
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.parentID, &r.name, &r.size, &r.sha, &r.mime,
			&r.vis, &r.exec, &r.updated); err != nil {
			internalErr(c, "publicFiles.scan", err)
			return
		}
		list = append(list, r)
		if r.parentID != nil {
			need[*r.parentID] = true
		}
	}

	// 一次把涉及的目录名查出来，再拼路径；不解一层层递归查询。
	names := map[int64]struct {
		name     string
		parentID *int64
	}{}
	if len(need) > 0 {
		ids := make([]int64, 0, len(need))
		for id := range need {
			ids = append(ids, id)
		}
		drows, derr := h.Pool.Query(ctx, `
			SELECT id, parent_id, name FROM file_nodes WHERE id = ANY($1)`, ids)
		if derr != nil {
			internalErr(c, "publicFiles.dirs", derr)
			return
		}
		defer drows.Close()
		for drows.Next() {
			var id int64
			var name string
			var parent *int64
			if err := drows.Scan(&id, &parent, &name); err != nil {
				internalErr(c, "publicFiles.dirsScan", err)
				return
			}
			names[id] = struct {
				name     string
				parentID *int64
			}{name: name, parentID: parent}
		}
	}

	items := make([]pubFileRow, 0, len(list))
	for _, r := range list {
		parts := []string{}
		cur := r.parentID
		guard := 0
		for cur != nil && guard < 64 {
			if n, ok := names[*cur]; ok {
				parts = append([]string{n.name}, parts...)
				cur = n.parentID
			} else {
				break
			}
			guard++
		}
		items = append(items, pubFileRow{
			ID: r.id, Name: r.name, Path: strings.Join(parts, "/"),
			Size: r.size, SHA256: r.sha, Mime: r.mime,
			Visibility: r.vis, ExecPublic: r.exec, UpdatedAt: r.updated,
		})
	}
	response.OK(c, response.List{Items: items, Total: total, Page: page, PageSize: pageSize})
}
