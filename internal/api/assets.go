package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Markdown 插图只收这几种；不给 SVG（能带脚本），也不给 TIFF/BMP 这类浏览器不认的。
var imageAllow = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

const (
	// 单张插图上限 8MB：再大就该走分片了，插图没必要
	maxAssetSize = 8 << 20
	// 源文件目录下统一放这个子目录，避免图片散在业务目录里
	assetDirName = "assets"
)

// rawRefRe 匹配正文里的 /raw/<id>（编辑器和前台渲染都用这个写法）
var rawRefRe = regexp.MustCompile(`/raw/(\d+)`)

// uploadAsset 给某个 Markdown 文件上传一张插图。
//
// 设计口径：
//   - 图片就是普通文件，走同一套 CAS + file_nodes，不另建一套存储；
//   - 落在源文件同级的 assets/ 子目录里（首次插图自动建目录）；
//   - 图片可见性跟随源文件：源文件 private 时图片也 private，公开后同步公开。
func (h *handlers) uploadAsset(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if _, ok := h.mustBeFile(c, id); !ok {
		return
	}

	ctx := c.Request.Context()
	var (
		srcName       string
		srcParent     *int64
		srcVisibility string
	)
	if err := h.Pool.QueryRow(ctx, `
		SELECT name, parent_id, visibility FROM file_nodes
		WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&srcName, &srcParent, &srcVisibility); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, 404, response.CodeNotFound, "文件不存在或已删除")
			return
		}
		internalErr(c, "uploadAsset.src", err)
		return
	}

	ext := strings.ToLower(path.Ext(srcName))
	if ext != ".md" && ext != ".markdown" {
		response.Fail(c, 400, response.CodeBadRequest, "只有 Markdown 文件支持插入图片")
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "缺少 file 字段")
		return
	}
	if fh.Size > maxAssetSize {
		response.Fail(c, 413, response.CodeBadRequest, "插图超过 8MB，请压缩后再上传")
		return
	}

	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		name = fh.Filename
	}
	if name == "" {
		name = "image.png"
	}
	imgExt := strings.ToLower(path.Ext(name))
	contentType, allowed := imageAllow[imgExt]
	if !allowed {
		response.Fail(c, 415, response.CodeBadRequest, "只支持 png / jpg / jpeg / gif / webp 插图")
		return
	}
	if err := validateName(name); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, err.Error())
		return
	}
	if ct := fh.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		response.Fail(c, 415, response.CodeBadRequest, "上传内容不是图片")
		return
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(imgExt)
	}

	tmpPath, sha, size, err := hashUpload(fh)
	if err != nil {
		internalErr(c, "uploadAsset.hash", err)
		return
	}
	defer os.Remove(tmpPath)

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "uploadAsset.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	dirID, err := ensureAssetDir(ctx, tx, srcParent)
	if err != nil {
		internalErr(c, "uploadAsset.dir", err)
		return
	}

	// 同目录重名就加时间戳后缀，别因为一张截图卡住整篇文档
	finalName := name
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM file_nodes
		              WHERE parent_id = $1 AND name = $2 AND deleted_at IS NULL)`,
		dirID, finalName).Scan(&exists); err != nil {
		internalErr(c, "uploadAsset.dupCheck", err)
		return
	}
	if exists {
		stem := strings.TrimSuffix(finalName, path.Ext(finalName))
		finalName = fmt.Sprintf("%s-%d%s", stem, time.Now().Unix(), path.Ext(finalName))
	}

	// 同一张图反复粘贴（截图工具常给随机临时名）不该在 assets 里堆一串重复节点：
	// 目录里已经有同样 sha256 的文件就直接复用。
	var reuseID int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM file_nodes
		WHERE parent_id = $1 AND type = 'file' AND sha256 = $2 AND deleted_at IS NULL
		ORDER BY id LIMIT 1`, dirID, sha).Scan(&reuseID); err == nil {
		if cerr := tx.Commit(ctx); cerr != nil {
			internalErr(c, "uploadAsset.commitReuse", cerr)
			return
		}
		var reuseName string
		_ = h.Pool.QueryRow(ctx, `SELECT name FROM file_nodes WHERE id = $1`, reuseID).Scan(&reuseName)
		response.OK(c, gin.H{
			"id":         reuseID,
			"name":       reuseName,
			"size":       size,
			"sha256":     sha,
			"url":        fmt.Sprintf("/raw/%d", reuseID),
			"markdown":   fmt.Sprintf("![%s](/raw/%d)", strings.TrimSuffix(reuseName, path.Ext(reuseName)), reuseID),
			"visibility": srcVisibility,
			"reused":     true,
		})
		return
	}

	if _, err := h.storeContent(ctx, tx, tmpPath, sha, size, contentType); err != nil {
		internalErr(c, "uploadAsset.storeContent", err)
		return
	}

	var fileID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO file_nodes (parent_id, name, type, size, sha256, mime, storage_key, visibility)
		VALUES ($1, $2, 'file', $3, $4, $5, $6, $7)
		RETURNING id`,
		dirID, finalName, size, sha, contentType, objectKey(sha), srcVisibility,
	).Scan(&fileID); err != nil {
		internalErr(c, "uploadAsset.insert", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "uploadAsset.commit", err)
		return
	}

	response.OK(c, gin.H{
		"id":         fileID,
		"name":       finalName,
		"size":       size,
		"sha256":     sha,
		"url":        fmt.Sprintf("/raw/%d", fileID),
		"markdown":   fmt.Sprintf("![%s](/raw/%d)", strings.TrimSuffix(finalName, path.Ext(finalName)), fileID),
		"visibility": srcVisibility,
	})
}

// ensureAssetDir 保证源文件所在目录下有一个 assets/ 子目录，返回它的节点 id。
func ensureAssetDir(ctx context.Context, tx pgx.Tx, parent *int64) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM file_nodes
		WHERE parent_id IS NOT DISTINCT FROM $1 AND name = $2 AND type = 'dir' AND deleted_at IS NULL
		ORDER BY id LIMIT 1`, parent, assetDirName).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO file_nodes (parent_id, name, type)
		VALUES ($1, $2, 'dir') RETURNING id`, parent, assetDirName).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// syncInlineAssets 让正文里引用的图片可见性跟着源文件走。
//
// 不这么做就会出现"文档公开了、图还是私有"，前台读者看到一片裂图。
// 反过来在私有阶段也不会提前把图暴露出去。
func syncInlineAssets(ctx context.Context, tx pgx.Tx, content, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return nil
	}
	ids := rawRefIDs(content)
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE file_nodes SET visibility = $2, updated_at = now()
		WHERE id = ANY($1) AND type = 'file' AND deleted_at IS NULL AND visibility <> $2`, ids, visibility)
	return err
}

// rawRefIDs 从正文里抽出所有 /raw/<id> 的 id，去重后返回。
func rawRefIDs(content string) []int64 {
	matches := rawRefRe.FindAllStringSubmatch(content, -1)
	seen := map[int64]bool{}
	out := make([]int64, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
