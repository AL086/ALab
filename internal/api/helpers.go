package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"blog-server/internal/response"
	"blog-server/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// pathID 解析路径上的 :id，失败时已经写好响应。
func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Fail(c, 400, response.CodeBadRequest, "id 不合法")
		return 0, false
	}
	return id, true
}

// paging 读 page / pageSize，默认 1 / 20，上限 100。
func paging(c *gin.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ = strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	return page, pageSize
}

// optionalInt64 读查询串/表单里的可空整数，空串返回 nil。
func optionalInt64(s string) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return nil, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("不是合法整数: %q", s)
	}
	return &v, nil
}

// slugify 把标题压成 URL 友好的 slug：保留字母/数字/汉字，其余折叠成单个连字符。
func slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "post"
	}
	return out
}

// isUniqueViolation 判断是不是唯一约束冲突（Postgres 23505）。
// 注意：部分唯一索引(NULLS NOT DISTINCT, WHERE deleted_at IS NULL)冲突也是 23505。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation 判断外键不存在（23503），用来把"标签不存在"转成 400。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// validateName 校验节点名，挡住目录穿越和路径分隔符。
func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name 不能为空")
	}
	if len(name) > 255 {
		return errors.New("name 过长")
	}
	if name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) ||
		strings.ContainsRune(name, 0) {
		return errors.New(`name 不能包含路径分隔符或 . / ..`)
	}
	return nil
}

// objectKey 内容寻址的存储 key：objects/ab/cd/<sha256>。定义在 store 包里，这里只是转发。
func objectKey(sha string) string { return store.ObjectKey(sha) }

// contentDisposition 生成兼容中文文件名的头：ASCII 兜底 + RFC 5987 的 filename*。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x80 && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

// postTagNames 取某篇文章的标签名（按名字排序），列表与详情都用它填充。
func (h *handlers) postTagNames(ctx context.Context, postID int64) ([]string, error) {
	rows, err := h.Pool.Query(ctx, `
		SELECT t.name FROM post_tags pt JOIN tags t ON t.id = pt.tag_id
		WHERE pt.post_id = $1 ORDER BY t.name`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	names := make([]string, 0, 4)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
