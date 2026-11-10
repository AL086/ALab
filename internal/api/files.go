package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

const maxUploadSize = 100 << 20 // 单次上传上限；更大的走分片（还没做）

type fileRow struct {
	ID         int64      `json:"id"`
	ParentID   *int64     `json:"parentId"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Size       int64      `json:"size"`
	SHA256     *string    `json:"sha256"`
	Mime       *string    `json:"mime"`
	Visibility string     `json:"visibility"`
	ExecPublic bool       `json:"execPublic"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	DeletedAt  *time.Time `json:"deletedAt"`
}

// mkdir 新建目录。
func (h *handlers) mkdir(c *gin.Context) {
	var in struct {
		ParentID *int64 `json:"parentId"`
		Name     string `json:"name"`
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

	ctx := c.Request.Context()
	if in.ParentID != nil {
		var ok bool
		if err := h.Pool.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM file_nodes
			              WHERE id = $1 AND type = 'dir' AND deleted_at IS NULL)`,
			*in.ParentID).Scan(&ok); err != nil {
			internalErr(c, "mkdir.parent", err)
			return
		}
		if !ok {
			response.Fail(c, 400, response.CodeBadRequest, "父目录不存在")
			return
		}
	}

	var f fileRow
	err := h.Pool.QueryRow(ctx, `
		INSERT INTO file_nodes (parent_id, name, type)
		VALUES ($1, $2, 'dir')
		RETURNING id, parent_id, name, type, size, sha256, mime, visibility, exec_public, updated_at, deleted_at`,
		in.ParentID, strings.TrimSpace(in.Name),
	).Scan(&f.ID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.SHA256, &f.Mime,
		&f.Visibility, &f.ExecPublic, &f.UpdatedAt, &f.DeletedAt)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "同目录下已存在同名节点")
		return
	}
	if err != nil {
		internalErr(c, "mkdir", err)
		return
	}
	response.OK(c, f)
}

// listFiles 列目录：目录在前、文件在后，按名字排序。
func (h *handlers) listFiles(c *gin.Context) {
	page, pageSize := paging(c)
	parent, err := optionalInt64(c.Query("parent"))
	if err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "parent 不合法")
		return
	}
	ctx := c.Request.Context()

	var total int64
	if err := h.Pool.QueryRow(ctx,
		`SELECT count(*) FROM file_nodes
		 WHERE deleted_at IS NULL AND parent_id IS NOT DISTINCT FROM $1`, parent,
	).Scan(&total); err != nil {
		internalErr(c, "listFiles.count", err)
		return
	}

	rows, err := h.Pool.Query(ctx, `
		SELECT id, parent_id, name, type, size, sha256, mime, visibility, exec_public, updated_at
		FROM file_nodes
		WHERE deleted_at IS NULL AND parent_id IS NOT DISTINCT FROM $1
		ORDER BY (type <> 'dir'), name
		LIMIT $2 OFFSET $3`, parent, pageSize, (page-1)*pageSize)
	if err != nil {
		internalErr(c, "listFiles.query", err)
		return
	}
	defer rows.Close()

	items := make([]fileRow, 0, pageSize)
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.ID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.SHA256, &f.Mime,
			&f.Visibility, &f.ExecPublic, &f.UpdatedAt); err != nil {
			internalErr(c, "listFiles.scan", err)
			return
		}
		items = append(items, f)
	}
	response.OK(c, response.List{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// upload 单文件上传。核心是 CAS：先算 sha256，已存在就不重复上传，
// 只把 ref_count +1 并新增一条版本记录。
func (h *handlers) upload(c *gin.Context) {
	parent, err := optionalInt64(c.PostForm("parentId"))
	if err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "parentId 不合法")
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "缺少 file 字段")
		return
	}
	if fh.Size > maxUploadSize {
		response.Fail(c, 413, response.CodeBadRequest, "文件超过 100MB，请走分片上传")
		return
	}

	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		name = fh.Filename
	}
	if err := validateName(name); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, err.Error())
		return
	}

	// 边写临时文件边算 sha256：不把整个文件读进内存。
	tmpPath, sha, size, err := hashUpload(fh)
	if err != nil {
		internalErr(c, "upload.hash", err)
		return
	}
	defer os.Remove(tmpPath)

	contentType := fh.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	ctx := c.Request.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "upload.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	fileID, err := h.storeContent(ctx, tx, tmpPath, sha, size, contentType)
	if err != nil {
		internalErr(c, "upload.storeContent", err)
		return
	}

	var f fileRow
	err = tx.QueryRow(ctx, `
		INSERT INTO file_nodes (parent_id, name, type, size, sha256, mime, storage_key)
		VALUES ($1, $2, 'file', $3, $4, $5, $6)
		RETURNING id, parent_id, name, type, size, sha256, mime, visibility, exec_public, updated_at`,
		parent, name, size, sha, contentType, objectKey(sha),
	).Scan(&f.ID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.SHA256, &f.Mime,
		&f.Visibility, &f.ExecPublic, &f.UpdatedAt)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "同目录下已存在同名节点")
		return
	}
	if err != nil {
		internalErr(c, "upload.insertNode", err)
		return
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO file_versions (file_id, version_no, sha256, size, storage_key)
		VALUES ($1, 1, $2, $3, $4)`, f.ID, sha, size, objectKey(sha)); err != nil {
		internalErr(c, "upload.insertVersion", err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE objects SET ref_count = ref_count + 1 WHERE sha256 = $1`, sha); err != nil {
		internalErr(c, "upload.refCount", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "upload.commit", err)
		return
	}

	response.OK(c, gin.H{
		"id":         f.ID,
		"name":       f.Name,
		"size":       size,
		"sha256":     sha,
		"storageKey": objectKey(sha),
		"dedup":      fileID, // true 表示命中了已有对象，没有重复上传，只加了引用计数
	})
}

// storeContent 保证对象存在（不存在则上传到 MinIO 并写 objects 行），返回是否命中已有对象。
// 注意只写 objects 行、不改 ref_count，计数交给调用方在插入版本之后加。
func (h *handlers) storeContent(ctx context.Context, tx pgx.Tx, tmpPath, sha string, size int64, contentType string) (bool, error) {
	var existed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM objects WHERE sha256 = $1)`, sha).Scan(&existed); err != nil {
		return false, err
	}
	if existed {
		return true, nil
	}

	key := objectKey(sha)
	fp, err := os.Open(tmpPath)
	if err != nil {
		return false, err
	}
	defer fp.Close()

	if _, err := h.S3.PutObject(ctx, h.Bucket, key, fp, size,
		minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return false, fmt.Errorf("上传对象失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO objects (sha256, size, ref_count) VALUES ($1, $2, 0)`,
		sha, size); err != nil {
		return false, err
	}
	return false, nil
}

// download 直接流式输出对象内容，并带上学长校验头。
func (h *handlers) download(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	var (
		name, typ, visibility string
		key                   *string
		sha, contentType      *string
		deletedAt             *time.Time
	)
	err := h.Pool.QueryRow(ctx, `
		SELECT name, type, visibility, storage_key, sha256, mime, deleted_at
		FROM file_nodes WHERE id = $1`, id,
	).Scan(&name, &typ, &visibility, &key, &sha, &contentType, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文件不存在")
		return
	}
	if err != nil {
		internalErr(c, "download.select", err)
		return
	}
	if deletedAt != nil || typ != "file" || key == nil {
		response.Fail(c, 404, response.CodeNotFound, "文件不存在或已删除")
		return
	}

	// 私有文件必须登录才能下，公开文件匿名可下（文档里的 /raw 分发就是这个语义）
	if visibility != "public" {
		if _, authed := h.Auth.Soft(c); !authed {
			response.Fail(c, 401, response.CodeUnauth, "该文件非公开，需要登录")
			return
		}
	}

	obj, err := h.S3.GetObject(ctx, h.Bucket, *key, minio.GetObjectOptions{})
	if err != nil {
		internalErr(c, "download.getObject", err)
		return
	}
	defer obj.Close()

	if _, err := obj.Stat(); err != nil {
		internalErr(c, "download.stat", err)
		return
	}

	if contentType != nil && *contentType != "" {
		c.Header("Content-Type", *contentType)
	}
	c.Header("Content-Disposition", contentDisposition(name))
	c.Header("X-Content-Type-Options", "nosniff")
	if sha != nil {
		c.Header("X-Content-Sha256", *sha)
	}
	c.Status(200)
	if _, err := io.Copy(c.Writer, obj); err != nil {
		log.Printf("[warn] download 中断 request_id=%s: %v", c.GetString("request_id"), err)
	}

	// 审计：失败不影响下载
	if _, err := h.Pool.Exec(ctx, `
		INSERT INTO download_logs (file_id, ip, user_agent, kind)
		VALUES ($1, NULLIF($2, '')::inet, $3, 'download')`,
		id, strings.TrimSpace(c.ClientIP()), c.GetHeader("User-Agent")); err != nil {
		log.Printf("[warn] 写下载日志失败: %v", err)
	}
}

// updateFile 重命名 / 移动。移动目录前必须查环。
func (h *handlers) updateFile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var in struct {
		Name       *string `json:"name"`
		ParentID   *int64  `json:"parentId"`   // 0 表示移到根目录
		Visibility *string `json:"visibility"` // public / private，决定前台匿名能不能看到
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if in.Name == nil && in.ParentID == nil && in.Visibility == nil {
		response.Fail(c, 400, response.CodeBadRequest, "name、parentId、visibility 至少给一个")
		return
	}
	if in.Visibility != nil {
		v := strings.TrimSpace(*in.Visibility)
		if v != "public" && v != "private" {
			response.Fail(c, 400, response.CodeBadRequest, "visibility 只能是 public 或 private")
			return
		}
		in.Visibility = &v
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

		// 防环：沿着目标父目录往上找祖先，出现自己就说明要把节点塞进自己的子树。
		var cycle bool
		if err := h.Pool.QueryRow(ctx, `
			WITH RECURSIVE up AS (
			    SELECT id, parent_id FROM file_nodes WHERE id = $2
			    UNION ALL
			    SELECT f.id, f.parent_id FROM file_nodes f JOIN up ON f.id = up.parent_id
			)
			SELECT EXISTS (SELECT 1 FROM up WHERE id = $1)`, id, *parent).Scan(&cycle); err != nil {
			internalErr(c, "updateFile.cycle", err)
			return
		}
		if cycle {
			response.Fail(c, 400, response.CodeBadRequest, "不能把节点移动到自己的子目录")
			return
		}

		var parentOK bool
		if err := h.Pool.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM file_nodes
			              WHERE id = $1 AND type = 'dir' AND deleted_at IS NULL)`,
			*parent).Scan(&parentOK); err != nil {
			internalErr(c, "updateFile.parent", err)
			return
		}
		if !parentOK {
			response.Fail(c, 400, response.CodeBadRequest, "目标父目录不存在")
			return
		}
	}

	var f fileRow
	err := h.Pool.QueryRow(ctx, `
		UPDATE file_nodes SET
		    name       = COALESCE($2, name),
		    parent_id  = CASE WHEN $3::boolean THEN $4::bigint ELSE parent_id END,
		    visibility = COALESCE($5, visibility),
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING id, parent_id, name, type, size, sha256, mime, visibility, exec_public, updated_at`,
		id, in.Name, in.ParentID != nil, parent, in.Visibility,
	).Scan(&f.ID, &f.ParentID, &f.Name, &f.Type, &f.Size, &f.SHA256, &f.Mime,
		&f.Visibility, &f.ExecPublic, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "节点不存在")
		return
	}
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "同目录下已存在同名节点")
		return
	}
	if err != nil {
		internalErr(c, "updateFile", err)
		return
	}
	response.OK(c, f)
}

// uploadVersion 替换文件内容：新增版本，指针指向新对象。
func (h *handlers) uploadVersion(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if _, ok := h.mustBeFile(c, id); !ok {
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "缺少 file 字段")
		return
	}
	if fh.Size > maxUploadSize {
		response.Fail(c, 413, response.CodeBadRequest, "文件超过 100MB")
		return
	}

	tmpPath, sha, size, err := hashUpload(fh)
	if err != nil {
		internalErr(c, "uploadVersion.hash", err)
		return
	}
	defer os.Remove(tmpPath)

	contentType := fh.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	ctx := c.Request.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "uploadVersion.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := h.storeContent(ctx, tx, tmpPath, sha, size, contentType); err != nil {
		internalErr(c, "uploadVersion.storeContent", err)
		return
	}

	var versionNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM file_versions WHERE file_id = $1`, id,
	).Scan(&versionNo); err != nil {
		internalErr(c, "uploadVersion.nextNo", err)
		return
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO file_versions (file_id, version_no, sha256, size, storage_key, note)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
		id, versionNo, sha, size, objectKey(sha), c.PostForm("note")); err != nil {
		internalErr(c, "uploadVersion.insert", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE file_nodes SET sha256 = $2, size = $3, storage_key = $4, mime = $5, updated_at = now()
		WHERE id = $1`, id, sha, size, objectKey(sha), contentType); err != nil {
		internalErr(c, "uploadVersion.updateNode", err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE objects SET ref_count = ref_count + 1 WHERE sha256 = $1`, sha); err != nil {
		internalErr(c, "uploadVersion.refCount", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "uploadVersion.commit", err)
		return
	}
	response.OK(c, gin.H{"id": id, "versionNo": versionNo, "sha256": sha, "size": size})
}

// rollbackFile 回滚到某版本。
// 因为 ref_count 的口径是"file_versions 行数"，版本行一直存在，
// 所以这里只改指针，不需要动 ref_count。
func (h *handlers) rollbackFile(c *gin.Context) {
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

	var sha, key string
	var size int64
	err := h.Pool.QueryRow(ctx, `
		SELECT sha256, size, storage_key FROM file_versions
		WHERE file_id = $1 AND version_no = $2`, id, in.VersionNo).Scan(&sha, &size, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "该版本不存在")
		return
	}
	if err != nil {
		internalErr(c, "rollbackFile.select", err)
		return
	}

	if _, err := h.Pool.Exec(ctx, `
		UPDATE file_nodes SET sha256 = $2, size = $3, storage_key = $4, updated_at = now()
		WHERE id = $1`, id, sha, size, key); err != nil {
		internalErr(c, "rollbackFile.update", err)
		return
	}
	response.OK(c, gin.H{"id": id, "versionNo": in.VersionNo, "sha256": sha, "size": size})
}

// listFileVersions 列出某个文件的历史版本，给后台的"版本 / 回滚"用。
// 版本行在删除文件后仍然保留（对象跟着 ref_count 走），所以这里不判 deleted_at。
func (h *handlers) listFileVersions(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	rows, err := h.Pool.Query(c.Request.Context(), `
		SELECT version_no, sha256, size, note, created_at
		FROM file_versions WHERE file_id = $1
		ORDER BY version_no DESC`, id)
	if err != nil {
		internalErr(c, "listFileVersions", err)
		return
	}
	defer rows.Close()

	type versionRow struct {
		VersionNo int       `json:"versionNo"`
		SHA256    string    `json:"sha256"`
		Size      int64     `json:"size"`
		Note      *string   `json:"note"`
		CreatedAt time.Time `json:"createdAt"`
	}
	items := make([]versionRow, 0, 8)
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.VersionNo, &v.SHA256, &v.Size, &v.Note, &v.CreatedAt); err != nil {
			internalErr(c, "listFileVersions.scan", err)
			return
		}
		items = append(items, v)
	}
	response.OK(c, gin.H{"items": items, "total": len(items)})
}

// deleteFile 软删：节点和整棵子树都标 deleted_at，物理对象不动（版本还在）。
func (h *handlers) deleteFile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	tag, err := h.Pool.Exec(c.Request.Context(), `
		WITH RECURSIVE sub AS (
		    SELECT id FROM file_nodes WHERE id = $1
		    UNION ALL
		    SELECT f.id FROM file_nodes f JOIN sub s ON f.parent_id = s.id
		)
		UPDATE file_nodes SET deleted_at = now(), updated_at = now()
		WHERE id IN (SELECT id FROM sub) AND deleted_at IS NULL`, id)
	if err != nil {
		internalErr(c, "deleteFile", err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.Fail(c, 404, response.CodeNotFound, "节点不存在或已删除")
		return
	}
	response.OK(c, gin.H{"affected": tag.RowsAffected()})
}

// restoreFile 从回收站恢复（连同子树）。
func (h *handlers) restoreFile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	tag, err := h.Pool.Exec(c.Request.Context(), `
		WITH RECURSIVE sub AS (
		    SELECT id FROM file_nodes WHERE id = $1
		    UNION ALL
		    SELECT f.id FROM file_nodes f JOIN sub s ON f.parent_id = s.id
		)
		UPDATE file_nodes SET deleted_at = NULL, updated_at = now()
		WHERE id IN (SELECT id FROM sub) AND deleted_at IS NOT NULL`, id)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "恢复失败：同目录下已有同名节点")
		return
	}
	if err != nil {
		internalErr(c, "restoreFile", err)
		return
	}
	// 什么都没恢复说明节点不存在或没被删，按 404 处理，别回一个空成功
	if tag.RowsAffected() == 0 {
		response.Fail(c, 404, response.CodeNotFound, "节点不存在或未被删除")
		return
	}
	response.OK(c, gin.H{"affected": tag.RowsAffected()})
}

// mustBeFile 校验节点是未删除的文件。
func (h *handlers) mustBeFile(c *gin.Context, id int64) (string, bool) {
	var typ string
	var deleted *time.Time
	err := h.Pool.QueryRow(c.Request.Context(),
		`SELECT type, deleted_at FROM file_nodes WHERE id = $1`, id).Scan(&typ, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "节点不存在")
		return "", false
	}
	if err != nil {
		internalErr(c, "mustBeFile", err)
		return "", false
	}
	if deleted != nil {
		response.Fail(c, 404, response.CodeNotFound, "节点已删除")
		return "", false
	}
	if typ != "file" {
		response.Fail(c, 400, response.CodeBadRequest, "该节点不是文件")
		return "", false
	}
	return typ, true
}

// hashUpload 把上传流落到临时文件，同时算出 sha256。
func hashUpload(fh *multipart.FileHeader) (string, string, int64, error) {
	src, err := fh.Open()
	if err != nil {
		return "", "", 0, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "kb-upload-*")
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	name := tmp.Name()
	tmp.Close()
	if err != nil {
		os.Remove(name)
		return "", "", 0, err
	}
	return name, hex.EncodeToString(h.Sum(nil)), n, nil
}
