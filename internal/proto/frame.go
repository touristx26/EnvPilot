// Package proto 定义网关与环境侧 agent 之间 WebSocket 帧协议。
// M1 骨架采用 JSON 帧 + 帧内 ID 多路复用，后续可平滑升级为二进制帧。
package proto

import (
	"encoding/json"
	"time"
)

// 帧类型
const (
	TypeRegister     = "REGISTER"      // agent → 网关：注册环境
	TypeRegisterAck  = "REGISTER_ACK"  // 网关 → agent：注册成功
	TypeHeartbeat    = "HEARTBEAT"     // agent → 网关：心跳
	TypeHeartbeatAck = "HEARTBEAT_ACK" // 网关 → agent：心跳应答
	TypeExecRequest  = "EXEC_REQUEST"  // 网关 → agent：命令执行请求
	TypeExecResult   = "EXEC_RESULT"   // agent → 网关：命令执行结果
	TypeFileRead     = "FILE_READ"     // 网关 → agent：读文件请求
	TypeFileWrite    = "FILE_WRITE"    // 网关 → agent：写文件请求
	TypeListDir      = "LIST_DIR"      // 网关 → agent：列目录请求
	TypeFileStat     = "FILE_STAT"     // 网关 → agent：文件元信息请求
	TypeErr          = "ERR"           // 双向：错误
)

// Frame 是 WS 上传输的统一帧结构。ID 用于请求/应答关联与多路复用。
type Frame struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ErrorPayload 是 TypeErr 帧的负载。
type ErrorPayload struct {
	Message string `json:"message"`
}

// RegisterRequest 是 TypeRegister 帧的负载。
type RegisterRequest struct {
	Token        string            `json:"token"`
	Name         string            `json:"name"`
	Type         string            `json:"type"` // 环境类型标签，如 linux（自由填写）
	Tags         map[string]string `json:"tags,omitempty"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Capabilities []string          `json:"capabilities,omitempty"`
}

// RegisterAck 是 TypeRegisterAck 帧的负载。
type RegisterAck struct {
	EnvID string `json:"env_id"`
}

// Heartbeat 是 TypeHeartbeat 帧的负载。
type Heartbeat struct {
	Metrics map[string]float64 `json:"metrics,omitempty"` // 如 load/cpu，M1 可为空
}

// ExecRequest 是 TypeExecRequest 帧的负载。
type ExecRequest struct {
	Command        string   `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"` // 默认 30
	Cwd            string   `json:"cwd,omitempty"`
	Env            []string `json:"env,omitempty"`
}

// ExecResult 是 TypeExecResult 帧的负载。
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"` // 执行器层面的错误（如超时、无法启动）
}

// FileReadRequest 是 TypeFileRead 帧的负载。
type FileReadRequest struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes,omitempty"` // 超出则报错，防止误读大文件
}

// FileReadResult 是 TypeFileRead 应答的负载。内容以 base64 编码（二进制安全）。
type FileReadResult struct {
	ContentBase64 string `json:"content_base64"`
	Size          int64  `json:"size"`
	Error         string `json:"error,omitempty"`
}

// FileWriteRequest 是 TypeFileWrite 帧的负载。
type FileWriteRequest struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Append        bool   `json:"append,omitempty"` // 追加模式，默认覆盖
}

// FileWriteResult 是 TypeFileWrite 应答的负载。
type FileWriteResult struct {
	BytesWritten int64  `json:"bytes_written"`
	Error        string `json:"error,omitempty"`
}

// ListDirRequest 是 TypeListDir 帧的负载。
type ListDirRequest struct {
	Path string `json:"path"`
}

// DirEntry 目录条目。
type DirEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// ListDirResult 是 TypeListDir 应答的负载。
type ListDirResult struct {
	Entries []DirEntry `json:"entries"`
	Error   string     `json:"error,omitempty"`
}

// FileStatRequest 是 TypeFileStat 帧的负载。
type FileStatRequest struct {
	Path string `json:"path"`
}

// FileStatResult 是 TypeFileStat 应答的负载。
type FileStatResult struct {
	Exists  bool      `json:"exists"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	Mode    string    `json:"mode"` // 八进制权限，如 0644
	ModTime time.Time `json:"mod_time"`
	Error   string    `json:"error,omitempty"`
}

// Marshal 将负载序列化为 json.RawMessage，失败时返回 nil（调用方应保证负载可序列化）。
func Marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
