package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

const (
	chunkPartSize   = 8 << 20 // 8MB 一片，S3 要求除最后一片外不小于 5MB
	maxChunkedSize  = 5 << 30 // 单文件 5GB 上限
	maxPartsPerFile = 10000   // S3 的分片上限制
)

// partRec 是 parts JSONB 里的一条记录。
type partRec struct {
	Part int    `json:"part"`
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

// uploadInit 开一个分片会话。
//
// 带了 sha256 且对象已存在时直接"秒传"：只建节点和版本，一个字节都不用传。
func (h *handlers) uploadInit(c *gin.Context) {
	var in struct {
		ParentID *int64 `json:"parentId"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
		Mime     string `json:"mime"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := validateName(in.Name); err != nil {
		response.Fail(c, 400, response.CodeBadRequest, err.Error())
		return
	}
	if in.Size <= 0 || in.Size > maxChunkedSize {
		response.Fail(c, 400, response.CodeBadRequest, "size 需在 1 字节到 5GB 之间")
		return
	}
	if in.ParentID != nil && *in.ParentID == 0 {
		in.ParentID = nil
	}

	sha := strings.ToLower(strings.TrimSpace(in.SHA256))
	if sha != "" && !isHex64(sha) {
		response.Fail(c, 400, response.CodeBadRequest, "sha256 需要 64 位十六进制")
		return
	}

	ctx := c.Request.Context()
	if in.ParentID != nil {
		var ok bool
		if err := h.Pool.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM file_nodes
			              WHERE id = $1 AND type = 'dir' AND deleted_at IS NULL)`,
			*in.ParentID).Scan(&ok); err != nil {
			internalErr(c, "uploadInit.parent", err)
			return
		}
		if !ok {
			response.Fail(c, 400, response.CodeBadRequest, "父目录不存在")
			return
		}
	}

	contentType := in.Mime
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(in.Name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// 秒传：内容已在对象表里，直接落一条节点记录
	if sha != "" {
		var exists bool
		if err := h.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM objects WHERE sha256 = $1)`, sha).Scan(&exists); err != nil {
			internalErr(c, "uploadInit.exists", err)
			return
		}
		if exists {
			nodeID, dedup, err := h.createNodeWithVersion(ctx, in.ParentID, in.Name, sha, in.Size, contentType)
			if isUniqueViolation(err) {
				response.Fail(c, 409, response.CodeBadRequest, "同目录下已存在同名节点")
				return
			}
			if err != nil {
				internalErr(c, "uploadInit.instant", err)
				return
			}
			response.OK(c, gin.H{
				"complete":   true,
				"id":         nodeID,
				"sha256":     sha,
				"size":       in.Size,
				"storageKey": objectKey(sha),
				"dedup":      dedup,
			})
			return
		}
	}

	totalParts := int((in.Size + chunkPartSize - 1) / chunkPartSize)
	if totalParts > maxPartsPerFile {
		response.Fail(c, 400, response.CodeBadRequest, "分片数过多，请调大分片或拆分文件")
		return
	}

	sessionID := randomHex(16)
	tmpKey := "uploads/" + sessionID + "/blob"

	s3UploadID, err := h.Core.NewMultipartUpload(ctx, h.Bucket, tmpKey,
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		internalErr(c, "uploadInit.newMultipart", err)
		return
	}

	if _, err := h.Pool.Exec(ctx, `
		INSERT INTO upload_sessions
		    (id, s3_upload_id, object_key, parent_id, name, size, part_size, total_parts, mime)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		sessionID, s3UploadID, tmpKey, in.ParentID, in.Name, in.Size,
		chunkPartSize, totalParts, contentType); err != nil {
		// 建库失败要把远端 multipart 也撤掉，否则留下垃圾分片
		_ = h.Core.AbortMultipartUpload(ctx, h.Bucket, tmpKey, s3UploadID)
		internalErr(c, "uploadInit.insertSession", err)
		return
	}

	response.OK(c, gin.H{
		"complete":   false,
		"uploadId":   sessionID,
		"partSize":   chunkPartSize,
		"totalParts": totalParts,
	})
}

// uploadPart 上传一片。body 是原始字节，不加 multipart 封装，省一层解析。
func (h *handlers) uploadPart(c *gin.Context) {
	sessionID := c.Param("uploadId")
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 1 {
		response.Fail(c, 400, response.CodeBadRequest, "分片序号不合法")
		return
	}

	ctx := c.Request.Context()
	var (
		s3UploadID, tmpKey string
		totalParts         int
		size, partSize     int64
		status             string
	)
	err = h.Pool.QueryRow(ctx, `
		SELECT s3_upload_id, object_key, total_parts, size, part_size, status
		FROM upload_sessions WHERE id = $1`, sessionID,
	).Scan(&s3UploadID, &tmpKey, &totalParts, &size, &partSize, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "上传会话不存在")
		return
	}
	if err != nil {
		internalErr(c, "uploadPart.select", err)
		return
	}
	if status != "pending" {
		response.Fail(c, 409, response.CodeBadRequest, "上传会话已结束: "+status)
		return
	}
	if index > totalParts {
		response.Fail(c, 400, response.CodeBadRequest, "分片序号超出范围")
		return
	}

	// 分片 ≤8MB，先落临时文件再一次性传给 MinIO：既能校验大小，也不用把 body 读两遍。
	tmpPath, n, err := spoolPart(c.Request.Body, partSize)
	if err != nil {
		internalErr(c, "uploadPart.spool", err)
		return
	}
	defer os.Remove(tmpPath)

	if n == 0 {
		response.Fail(c, 400, response.CodeBadRequest, "分片内容为空")
		return
	}
	if n > partSize {
		response.Fail(c, 413, response.CodeBadRequest, "分片超过约定大小")
		return
	}

	fp, err := os.Open(tmpPath)
	if err != nil {
		internalErr(c, "uploadPart.open", err)
		return
	}
	defer fp.Close()

	info, err := h.Core.PutObjectPart(ctx, h.Bucket, tmpKey, s3UploadID, index,
		fp, n, minio.PutObjectPartOptions{})
	if err != nil {
		internalErr(c, "uploadPart.put", err)
		return
	}

	rec, _ := json.Marshal(partRec{Part: index, ETag: info.ETag, Size: n})
	if _, err := h.Pool.Exec(ctx, `
		UPDATE upload_sessions
		SET parts = parts || $2::jsonb, updated_at = now()
		WHERE id = $1 AND status = 'pending'`,
		sessionID, string(rec)); err != nil {
		internalErr(c, "uploadPart.record", err)
		return
	}

	response.OK(c, gin.H{"part": index, "etag": info.ETag, "size": n})
}

// uploadComplete 合并分片，算 sha256，落节点与版本。
func (h *handlers) uploadComplete(c *gin.Context) {
	sessionID := c.Param("uploadId")
	ctx := c.Request.Context()

	var (
		s3UploadID, tmpKey, name, contentType string
		parentID                              *int64
		totalParts                            int
		size                                  int64
		status                                string
		partsJSON                             []byte
	)
	err := h.Pool.QueryRow(ctx, `
		SELECT s3_upload_id, object_key, name, mime, parent_id,
		       total_parts, size, status, parts
		FROM upload_sessions WHERE id = $1`, sessionID,
	).Scan(&s3UploadID, &tmpKey, &name, &contentType, &parentID,
		&totalParts, &size, &status, &partsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "上传会话不存在")
		return
	}
	if err != nil {
		internalErr(c, "uploadComplete.select", err)
		return
	}
	if status != "pending" {
		response.Fail(c, 409, response.CodeBadRequest, "上传会话已结束: "+status)
		return
	}

	var recs []partRec
	if err := json.Unmarshal(partsJSON, &recs); err != nil {
		internalErr(c, "uploadComplete.parts", err)
		return
	}
	// 同号分片以最后一次为准
	byPart := make(map[int]partRec, len(recs))
	for _, r := range recs {
		byPart[r.Part] = r
	}
	if len(byPart) != totalParts {
		response.Fail(c, 400, response.CodeBadRequest,
			fmt.Sprintf("分片不全：需要 %d，收到 %d", totalParts, len(byPart)))
		return
	}
	complete := make([]minio.CompletePart, 0, totalParts)
	var gotSize int64
	for i := 1; i <= totalParts; i++ {
		r, ok := byPart[i]
		if !ok {
			response.Fail(c, 400, response.CodeBadRequest, "缺少分片 "+strconv.Itoa(i))
			return
		}
		complete = append(complete, minio.CompletePart{PartNumber: i, ETag: r.ETag})
		gotSize += r.Size
	}

	if _, err := h.Core.CompleteMultipartUpload(ctx, h.Bucket, tmpKey, s3UploadID,
		complete, minio.PutObjectOptions{ContentType: contentType}); err != nil {
		internalErr(c, "uploadComplete.complete", err)
		return
	}

	// 合并完才知道整体 sha256：流式读一遍，不占内存
	sha, realSize, err := h.hashObject(ctx, tmpKey)
	if err != nil {
		internalErr(c, "uploadComplete.hash", err)
		return
	}
	if realSize != size || gotSize != size {
		log.Printf("[warn] 分片大小与声明不一致 session=%s 声明=%d 实际=%d", sessionID, size, realSize)
	}

	key := objectKey(sha)
	var existed bool
	if err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM objects WHERE sha256 = $1)`, sha).Scan(&existed); err != nil {
		internalErr(c, "uploadComplete.exists", err)
		return
	}
	if !existed {
		// 临时对象服务端复制到 CAS 路径，不经过应用内存
		if _, err := h.S3.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: h.Bucket, Object: key},
			minio.CopySrcOptions{Bucket: h.Bucket, Object: tmpKey}); err != nil {
			internalErr(c, "uploadComplete.copy", err)
			return
		}
		if _, err := h.Pool.Exec(ctx,
			`INSERT INTO objects (sha256, size, ref_count) VALUES ($1, $2, 0)
			 ON CONFLICT (sha256) DO NOTHING`, sha, realSize); err != nil {
			internalErr(c, "uploadComplete.insertObject", err)
			return
		}
	}
	// 临时对象可以删了，内容已经在 CAS 路径（或本来就有）
	_ = h.S3.RemoveObject(ctx, h.Bucket, tmpKey, minio.RemoveObjectOptions{})

	nodeID, _, err := h.createNodeWithVersion(ctx, parentID, name, sha, realSize, contentType)
	if isUniqueViolation(err) {
		response.Fail(c, 409, response.CodeBadRequest, "同目录下已存在同名节点")
		return
	}
	if err != nil {
		internalErr(c, "uploadComplete.node", err)
		return
	}

	if _, err := h.Pool.Exec(ctx,
		`UPDATE upload_sessions SET status = 'completed', updated_at = now() WHERE id = $1`,
		sessionID); err != nil {
		internalErr(c, "uploadComplete.mark", err)
		return
	}

	response.OK(c, gin.H{
		"complete":   true,
		"id":         nodeID,
		"name":       name,
		"sha256":     sha,
		"size":       realSize,
		"storageKey": key,
		"dedup":      existed,
		"parts":      totalParts,
	})
}

// uploadAbort 主动取消，释放远端分片。
func (h *handlers) uploadAbort(c *gin.Context) {
	sessionID := c.Param("uploadId")
	ctx := c.Request.Context()

	var s3UploadID, tmpKey, status string
	err := h.Pool.QueryRow(ctx,
		`SELECT COALESCE(s3_upload_id, ''), object_key, status FROM upload_sessions WHERE id = $1`,
		sessionID).Scan(&s3UploadID, &tmpKey, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, 404, response.CodeNotFound, "上传会话不存在")
		return
	}
	if err != nil {
		internalErr(c, "uploadAbort.select", err)
		return
	}
	if status != "pending" {
		response.Fail(c, 409, response.CodeBadRequest, "上传会话已结束: "+status)
		return
	}

	if s3UploadID != "" {
		if err := h.Core.AbortMultipartUpload(ctx, h.Bucket, tmpKey, s3UploadID); err != nil {
			log.Printf("[warn] 撤销 multipart 失败 session=%s: %v", sessionID, err)
		}
		_ = h.S3.RemoveObject(ctx, h.Bucket, tmpKey, minio.RemoveObjectOptions{})
	}
	if _, err := h.Pool.Exec(ctx,
		`UPDATE upload_sessions SET status = 'aborted', updated_at = now() WHERE id = $1`,
		sessionID); err != nil {
		internalErr(c, "uploadAbort.mark", err)
		return
	}
	response.OK(c, gin.H{"uploadId": sessionID, "status": "aborted"})
}

// createNodeWithVersion 在事务里落一条文件节点 + 初版版本，并把引用计数 +1。
// 单文件上传、秒传、分片合流都复用它。
func (h *handlers) createNodeWithVersion(ctx context.Context, parentID *int64, name, sha string, size int64, contentType string) (int64, bool, error) {
	// 判断是否已存在同内容对象（调用方可能已插过行，这里以"有没有其它引用"为准）
	var dedup bool
	if err := h.Pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM file_versions v WHERE v.sha256 = $1)`, sha).Scan(&dedup); err != nil {
		return 0, false, err
	}

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	var nodeID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO file_nodes (parent_id, name, type, size, sha256, mime, storage_key)
		VALUES ($1, $2, 'file', $3, $4, $5, $6)
		RETURNING id`,
		parentID, name, size, sha, contentType, objectKey(sha)).Scan(&nodeID); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO file_versions (file_id, version_no, sha256, size, storage_key)
		VALUES ($1, 1, $2, $3, $4)`, nodeID, sha, size, objectKey(sha)); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE objects SET ref_count = ref_count + 1 WHERE sha256 = $1`, sha); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return nodeID, dedup, nil
}

// hashObject 流式读对象算 sha256 与实际大小。
func (h *handlers) hashObject(ctx context.Context, key string) (string, int64, error) {
	obj, err := h.S3.GetObject(ctx, h.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", 0, err
	}
	defer obj.Close()

	hasher := sha256.New()
	n, err := io.Copy(hasher, obj)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// spoolPart 把请求体落到临时文件，同时统计大小并防止超出上限。
func spoolPart(body io.Reader, limit int64) (string, int64, error) {
	tmp, err := os.CreateTemp("", "kb-part-*")
	if err != nil {
		return "", 0, err
	}
	name := tmp.Name()

	// 多读 1 字节，用来判断"超了"
	n, err := io.Copy(tmp, io.LimitReader(body, limit+1))
	tmp.Close()
	if err != nil {
		os.Remove(name)
		return "", 0, err
	}
	return name, n, nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
