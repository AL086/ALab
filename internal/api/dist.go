package api

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

// 打包下载一次最多装这么多文件，防止一个人把整个库拖走。
const zipMaxFiles = 100

// errNotDir 表示 dir 参数指的不是目录。
var errNotDir = errors.New("节点不是目录")

// zipEntry 是打包时用的一行：zip 内的路径 + 对象存储里的 key。
type zipEntry struct {
	Path string
	Key  string
}

// zipFiles 把一批文件打成 zip 流式吐出去，不落临时文件。
//
//	GET /files/zip?ids=1,2,3   挑几个文件/目录
//	GET /files/zip?dir=10      整个目录（递归含子目录）
func (h *handlers) zipFiles(c *gin.Context) {
	ctx := c.Request.Context()

	var (
		entries []zipEntry
		err     error
		name    = "files.zip"
	)
	if dir := c.Query("dir"); dir != "" {
		rootID, perr := strconv.ParseInt(dir, 10, 64)
		if perr != nil || rootID <= 0 {
			response.Fail(c, 400, response.CodeBadRequest, "dir 必须是正整数")
			return
		}
		entries, name, err = h.collectDirZip(ctx, rootID)
	} else {
		ids := parseIDList(c.Query("ids"), zipMaxFiles)
		if len(ids) == 0 {
			response.Fail(c, 400, response.CodeBadRequest, "给 ids=1,2 或 dir=<目录id>")
			return
		}
		entries, err = h.collectIDsZip(ctx, ids)
	}
	if err != nil {
		if errors.Is(err, errNotDir) {
			response.Fail(c, 400, response.CodeBadRequest, "dir 指向的不是目录")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, 404, response.CodeNotFound, "目录不存在")
			return
		}
		internalErr(c, "zipFiles.collect", err)
		return
	}
	if len(entries) == 0 {
		response.Fail(c, 404, response.CodeNotFound, "没有可打包的文件")
		return
	}

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", contentDisposition(name))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	names := map[string]int{}
	for _, e := range entries {
		// 同名文件加 (2)(3) 后缀，否则 zip 里会出现两条一模一样的路径
		if n := names[e.Path]; n > 0 {
			ext := path.Ext(e.Path)
			base := strings.TrimSuffix(e.Path, ext)
			e.Path = base + "(" + strconv.Itoa(n+1) + ")" + ext
		}
		names[e.Path]++

		w, zerr := zw.CreateHeader(&zip.FileHeader{Name: e.Path, Method: zip.Deflate})
		if zerr != nil {
			log.Printf("[warn] zip 建条目失败 %s: %v", e.Path, zerr)
			continue
		}
		obj, oerr := h.S3.GetObject(ctx, h.Bucket, e.Key, minio.GetObjectOptions{})
		if oerr != nil {
			log.Printf("[warn] zip 取对象失败 %s: %v", e.Path, oerr)
			continue
		}
		if _, serr := obj.Stat(); serr != nil {
			obj.Close()
			log.Printf("[warn] zip stat 失败 %s: %v", e.Path, serr)
			continue
		}
		if _, cerr := io.Copy(w, obj); cerr != nil {
			log.Printf("[warn] zip 写条目中断 %s: %v", e.Path, cerr)
		}
		obj.Close()
	}
	if err := zw.Close(); err != nil {
		log.Printf("[warn] zip 收尾失败: %v", err)
	}

	if _, err := h.Pool.Exec(ctx, `
		INSERT INTO download_logs (file_id, ip, user_agent, kind)
		VALUES (NULL, NULLIF($1, '')::inet, $2, 'zip')`,
		strings.TrimSpace(c.ClientIP()), c.GetHeader("User-Agent")); err != nil {
		log.Printf("[warn] 写打包下载日志失败: %v", err)
	}
}

// collectDirZip 递归收集某个目录下的文件，zip 内保留相对路径（目录名作为顶层）。
func (h *handlers) collectDirZip(ctx context.Context, dirID int64) ([]zipEntry, string, error) {
	var (
		dirName, dirType string
	)
	if err := h.Pool.QueryRow(ctx,
		`SELECT name, type FROM file_nodes WHERE id = $1 AND deleted_at IS NULL`, dirID).
		Scan(&dirName, &dirType); err != nil {
		return nil, "", err
	}
	if dirType != "dir" {
		return nil, "", errNotDir
	}

	rows, err := h.Pool.Query(ctx, `
		WITH RECURSIVE tree AS (
		    SELECT id, parent_id, name, type, storage_key, name::text AS rel
		    FROM file_nodes
		    WHERE id = $1 AND deleted_at IS NULL
		    UNION ALL
		    SELECT f.id, f.parent_id, f.name, f.type, f.storage_key, t.rel || '/' || f.name
		    FROM file_nodes f JOIN tree t ON f.parent_id = t.id
		    WHERE f.deleted_at IS NULL
		)
		SELECT rel, storage_key FROM tree
		WHERE type = 'file' AND storage_key IS NOT NULL
		ORDER BY rel
		LIMIT $2`, dirID, zipMaxFiles)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	entries := make([]zipEntry, 0, 32)
	for rows.Next() {
		var e zipEntry
		if err := rows.Scan(&e.Path, &e.Key); err != nil {
			return nil, "", err
		}
		entries = append(entries, e)
	}
	return entries, dirName + ".zip", rows.Err()
}

// collectIDsZip 按一组 id 打包，zip 内用文件名本身（不重建目录层级）。
func (h *handlers) collectIDsZip(ctx context.Context, ids []int64) ([]zipEntry, error) {
	rows, err := h.Pool.Query(ctx, `
		SELECT name, storage_key FROM file_nodes
		WHERE id = ANY($1::bigint[]) AND type = 'file'
		  AND deleted_at IS NULL AND storage_key IS NOT NULL
		ORDER BY name
		LIMIT $2`, ids, zipMaxFiles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]zipEntry, 0, len(ids))
	for rows.Next() {
		var e zipEntry
		if err := rows.Scan(&e.Path, &e.Key); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// previewAllow 是 /raw/ 允许"浏览器里直接看"的扩展名白名单。
// 不在名单里的一律不给 inline，免得把 HTML/SVG 之类当成网页执行。
var previewAllow = map[string]string{
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/plain; charset=utf-8",
	".log":  "text/plain; charset=utf-8",
	".json": "text/plain; charset=utf-8",
	".csv":  "text/plain; charset=utf-8",
	".yaml": "text/plain; charset=utf-8",
	".yml":  "text/plain; charset=utf-8",
	".sh":   "text/plain; charset=utf-8",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".pdf":  "application/pdf",
}

// shareDownload 是 /s/:id —— 稳定的下载短链，语义等同 /files/:id/download。
// 私有文件仍然要登录，只是省掉了带 token 的长参数。
func (h *handlers) shareDownload(c *gin.Context) {
	h.serveDist(c, "attachment", "share")
}

// rawPreview 是 /raw/:id —— 只做"看"，白名单之外的类型直接拒。
func (h *handlers) rawPreview(c *gin.Context) {
	h.serveDist(c, "inline", "raw")
}

// serveDist 是 /s/ 与 /raw/ 共用的一条分发逻辑。
//
// 安全口径（对齐设计文档 F5）：只读、不执行、白名单、nosniff、私有要鉴权、有审计。
func (h *handlers) serveDist(c *gin.Context, disposition, kind string) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if !allowByIP(c.ClientIP()) {
		response.Fail(c, 429, response.CodeBadRequest, "请求太快了，稍后再试")
		return
	}
	ctx := c.Request.Context()

	var (
		name, typ, visibility string
		key, sha, mimeType    *string
		deletedAt             *time.Time
	)
	err := h.Pool.QueryRow(ctx, `
		SELECT name, type, visibility, storage_key, sha256, mime, deleted_at
		FROM file_nodes WHERE id = $1`, id,
	).Scan(&name, &typ, &visibility, &key, &sha, &mimeType, &deletedAt)
	if err == pgx.ErrNoRows {
		response.Fail(c, 404, response.CodeNotFound, "文件不存在")
		return
	}
	if err != nil {
		internalErr(c, "serveDist.select", err)
		return
	}
	if deletedAt != nil || typ != "file" || key == nil {
		response.Fail(c, 404, response.CodeNotFound, "文件不存在或已删除")
		return
	}

	if visibility != "public" {
		if _, authed := h.Auth.Soft(c); !authed {
			// 私有文件对外一律 404，不泄露"存在但没权限"
			response.Fail(c, 404, response.CodeNotFound, "文件不存在")
			return
		}
	}

	ct := ""
	if disposition == "inline" {
		ext := strings.ToLower(path.Ext(name))
		allow, ok := previewAllow[ext]
		if !ok {
			response.Fail(c, 415, response.CodeBadRequest,
				"该类型不支持在线预览（只认 txt/md/json/csv/图片/PDF），请用 /s/ 下载")
			return
		}
		ct = allow
	}
	if ct == "" && mimeType != nil && *mimeType != "" {
		ct = *mimeType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	obj, err := h.S3.GetObject(ctx, h.Bucket, *key, minio.GetObjectOptions{})
	if err != nil {
		internalErr(c, "serveDist.getObject", err)
		return
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		internalErr(c, "serveDist.stat", err)
		return
	}

	c.Header("Content-Type", ct)
	c.Header("Content-Disposition", distDisposition(disposition, name))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "public, max-age=300")
	if sha != nil {
		c.Header("X-Content-Sha256", *sha)
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, obj); err != nil {
		log.Printf("[warn] %s 传输中断 request_id=%s: %v", kind, c.GetString("request_id"), err)
	}

	if _, err := h.Pool.Exec(ctx, `
		INSERT INTO download_logs (file_id, ip, user_agent, kind)
		VALUES ($1, NULLIF($2, '')::inet, $3, $4)`,
		id, strings.TrimSpace(c.ClientIP()), c.GetHeader("User-Agent"), kind); err != nil {
		log.Printf("[warn] 写分发日志失败: %v", err)
	}
}

// distDisposition 只放 ASCII 回退名 + RFC 5987 的 UTF-8 名，中文文件名才不会被浏览器截断。
func distDisposition(kind, name string) string {
	if kind == "inline" {
		return "inline"
	}
	return contentDisposition(name)
}

// parseIDList 把 "1,2,3" 解析成 id 列表，顺带去重和截断。
func parseIDList(raw string, limit int) []int64 {
	if raw == "" {
		return nil
	}
	out := make([]int64, 0, limit)
	seen := map[int64]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// 简易限流：按 IP 做 1 分钟固定窗口，够挡脚本循环刷 /s/ 和 /raw/。
// 单机个人站，内存计数就够了，不引 Redis。
var (
	limitMu   sync.Mutex
	limitHits = map[string]struct {
		window time.Time
		count  int
	}{}
)

const limitPerMinute = 60

func allowByIP(ip string) bool {
	if ip == "" {
		return true
	}
	now := time.Now()
	limitMu.Lock()
	defer limitMu.Unlock()
	rec := limitHits[ip]
	if now.Sub(rec.window) >= time.Minute {
		rec.window = now
		rec.count = 0
	}
	rec.count++
	limitHits[ip] = rec
	// 顺手清掉过期窗口，避免 map 无限长
	if len(limitHits) > 4096 {
		for k, v := range limitHits {
			if now.Sub(v.window) >= time.Minute {
				delete(limitHits, k)
			}
		}
	}
	return rec.count <= limitPerMinute
}
