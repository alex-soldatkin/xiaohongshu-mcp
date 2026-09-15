package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// authMiddleware 静态 Bearer Token 鉴权中间件，Token 为空时关闭鉴权。
func authMiddleware(token string) gin.HandlerFunc {
	expectedToken := []byte(token)

	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}

		scheme, credentials, found := strings.Cut(c.GetHeader("Authorization"), " ")
		credentials = strings.TrimLeft(credentials, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(credentials), expectedToken) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			respondError(c, http.StatusUnauthorized, "UNAUTHORIZED", "未授权", nil)
			c.Abort()
			return
		}

		c.Next()
	}
}

// corsMiddleware CORS 中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// errorHandlingMiddleware 错误处理中间件
func errorHandlingMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		logrus.Errorf("服务器内部错误: %v, path: %s", recovered, c.Request.URL.Path)

		respondError(c, http.StatusInternalServerError, "INTERNAL_ERROR",
			"服务器内部错误", recovered)
	})
}

// forceRefreshMiddleware lets an HTTP caller bypass the read-through cache,
// the way force_refresh does for an MCP tool call (issue #7).
//
// Two spellings because the read endpoints are a mix of GET and POST: a query
// parameter is natural on a GET and does not disturb a POST body, and a header
// works for any method and for clients that build their URLs elsewhere.
func forceRefreshMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !truthy(c.Query("force_refresh")) && !truthy(c.GetHeader("X-Force-Refresh")) {
			c.Next()
			return
		}

		c.Request = c.Request.WithContext(withForceRefresh(c.Request.Context(), true))
		c.Next()
	}
}

// truthy accepts the usual spellings of yes. An absent value is false, and so
// is anything unrecognised: forcing a live fetch costs a page load and the
// account's read budget, so it happens only when it was clearly asked for.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
