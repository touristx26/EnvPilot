// Package client 是环境侧 agent 的 WS 客户端：
// 注册 → 心跳 → 接收并执行命令，断线指数退避重连。
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"envpilot/internal/agent/exec"
	"envpilot/internal/agent/files"
	"envpilot/internal/proto"
)

// Config agent 运行配置。
type Config struct {
	Gateway   string // 网关 WS 地址，如 ws://gw:8080/ws
	Token     string // 注册令牌
	Name      string // 环境名（全局唯一，重连复用）
	Type      string // 环境类型标签，如 linux（自由填写）
	Tags      map[string]string
	Heartbeat time.Duration
}

// Run 阻塞运行 agent 主循环（含重连），ctx 取消时退出。
func Run(ctx context.Context, cfg Config) error {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 10 * time.Second
	}
	backoff := time.Second
	for {
		sessionStart := time.Now()
		err := runOnce(ctx, cfg)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("[agent] session ended: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		// 会话存活较久说明是稳定环境，重置退避
		if time.Since(sessionStart) > time.Minute {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

// runOnce 建立一次连接会话，返回时连接已断开。
func runOnce(ctx context.Context, cfg Config) error {
	c, _, err := websocket.Dial(ctx, cfg.Gateway, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()

	// 连接读超时：整个会话生命周期内空闲即断（心跳会带来流量，实际不会空闲）
	// 注册
	var wmu sync.Mutex // 串行化下行写
	var nextID atomic.Int64

	caps := []string{"exec", "files"}

	regReq := proto.RegisterRequest{
		Token:        cfg.Token,
		Name:         cfg.Name,
		Type:         cfg.Type,
		Tags:         cfg.Tags,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Capabilities: caps,
	}
	if err := writeFrame(ctx, c, &wmu, proto.Frame{ID: nextID.Add(1), Type: proto.TypeRegister, Payload: proto.Marshal(regReq)}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// 等待注册应答
	var ack proto.Frame
	if err := readFrame(ctx, c, &ack); err != nil {
		return fmt.Errorf("wait register ack: %w", err)
	}
	if ack.Type == proto.TypeErr {
		var p proto.ErrorPayload
		_ = json.Unmarshal(ack.Payload, &p)
		return fmt.Errorf("register rejected: %s", p.Message)
	}
	if ack.Type != proto.TypeRegisterAck {
		return fmt.Errorf("unexpected frame %q after register", ack.Type)
	}
	var ackPayload proto.RegisterAck
	_ = json.Unmarshal(ack.Payload, &ackPayload)
	log.Printf("[agent] registered as %s (name=%s)", ackPayload.EnvID, cfg.Name)

	// 心跳协程
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(cfg.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_ = writeFrame(hbCtx, c, &wmu, proto.Frame{
					ID:      nextID.Add(1),
					Type:    proto.TypeHeartbeat,
					Payload: proto.Marshal(proto.Heartbeat{}),
				})
			}
		}
	}()

	// 任务执行协程（命令/文件，与读循环解耦，避免长命令阻塞心跳）
	taskCh := make(chan proto.Frame, 8)
	defer close(taskCh)
	go func() {
		for f := range taskCh {
			handleTask(ctx, c, &wmu, f)
		}
	}()

	// 读循环
	for {
		var f proto.Frame
		if err := readFrame(ctx, c, &f); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		switch f.Type {
		case proto.TypeExecRequest, proto.TypeFileRead, proto.TypeFileWrite,
			proto.TypeListDir, proto.TypeFileStat:
			taskCh <- f
		case proto.TypeErr:
			var p proto.ErrorPayload
			_ = json.Unmarshal(f.Payload, &p)
			log.Printf("[agent] gateway error: %s", p.Message)
		default:
			// HEARTBEAT_ACK 等可忽略
		}
	}
}

// handleTask 处理一条下行的任务请求帧并回写应答。
func handleTask(ctx context.Context, c *websocket.Conn, wmu *sync.Mutex, f proto.Frame) {
	switch f.Type {
	case proto.TypeExecRequest:
		var req proto.ExecRequest
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: "invalid EXEC_REQUEST payload"})})
			return
		}
		log.Printf("[agent] exec: %s", req.Command)
		res := exec.Run(req)
		_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeExecResult, Payload: proto.Marshal(res)})

	case proto.TypeFileRead:
		var req proto.FileReadRequest
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: "invalid FILE_READ payload"})})
			return
		}
		log.Printf("[agent] file read: %s", req.Path)
		res := files.Read(req)
		_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeFileRead, Payload: proto.Marshal(res)})

	case proto.TypeFileWrite:
		var req proto.FileWriteRequest
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: "invalid FILE_WRITE payload"})})
			return
		}
		log.Printf("[agent] file write: %s (append=%v)", req.Path, req.Append)
		res := files.Write(req)
		_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeFileWrite, Payload: proto.Marshal(res)})

	case proto.TypeListDir:
		var req proto.ListDirRequest
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: "invalid LIST_DIR payload"})})
			return
		}
		log.Printf("[agent] list dir: %s", req.Path)
		res := files.ListDir(req)
		_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeListDir, Payload: proto.Marshal(res)})

	case proto.TypeFileStat:
		var req proto.FileStatRequest
		if err := json.Unmarshal(f.Payload, &req); err != nil {
			_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: "invalid FILE_STAT payload"})})
			return
		}
		res := files.Stat(req)
		_ = writeFrame(ctx, c, wmu, proto.Frame{ID: f.ID, Type: proto.TypeFileStat, Payload: proto.Marshal(res)})
	}
}

func readFrame(ctx context.Context, c *websocket.Conn, f *proto.Frame) error {
	_, r, err := c.Reader(ctx)
	if err != nil {
		return err
	}
	// 必须读完整条消息，否则后续 Reader 会报 "previous message not read to completion"
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, f)
}

func writeFrame(ctx context.Context, c *websocket.Conn, mu *sync.Mutex, f proto.Frame) error {
	mu.Lock()
	defer mu.Unlock()
	w, err := c.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(f); err != nil {
		return err
	}
	return w.Close()
}
