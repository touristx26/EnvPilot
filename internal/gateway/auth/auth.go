// Package auth 提供 MCP 端点的单 token Bearer 认证中间件。
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Middleware 校验请求头 Authorization: Bearer <token>（与网关启动时 --token 一致）。
func Middleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validToken(token, bearerToken(r)) {
			http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validToken 常数时间比较，防时序侧信道。
func validToken(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// bearerToken 从 Authorization 头提取 Bearer token。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
