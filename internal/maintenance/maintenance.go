// Package maintenance 是后台清理任务：回收站过期删除、孤儿对象 GC、僵死上传会话。
//
// 三件事都按"删除只是标记、垃圾晚点收"的思路做：
// 删文件时不动对象存储，等版本行真的没了，GC 再去删 MinIO 里的 blob。
package maintenance

import (
	"context"
	"log"
	"time"

	"blog-server/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

const (
	// 回收站保留期。文档写的是 30 天，这里保持一致。
	trashRetention = 30 * 24 * time.Hour
	// 对象先留 1 小时再回收：给"刚上传还没写版本行"的竞态留缓冲。
	gcGrace = 1 * time.Hour
	// 未完成的上传会话超过这个时间就作废。
	staleUpload = 24 * time.Hour
	// 每批处理的节点数，避免一次锁太久。
	purgeBatch = 500
	// 定时周期。
	runInterval = 12 * time.Hour
)

type Job struct {
	Pool   *pgxpool.Pool
	S3     *minio.Client
	Core   *minio.Core
	Bucket string
}

// Result 是一次清理的结果，接口返回和日志都用它。
type Result struct {
	PurgedNodes    int64 `json:"purgedNodes"`
	PurgedVersions int64 `json:"purgedVersions"`
	PurgedPosts    int64 `json:"purgedPosts"`
	AbortedUploads int64 `json:"abortedUploads"`
	GCDeleted      int64 `json:"gcDeletedObjects"`
	GCErrors       int64 `json:"gcErrors"`
}

// Loop 启动时先跑一次补齐积压，之后每 12 小时一次，直到 ctx 取消。
func (j *Job) Loop(ctx context.Context) {
	j.runAndLog(ctx, "启动")
	t := time.NewTicker(runInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.runAndLog(ctx, "定时")
		}
	}
}

func (j *Job) runAndLog(ctx context.Context, tag string) {
	res, err := j.RunOnce(ctx)
	if err != nil {
		log.Printf("[maintenance] %s清理失败: %v", tag, err)
		return
	}
	if res.PurgedNodes == 0 && res.PurgedPosts == 0 && res.AbortedUploads == 0 && res.GCDeleted == 0 {
		return // 没事就不刷日志
	}
	log.Printf("[maintenance] %s清理完成: 回收站节点=%d 版本=%d 文章=%d 作废上传=%d 回收对象=%d 失败=%d",
		tag, res.PurgedNodes, res.PurgedVersions, res.PurgedPosts, res.AbortedUploads, res.GCDeleted, res.GCErrors)
}

// RunOnce 依次跑三个任务，返回汇总结果。
func (j *Job) RunOnce(ctx context.Context) (Result, error) {
	var res Result

	purged, versions, err := j.PurgeTrash(ctx)
	if err != nil {
		return res, err
	}
	res.PurgedNodes, res.PurgedVersions = purged, versions

	purgedPosts, err := j.PurgePostTrash(ctx)
	if err != nil {
		return res, err
	}
	res.PurgedPosts = purgedPosts

	aborted, err := j.AbortStaleUploads(ctx)
	if err != nil {
		return res, err
	}
	res.AbortedUploads = aborted

	deleted, failed, err := j.GCObjects(ctx)
	if err != nil {
		return res, err
	}
	res.GCDeleted, res.GCErrors = deleted, failed

	return res, nil
}

// PurgePostTrash 物理删除回收站里超过 30 天的文章。
// post_revisions / post_tags 的外键是 ON DELETE CASCADE，会跟着走；
// 全文索引那边在上一次归档/删除时已经发过 delete 事件，这里不用再处理。
func (j *Job) PurgePostTrash(ctx context.Context) (int64, error) {
	tag, err := j.Pool.Exec(ctx,
		`DELETE FROM posts WHERE deleted_at IS NOT NULL AND deleted_at < now() - $1::interval`,
		trashRetention.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PurgeTrash 物理删除软删超过 30 天的节点（含子树）及其版本行。
// 删版本时同步把 objects.ref_count 减回去，剩下的空对象交给 GC。
func (j *Job) PurgeTrash(ctx context.Context) (int64, int64, error) {
	var totalNodes, totalVersions int64

	for i := 0; i < 10; i++ {
		ids, err := j.expiredNodeIDs(ctx)
		if err != nil {
			return totalNodes, totalVersions, err
		}
		if len(ids) == 0 {
			break
		}

		tx, err := j.Pool.Begin(ctx)
		if err != nil {
			return totalNodes, totalVersions, err
		}
		var nodes, versions int64
		if err := func() error {
			// 1) 删版本，顺便统计每个 sha 少了多少引用
			rows, err := tx.Query(ctx, `
				WITH del AS (
				    DELETE FROM file_versions WHERE file_id = ANY($1::bigint[])
				    RETURNING sha256
				)
				SELECT sha256, count(*) FROM del GROUP BY sha256`, ids)
			if err != nil {
				return err
			}
			type dec struct {
				sha string
				n   int64
			}
			decs := make([]dec, 0, 16)
			for rows.Next() {
				var d dec
				if err := rows.Scan(&d.sha, &d.n); err != nil {
					rows.Close()
					return err
				}
				decs = append(decs, d)
				versions += d.n
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, d := range decs {
				if _, err := tx.Exec(ctx, `
					UPDATE objects SET ref_count = GREATEST(ref_count - $2, 0)
					WHERE sha256 = $1`, d.sha, d.n); err != nil {
					return err
				}
			}

			// 2) 删节点
			tag, err := tx.Exec(ctx, `DELETE FROM file_nodes WHERE id = ANY($1::bigint[])`, ids)
			if err != nil {
				return err
			}
			nodes = tag.RowsAffected()
			return nil
		}(); err != nil {
			_ = tx.Rollback(ctx)
			return totalNodes, totalVersions, err
		}
		if err := tx.Commit(ctx); err != nil {
			return totalNodes, totalVersions, err
		}
		totalNodes += nodes
		totalVersions += versions
	}
	return totalNodes, totalVersions, nil
}

// expiredNodeIDs 找出回收站里过期节点的整棵子树。
// 只看"根"节点是不够的：父目录过期时子节点未必自己过期满 30 天，但要一起删。
func (j *Job) expiredNodeIDs(ctx context.Context) ([]int64, error) {
	rows, err := j.Pool.Query(ctx, `
		WITH RECURSIVE expired AS (
		    SELECT id, parent_id, deleted_at FROM file_nodes
		    WHERE deleted_at IS NOT NULL AND deleted_at < now() - $1::interval
		    UNION ALL
		    SELECT f.id, f.parent_id, f.deleted_at FROM file_nodes f
		    JOIN expired e ON f.parent_id = e.id
		)
		SELECT DISTINCT id FROM expired
		LIMIT $2`, trashRetention.String(), purgeBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0, purgeBatch)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AbortStaleUploads 作废超过 24 小时还没完成的分片会话，并释放远端分片。
func (j *Job) AbortStaleUploads(ctx context.Context) (int64, error) {
	rows, err := j.Pool.Query(ctx, `
		SELECT id, COALESCE(s3_upload_id, ''), object_key FROM upload_sessions
		WHERE status = 'pending' AND created_at < now() - $1::interval
		LIMIT 100`, staleUpload.String())
	if err != nil {
		return 0, err
	}
	type stale struct {
		id, s3UploadID, key string
	}
	items := make([]stale, 0, 100)
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.s3UploadID, &s.key); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var n int64
	for _, s := range items {
		if s.s3UploadID != "" && j.Core != nil {
			if err := j.Core.AbortMultipartUpload(ctx, j.Bucket, s.key, s.s3UploadID); err != nil {
				log.Printf("[maintenance] 撤销 multipart 失败 session=%s: %v", s.id, err)
			}
			_ = j.S3.RemoveObject(ctx, j.Bucket, s.key, minio.RemoveObjectOptions{})
		}
		if _, err := j.Pool.Exec(ctx,
			`UPDATE upload_sessions SET status = 'aborted', updated_at = now() WHERE id = $1`,
			s.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// GCObjects 先按版本行把 ref_count 校正一遍（自愈），再删掉"没有版本引用且已过缓冲期"的对象。
func (j *Job) GCObjects(ctx context.Context) (int64, int64, error) {
	// 自愈：ref_count 是冗余字段，以 file_versions 的实际行数为准
	if _, err := j.Pool.Exec(ctx, `
		UPDATE objects o
		SET ref_count = (SELECT count(*) FROM file_versions v WHERE v.sha256 = o.sha256)
		WHERE o.ref_count <> (SELECT count(*) FROM file_versions v WHERE v.sha256 = o.sha256)`); err != nil {
		return 0, 0, err
	}

	rows, err := j.Pool.Query(ctx, `
		SELECT o.sha256 FROM objects o
		WHERE o.created_at < now() - $1::interval
		  AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.sha256 = o.sha256)
		LIMIT 500`, gcGrace.String())
	if err != nil {
		return 0, 0, err
	}
	shas := make([]string, 0, 500)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return 0, 0, err
		}
		shas = append(shas, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	var deleted, failed int64
	for _, sha := range shas {
		if err := j.S3.RemoveObject(ctx, j.Bucket, store.ObjectKey(sha), minio.RemoveObjectOptions{}); err != nil {
			// 对象本来不在不算错（RemoveObject 通常不报），真报错就记下来下轮重试
			log.Printf("[maintenance] 删除对象失败 sha=%s: %v", sha, err)
			failed++
			continue
		}
		if _, err := j.Pool.Exec(ctx, `
			DELETE FROM objects WHERE sha256 = $1
			  AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.sha256 = $1)`, sha); err != nil {
			if err != pgx.ErrNoRows {
				return deleted, failed, err
			}
		}
		deleted++
	}
	return deleted, failed, nil
}
