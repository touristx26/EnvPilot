// envpilot-agent 环境侧 agent 入口：向网关注册本环境并长连待命。
// 支持环境变量配置：ENVPILOT_GATEWAY / ENVPILOT_TOKEN / ENVPILOT_NAME /
// ENVPILOT_TYPE / ENVPILOT_TAGS。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"envpilot/internal/agent/client"
)

func main() {
	gateway := flag.String("gateway", envDefault("ENVPILOT_GATEWAY", "ws://127.0.0.1:8080/ws"), "网关 WS 地址")
	token := flag.String("token", envDefault("ENVPILOT_TOKEN", "dev-enroll-token"), "注册令牌")
	name := flag.String("name", envDefault("ENVPILOT_NAME", hostname()), "环境名（网关内唯一，重连复用）")
	envType := flag.String("type", envDefault("ENVPILOT_TYPE", "linux"), "环境类型")
	tags := flag.String("tags", envDefault("ENVPILOT_TAGS", ""), "标签，逗号分隔的 k=v 对，如 role=nettest,arch=x86")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := client.Config{
		Gateway: *gateway,
		Token:   *token,
		Name:    *name,
		Type:    *envType,
		Tags:    parseTags(*tags),
	}
	log.Printf("[agent] connecting to %s as %q (type=%s)", cfg.Gateway, cfg.Name, cfg.Type)
	if err := client.Run(ctx, cfg); err != nil && ctx.Err() == nil {
		log.Fatalf("[agent] exited with error: %v", err)
	}
	log.Println("[agent] exited")
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseTags(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	tags := make(map[string]string)
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && k != "" {
			tags[k] = v
		}
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "env-unknown"
	}
	return h
}
