package web

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed app.html home.html
var files embed.FS

// serve 直接 c.Data 输出，不额外走文件系统；no-cache 方便改完就刷新看到。
func serve(c *gin.Context, name string) {
	b, err := files.ReadFile(name)
	if err != nil {
		c.String(http.StatusInternalServerError, "前端页面未打包")
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// Home 是公开前台：文章、搜索、公开仓库，匿名可看。
func Home(c *gin.Context) { serve(c, "home.html") }

// Index 是后台管理页，需要登录才能写。
func Index(c *gin.Context) { serve(c, "app.html") }
