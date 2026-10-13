// Package hub 维护环境侧 agent 的反向 WebSocket 长连接，
// 并将网关侧请求（如命令执行）路由到对应环境。
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"envpilot/internal/gateway/registry"
	"envpilot/internal/proto"
)

// Hub 管理所有 agent 连接与待应答请求。
type Hub struct {
	token  string // 单一认证令牌（启动时指定）
	reg    *registry.Registry
	conns  map[string]*agentConn // envID → 连接
	pending map[int64]chan proto.Frame
	nextID  atomic.Int64
	mu     sync.Mutex
}

// agentConn 单个环境的 WS 连接。send 集中序列化所有下行帧。
type agentConn struct {
	envID string
	c     *websocket.Conn
	send  chan proto.Frame
}

// New 创建 Hub。token 为网关启动时指定的唯一认证令牌。
func New(token string, reg *registry.Registry) *Hub {
	return &Hub{
		token:   token,
		reg:     reg,
		conns:   make(map[string]*agentConn),
		pending: make(map[int64]chan proto.Frame),
	}
}

// ServeHTTP 处理 agent 的 WS 接入：注册 → 心跳/命令结果分发。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx := r.Context()

	// 第一帧必须是 REGISTER（10 秒内）
	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var f proto.Frame
	if err := readFrame(regCtx, c, &f); err != nil {
		h.reject(ctx, c, "expected REGISTER frame")
		cancel()
		return
	}
	cancel()

	if f.Type != proto.TypeRegister {
		h.reject(ctx, c, "first frame must be REGISTER")
		return
	}
	var req proto.RegisterRequest
	if err := json.Unmarshal(f.Payload, &req); err != nil || req.Name == "" {
		h.reject(ctx, c, "invalid REGISTER payload")
		return
	}
	// 校验注册令牌（与启动时 --token 配置值常数时间比较）
	if subtle.ConstantTimeCompare([]byte(h.token), []byte(req.Token)) != 1 {
		h.reject(ctx, c, "invalid token")
		log.Printf("[hub] rejected register from %q: invalid token", req.Name)
		return
	}

	envID := h.reg.Register(req)

	// 同名环境重连时，顶掉旧连接
	h.mu.Lock()
	if old, ok := h.conns[envID]; ok {
		old.c.CloseNow()
		delete(h.conns, envID)
	}
	ac := &agentConn{envID: envID, c: c, send: make(chan proto.Frame, 32)}
	h.conns[envID] = ac
	h.mu.Unlock()

	log.Printf("[hub] environment %s (%s, %s/%s) connected", envID, req.Name, req.OS, req.Arch)

	defer func() {
		h.mu.Lock()
		if cur, ok := h.conns[envID]; ok && cur == ac {
			delete(h.conns, envID)
			h.reg.SetOffline(envID)
		}
		h.mu.Unlock()
		log.Printf("[hub] environment %s (%s) disconnected", envID, req.Name)
	}()

	// 下行写泵：串行化所有写操作
	go func() {
		for {
			select {
			case out := <-ac.send:
				wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
				err := writeFrame(wctx, c, out)
				wcancel()
				if err != nil {
					c.CloseNow()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// 注册应答
	ac.send <- proto.Frame{ID: f.ID, Type: proto.TypeRegisterAck, Payload: proto.Marshal(proto.RegisterAck{EnvID: envID})}

	// 上行读循环
	for {
		var in proto.Frame
		if err := readFrame(ctx, c, &in); err != nil {
			return
		}
		switch in.Type {
		case proto.TypeHeartbeat:
			h.reg.MarkSeen(envID)
			select {
			case ac.send <- proto.Frame{ID: in.ID, Type: proto.TypeHeartbeatAck}:
			case <-ctx.Done():
				return
			}
		default:
			// 按 ID 交付给等待中的调用方（如 Exec）
			if ch := h.takePending(in.ID); ch != nil {
				select {
				case ch <- in:
				default:
				}
			}
		}
	}
}

// Exec 在指定环境上同步执行命令并返回结果。
func (h *Hub) Exec(ctx context.Context, envID, command string, timeout time.Duration) (*proto.ExecResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	payload, err := h.roundtrip(ctx, envID, proto.TypeExecRequest,
		proto.ExecRequest{Command: command, TimeoutSeconds: int(timeout.Seconds())},
		timeout+30*time.Second)

	var res *proto.ExecResult
	if err == nil {
		var r proto.ExecResult
		if jsonErr := json.Unmarshal(payload, &r); jsonErr != nil {
			err = fmt.Errorf("invalid EXEC_RESULT payload: %w", jsonErr)
		} else {
			res = &r
		}
	}
	return res, err
}

// FileRead 读取环境上的文件（base64 内容）。
func (h *Hub) FileRead(ctx context.Context, envID, path string, maxBytes int64) (*proto.FileReadResult, error) {
	payload, err := h.roundtrip(ctx, envID, proto.TypeFileRead,
		proto.FileReadRequest{Path: path, MaxBytes: maxBytes}, 60*time.Second)

	var res *proto.FileReadResult
	if err == nil {
		var r proto.FileReadResult
		if jsonErr := json.Unmarshal(payload, &r); jsonErr != nil {
			err = fmt.Errorf("invalid FILE_READ result payload: %w", jsonErr)
		} else if r.Error != "" {
			err = errors.New(r.Error)
		} else {
			res = &r
		}
	}
	return res, err
}

// FileWrite 写入环境上的文件（base64 内容）。
func (h *Hub) FileWrite(ctx context.Context, envID, path string, contentBase64 string, append bool) (*proto.FileWriteResult, error) {
	payload, err := h.roundtrip(ctx, envID, proto.TypeFileWrite,
		proto.FileWriteRequest{Path: path, ContentBase64: contentBase64, Append: append}, 60*time.Second)

	var res *proto.FileWriteResult
	if err == nil {
		var r proto.FileWriteResult
		if jsonErr := json.Unmarshal(payload, &r); jsonErr != nil {
			err = fmt.Errorf("invalid FILE_WRITE result payload: %w", jsonErr)
		} else if r.Error != "" {
			err = errors.New(r.Error)
		} else {
			res = &r
		}
	}
	return res, err
}

// ListDir 列出环境上的目录。
func (h *Hub) ListDir(ctx context.Context, envID, path string) (*proto.ListDirResult, error) {
	payload, err := h.roundtrip(ctx, envID, proto.TypeListDir,
		proto.ListDirRequest{Path: path}, 30*time.Second)

	var res *proto.ListDirResult
	if err == nil {
		var r proto.ListDirResult
		if jsonErr := json.Unmarshal(payload, &r); jsonErr != nil {
			err = fmt.Errorf("invalid LIST_DIR result payload: %w", jsonErr)
		} else if r.Error != "" {
			err = errors.New(r.Error)
		} else {
			res = &r
		}
	}
	return res, err
}

// FileStat 查询环境上文件的元信息。
func (h *Hub) FileStat(ctx context.Context, envID, path string) (*proto.FileStatResult, error) {
	payload, err := h.roundtrip(ctx, envID, proto.TypeFileStat,
		proto.FileStatRequest{Path: path}, 30*time.Second)

	var res *proto.FileStatResult
	if err == nil {
		var r proto.FileStatResult
		if jsonErr := json.Unmarshal(payload, &r); jsonErr != nil {
			err = fmt.Errorf("invalid FILE_STAT result payload: %w", jsonErr)
		} else if r.Error != "" {
			err = errors.New(r.Error)
		} else {
			res = &r
		}
	}
	return res, err
}

// roundtrip 向环境发送一个请求帧并等待同 ID 的应答帧，返回应答负载。
// 错误可能来自：环境离线、连接繁忙、ctx 取消、超时、agent 回 ERR 帧。
func (h *Hub) roundtrip(ctx context.Context, envID, reqType string, payload any, timeout time.Duration) (json.RawMessage, error) {
	h.mu.Lock()
	ac, ok := h.conns[envID]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("environment %s is offline or not registered", envID)
	}

	id := h.nextID.Add(1)
	ch := make(chan proto.Frame, 1)
	h.addPending(id, ch)
	defer h.takePending(id)

	// 下发请求
	select {
	case ac.send <- proto.Frame{ID: id, Type: reqType, Payload: proto.Marshal(payload)}:
	case <-time.After(5 * time.Second):
		return nil, errors.New("environment connection busy")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 等待应答
	select {
	case in := <-ch:
		if in.Type == proto.TypeErr {
			var p proto.ErrorPayload
			_ = json.Unmarshal(in.Payload, &p)
			if p.Message == "" {
				p.Message = "unknown agent error"
			}
			return nil, errors.New(p.Message)
		}
		return in.Payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for agent %s response", reqType)
	}
}

func (h *Hub) reject(ctx context.Context, c *websocket.Conn, msg string) {
	_ = writeFrame(ctx, c, proto.Frame{Type: proto.TypeErr, Payload: proto.Marshal(proto.ErrorPayload{Message: msg})})
}

func (h *Hub) addPending(id int64, ch chan proto.Frame) {
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
}

func (h *Hub) takePending(id int64) chan proto.Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.pending[id]
	if ok {
		delete(h.pending, id)
	}
	return ch
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

func writeFrame(ctx context.Context, c *websocket.Conn, f proto.Frame) error {
	w, err := c.Writer(ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(f); err != nil {
		return err
	}
	return w.Close()
}
