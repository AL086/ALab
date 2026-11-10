package api

import (
	"context"
	"errors"

	"blog-server/internal/auth"
	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// login 校验口令并签发 JWT。
func (h *handlers) login(c *gin.Context) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Username == "" || in.Password == "" {
		response.Fail(c, 400, response.CodeBadRequest, "需要 username 与 password")
		return
	}

	token, u, err := h.Auth.Login(c.Request.Context(), in.Username, in.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		response.Fail(c, 401, response.CodeUnauth, "用户名或密码不正确")
		return
	}
	if err != nil {
		internalErr(c, "login", err)
		return
	}
	response.OK(c, gin.H{"token": token, "tokenType": "Bearer", "expiresIn": 86400, "user": u})
}

// me 回显当前身份，前端拿它判断登录态是否还有效。
func (h *handlers) me(c *gin.Context) {
	u, _ := c.Get("user")
	response.OK(c, u)
}

// search 调 Meilisearch。未登录只能搜到已发布且公开的文章。
func (h *handlers) search(c *gin.Context) {
	q := c.Query("q")
	page, pageSize := paging(c)

	filter := "status = published"
	if _, ok := h.Auth.Soft(c); !ok {
		filter += " AND visibility = public"
	}

	out, err := h.Search.Search(c.Request.Context(), q, pageSize, (page-1)*pageSize, filter)
	if err != nil {
		internalErr(c, "search", err)
		return
	}

	hits, total := out["hits"], out["estimatedTotalHits"]
	response.OK(c, gin.H{"q": q, "page": page, "pageSize": pageSize, "total": total, "items": hits})
}

// reindex 把所有文章塞进 outbox，让后台任务重建索引。
func (h *handlers) reindex(c *gin.Context) {
	tag, err := h.Pool.Exec(c.Request.Context(),
		`INSERT INTO search_outbox (entity_id, op) SELECT id, 'upsert' FROM posts`)
	if err != nil {
		internalErr(c, "reindex", err)
		return
	}
	response.OK(c, gin.H{"enqueued": tag.RowsAffected(), "note": "后台任务每 2 秒同步一批"})
}

// enqueueSearch 在业务事务里登记一次待同步，保证"库里改了"和"要同步"同生共死。
func enqueueSearch(ctx context.Context, tx pgx.Tx, postID int64, op string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (entity_id, op) VALUES ($1, $2)`, postID, op)
	return err
}
