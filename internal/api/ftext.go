package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path"
	"strings"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

// 在线编辑只开放"文本"这类文件；PDF/图片/压缩包一律只能下载。
// 白名单而不是黑名单：宁可少开，也不要让二进制内容进 textarea。
var textEditAllow = map[string]string{
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".txt":      "text/plain; charset=utf-8",
	".log":      "text/plain; charset=utf-8",
	".csv":      "text/plain; charset=utf-8",
	".tsv":      "text/plain; charset=utf-8",
	".yaml":     "text/plain; charset=utf-8",
	".yml":      "text/plain; charset=utf-8",
	".sh":       "text/plain; charset=utf-8",
	".ini":      "text/plain; charset=utf-8",
	".conf":     "text/plain; charset=utf-8",
	".toml":     "text/plain; charset=utf-8",
	".sql":      "text/plain; charset=utf-8",
	".py":       "text/plain; charset=utf-8",
	".go":       "text/plain; charset=utf-8",
	".js":       "text/plain; charset=utf-8",
	".css":      "text/plain; charset=utf-8",
	".xml":      "text/plain; charset=utf-8",
	".json":     "application/json; charset=utf-8",
	".html":     "text/plain; charset=utf-8",
	".htm":      "text/plain; charset=utf-8",
}

// 在线编辑的大小上限：超过就老老实实下载，别把浏览器和内存拖垮。
const maxTextEdit = 512 << 10

// editableMime 判断某个文件名能不能在线编辑，返回落库用的 mime。
func editableMime(name string) (string, bool) {
	m, ok := textEditAllow[strings.ToLower(path.Ext(name))]
	return m, ok
}

type fileTextRow struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Content    string `json:"content"`
	VersionNo  int    `json:"versionNo"`
	VersionCnt int    `json:"versionCount"`
	Mime       string `json:"mime"`
}

// getFileText 读出文件的当前内容给编辑器。只允许白名单类型，私有文件本来就要登录。
func (h *handlers) getFileText(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if _, ok := h.mustBeFile(c, id); !ok {
		return
	}
	ctx := c.Request.Context()

	var (
		name, mimeType string
		size           int64
		sha            *string
		key            *string
	)
	err := h.Pool.QueryRow(ctx, `
		SELECT name, COALESCE(mime, ''), size, sha256, storage_key
		FROM file_nodes WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&name, &mimeType, &size, &sha, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "文件不存在或已删除")
		return
	}
	if err != nil {
		internalErr(c, "getFileText.select", err)
		return
	}

	want, ok := editableMime(name)
	if !ok {
		response.Fail(c, 415, response.CodeBadRequest, "这个类型不支持在线编辑，只能下载")
		return
	}
	if size > maxTextEdit {
		response.Fail(c, 413, response.CodeBadRequest, "文件超过 512KB，请下载后本地编辑再上传")
		return
	}
	if key == nil {
		response.Fail(c, 404, response.CodeNotFound, "文件还没有内容")
		return
	}

	obj, err := h.S3.GetObject(ctx, h.Bucket, *key, minio.GetObjectOptions{})
	if err != nil {
		internalErr(c, "getFileText.getObject", err)
		return
	}
	defer obj.Close()

	// 多读 1 字节：如果实际比库里记的大，说明元数据不对，宁可拒掉也不硬读
	buf := make([]byte, size+1)
	n, rerr := io.ReadFull(obj, buf)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		internalErr(c, "getFileText.read", rerr)
		return
	}
	if int64(n) > size {
		response.Fail(c, 413, response.CodeBadRequest, "对象实际大小与记录不一致，拒绝在线编辑")
		return
	}

	var versionNo, versionCnt int
	if err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT version_no FROM file_versions
		                 WHERE file_id = $1 AND sha256 = $2 ORDER BY version_no DESC LIMIT 1), 0),
		       (SELECT count(*) FROM file_versions WHERE file_id = $1)`, id, *sha).
		Scan(&versionNo, &versionCnt); err != nil {
		internalErr(c, "getFileText.versions", err)
		return
	}

	row := fileTextRow{
		ID: id, Name: name, Size: int64(n), Content: string(buf[:n]),
		VersionNo: versionNo, VersionCnt: versionCnt, Mime: mimeType,
	}
	if sha != nil {
		row.SHA256 = *sha
	}
	_ = want
	response.OK(c, row)
}

// putFileText 把编辑器里的内容存成新版本。
//
// 走的是和 uploadVersion 完全一样的路径：算 sha → 落 CAS 对象 → 插版本行 → 改节点指针 → ref_count+1。
// 所以在线编辑天然可回滚，历史版本不会丢。
func (h *handlers) putFileText(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if _, ok := h.mustBeFile(c, id); !ok {
		return
	}
	var in struct {
		Content string `json:"content"`
		Note    string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}

	ctx := c.Request.Context()
	var name, visibility string
	if err := h.Pool.QueryRow(ctx, `
		SELECT name, visibility FROM file_nodes WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&name, &visibility); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, 404, response.CodeNotFound, "文件不存在或已删除")
			return
		}
		internalErr(c, "putFileText.name", err)
		return
	}
	mimeType, ok := editableMime(name)
	if !ok {
		response.Fail(c, 415, response.CodeBadRequest, "这个类型不支持在线编辑，只能下载")
		return
	}
	if len(in.Content) > maxTextEdit {
		response.Fail(c, 413, response.CodeBadRequest, "内容超过 512KB，请本地编辑后再上传")
		return
	}

	// 统一按 UTF-8 落库；编辑器里是 CRLF 的话顺手归一，避免每次保存都多一个空版本
	content := strings.ReplaceAll(in.Content, "\r\n", "\n")
	if content == "" && len(in.Content) > 0 {
		content = in.Content
	}
	raw := []byte(content)
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	size := int64(len(raw))

	tmp, err := os.CreateTemp("", "kb-text-*")
	if err != nil {
		internalErr(c, "putFileText.temp", err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		internalErr(c, "putFileText.writeTemp", err)
		return
	}
	tmp.Close()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		internalErr(c, "putFileText.begin", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := h.storeContent(ctx, tx, tmpPath, sha, size, mimeType); err != nil {
		internalErr(c, "putFileText.storeContent", err)
		return
	}

	var versionNo int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM file_versions WHERE file_id = $1`, id).
		Scan(&versionNo); err != nil {
		internalErr(c, "putFileText.nextNo", err)
		return
	}

	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = "在线编辑"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO file_versions (file_id, version_no, sha256, size, storage_key, note)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, versionNo, sha, size, objectKey(sha), note); err != nil {
		internalErr(c, "putFileText.insert", err)
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE file_nodes SET sha256 = $2, size = $3, storage_key = $4, mime = $5, updated_at = now()
		WHERE id = $1`, id, sha, size, objectKey(sha), mimeType); err != nil {
		internalErr(c, "putFileText.updateNode", err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE objects SET ref_count = ref_count + 1 WHERE sha256 = $1`, sha); err != nil {
		internalErr(c, "putFileText.refCount", err)
		return
	}
	// 正文里引用的插图跟着这份文档的可见性走：文档私有就不外泄，公开了前台才不裂图
	if err := syncInlineAssets(ctx, tx, content, visibility); err != nil {
		internalErr(c, "putFileText.syncAssets", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalErr(c, "putFileText.commit", err)
		return
	}

	log.Printf("[info] 在线编辑保存 file=%d version=%d size=%d", id, versionNo, size)
	response.OK(c, gin.H{"id": id, "versionNo": versionNo, "sha256": sha, "size": size})
}
