package api

import (
	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
)

// runMaintenance 手工触发一次清理（回收站过期删除 + 孤儿对象 GC + 僵死上传作废）。
// 定时任务每 12 小时自己跑，这个接口用于运维和测试。
func (h *handlers) runMaintenance(c *gin.Context) {
	if h.Maint == nil {
		response.Fail(c, 503, response.CodeDependency, "清理任务未初始化")
		return
	}
	res, err := h.Maint.RunOnce(c.Request.Context())
	if err != nil {
		internalErr(c, "runMaintenance", err)
		return
	}
	response.OK(c, res)
}
