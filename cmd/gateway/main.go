// envpilot-gateway 网关入口：聚合环境连接（WS）、在线安装（bootstrap）与 MCP Server。
// 全程单一 token 认证：agent 注册、bootstrap 安装脚本、MCP 调用共用启动时指定的令牌。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"envpilot/internal/gateway/auth"
	"envpilot/internal/gateway/bootstrap"
	"envpilot/internal/gateway/hub"
	"envpilot/internal/gateway/mcpserver"
	"envpilot/internal/gateway/registry"
	"envpilot/internal/gateway/store"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	token := flag.String("token", "dev-token", "唯一认证令牌（agent 注册 / bootstrap / MCP 共用）")
	dbPath := flag.String("db", "envpilot.db", "SQLite 数据库文件路径（仅存环境元数据）")
	agentBinDir := flag.String("agent-bindir", "bin", "各平台 agent 二进制目录（供 /bootstrap 在线安装下载），文件名: envpilot-agent-<goos>-<goarch>[.exe]")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 持久层：仅环境元数据
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("[gateway] failed to open store %s: %v", *dbPath, err)
	}
	defer st.Close()

	reg := registry.New(st)
	reg.StartSweeper(ctx, 5*time.Second, 30*time.Second)

	h := hub.New(*token, reg)

	mux := http.NewServeMux()
	mux.Handle("/ws", h)                                    // agent 反向连接入口
	mux.Handle("/bootstrap/", bootstrap.New(*agentBinDir))  // 在线一键注册（install.sh + 二进制下载）
	mux.Handle("/mcp", auth.Middleware(*token,              // MCP (Streamable HTTP)，单 token Bearer 认证
		server.NewStreamableHTTPServer(mcpserver.New(reg, h))))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[gateway] listening on %s (db=%s, ws=/ws, bootstrap=/bootstrap, mcp=/mcp)", *addr, *dbPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[gateway] server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("[gateway] shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
