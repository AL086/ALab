package api

import (
	"log"

	"blog-server/internal/auth"
	"blog-server/internal/maintenance"
	"blog-server/internal/response"
	"blog-server/internal/search"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

// Deps 是所有 handler 共享的依赖。
type Deps struct {
	Pool          *pgxpool.Pool
	S3            *minio.Client
	Core          *minio.Core // 分片上传用底层客户端
	Bucket        string
	PublicBaseURL string
	Auth          *auth.Manager
	Search        *search.Client
	Maint         *maintenance.Job
}

type handlers struct {
	Deps
}

// Register 把路由挂到给定的 group 上（通常是 /api/v1）。
//
// 读写分成两个 group：读接口匿名可用，写接口一律要 JWT。
// 这样以后加接口时不用逐个想"这个要不要登录"，默认就落在正确的一侧。
func Register(r *gin.RouterGroup, d Deps) {
	h := &handlers{Deps: d}

	// ---- 公开读 ----
	r.GET("/search", h.search)
	r.GET("/posts", h.listPosts)
	r.GET("/posts/:id", h.getPost)
	r.GET("/categories", h.listCategories)
	r.GET("/tags", h.listTags)
	// 前台匿名浏览"公开仓库"：只给 visibility=public 的节点，私有内容一律不露
	r.GET("/pub/files", h.publicFiles)
	// 下载单独处理：公开文件匿名可下，私有文件要登录（handler 内判断）
	r.GET("/files/:id/download", h.download)
	// 只读分发的短链：/s/ 下载、/raw/ 在线看（白名单 + 限流 + 审计，服务端绝不执行内容）
	r.GET("/s/:id", h.shareDownload)
	r.GET("/raw/:id", h.rawPreview)

	// ---- 需要登录 ----
	a := r.Group("")
	a.Use(h.Auth.Middleware())

	// 身份与运维
	a.GET("/auth/me", h.me)
	a.POST("/admin/reindex", h.reindex)
	a.POST("/admin/run-maintenance", h.runMaintenance)

	// 文章
	a.POST("/posts", h.createPost)
	a.PUT("/posts/:id", h.updatePost)
	a.POST("/posts/:id/publish", h.publishPost)
	a.POST("/posts/:id/rollback", h.rollbackPost)
	a.PUT("/posts/:id/tags", h.setPostTags)
	a.PUT("/posts/:id/status", h.setPostStatus) // draft / archived（下线用，上线走 publish）
	a.DELETE("/posts/:id", h.deletePost)        // 进回收站，30 天后由清理任务物理删除
	a.POST("/posts/:id/restore", h.restorePost)
	a.GET("/posts/:id/revisions", h.listRevisions)

	// 分类与标签
	a.POST("/categories", h.createCategory)
	a.PUT("/categories/:id", h.updateCategory)
	a.DELETE("/categories/:id", h.deleteCategory)
	a.POST("/tags", h.createTag)
	a.PUT("/tags/:id", h.updateTag)
	a.DELETE("/tags/:id", h.deleteTag)

	// 文件仓库
	a.POST("/files/dir", h.mkdir)
	a.GET("/files", h.listFiles)
	a.POST("/files/upload", h.upload)
	// 打包下载：?ids=1,2,3 或 ?dir=<目录id>（递归），流式 zip 不落临时文件
	a.GET("/files/zip", h.zipFiles)
	a.PATCH("/files/:id", h.updateFile)
	a.GET("/files/:id/versions", h.listFileVersions)
	a.POST("/files/:id/versions", h.uploadVersion)
	a.POST("/files/:id/assets", h.uploadAsset)
	a.GET("/files/:id/text", h.getFileText) // 在线编辑：读当前内容（只开放文本类）
	a.PUT("/files/:id/text", h.putFileText) // 在线编辑：存成新版本，可回滚
	a.POST("/files/:id/rollback", h.rollbackFile)
	a.DELETE("/files/:id", h.deleteFile)
	a.POST("/files/:id/restore", h.restoreFile)

	// 分片上传
	a.POST("/files/upload/init", h.uploadInit)
	a.PUT("/files/upload/:uploadId/part/:index", h.uploadPart)
	a.POST("/files/upload/:uploadId/complete", h.uploadComplete)
	a.DELETE("/files/upload/:uploadId", h.uploadAbort)

	// 登录本身不需鉴权
	r.POST("/auth/login", h.login)
}

// RegisterShortLinks 把只读分发的三个接口再挂一份到根路径。
//
// 原因：Markdown 正文里插图写的是 `![](/raw/<id>)`，如果只有 /api/v1/raw/<id>，
// 浏览器按相对根解析就会 404（图片全裂）。这里挂别名，权限判断仍走同一个 handler。
func RegisterShortLinks(r *gin.Engine, d Deps) {
	RegisterShortLinksOn(r.Group(""), d)
}

// RegisterShortLinksOn 供测试或其它 RouterGroup 复用。
func RegisterShortLinksOn(g *gin.RouterGroup, d Deps) {
	h := &handlers{Deps: d}
	g.GET("/raw/:id", h.rawPreview)
	g.GET("/s/:id", h.shareDownload)
	g.GET("/files/:id/download", h.download)
}

// internalErr 统一日志与对外文案：细节只进日志，不回给客户端。
func internalErr(c *gin.Context, action string, err error) {
	log.Printf("[error] %s request_id=%s: %v", action, c.GetString("request_id"), err)
	response.Fail(c, 500, response.CodeInternal, "服务内部错误")
}
