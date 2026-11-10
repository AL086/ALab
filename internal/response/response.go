// Package response 统一所有接口的返回结构，避免每个 handler 各写一套 gin.H。
//
//	成功：{"code":0,"message":"ok","data":{...},"requestId":"..."}
//	失败：{"code":4001,"message":"参数错误","data":null,"requestId":"..."}
//
// code=0 表示成功；非 0 是业务码，与 HTTP 状态码分开表达。
package response

import "github.com/gin-gonic/gin"

const (
	CodeOK         = 0
	CodeBadRequest = 4001
	CodeUnauth     = 4010
	CodeForbidden  = 4030
	CodeNotFound   = 4040
	CodeInternal   = 5000
	CodeDependency = 5003 // 下游依赖（DB/搜索/对象存储）不可用
)

// Envelope 是所有接口的最外层结构。
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// List 用于分页列表。
type List struct {
	Items    any   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
}

// OK 返回成功（HTTP 200）。
func OK(c *gin.Context, data any) {
	c.JSON(200, Envelope{Code: CodeOK, Message: "ok", Data: data, RequestID: requestID(c)})
}

// Fail 返回失败，HTTP 状态码与业务码分开传。
func Fail(c *gin.Context, httpStatus, code int, msg string) {
	c.AbortWithStatusJSON(httpStatus, Envelope{Code: code, Message: msg, RequestID: requestID(c)})
}

// FailData 用于既要报错又想带上诊断数据的场景（例如 /readyz 列出每个依赖的状态）。
func FailData(c *gin.Context, httpStatus, code int, msg string, data any) {
	c.AbortWithStatusJSON(httpStatus, Envelope{Code: code, Message: msg, Data: data, RequestID: requestID(c)})
}

// SetRequestID 由中间件调用，把本次请求的 ID 存进上下文。
func SetRequestID(c *gin.Context, id string) {
	c.Set("request_id", id)
}

func requestID(c *gin.Context) string {
	return c.GetString("request_id")
}
