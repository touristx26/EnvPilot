// Package exec 在环境侧本地执行命令（M1：非交互，聚合输出）。
package exec

import (
	"context"
	"errors"
	osexec "os/exec"
	"runtime"
	"time"

	"envpilot/internal/proto"
)

// Run 执行一条 shell 命令并聚合 stdout/stderr/exit_code。
// 超时后命令被终止，Error 字段说明原因。
func Run(req proto.ExecRequest) proto.ExecResult {
	timeout := 30 * time.Second
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := shellCommand(ctx, req.Command)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	}

	var stdout, stderr bufWriter
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	res := proto.ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err := cmd.Run(); err != nil {
		var exitErr *osexec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			res.Error = "command timed out"
			res.ExitCode = -1
		case errors.As(err, &exitErr):
			res.ExitCode = exitErr.ExitCode()
		default:
			res.Error = err.Error()
			res.ExitCode = -1
		}
	}
	// 再次读取，确保聚合完成
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	return res
}

// shellCommand 按平台选择 shell（Linux 用 sh -c，Windows 便于本地开发测试用 cmd /C）。
func shellCommand(ctx context.Context, command string) *osexec.Cmd {
	if runtime.GOOS == "windows" {
		return osexec.CommandContext(ctx, "cmd", "/C", command)
	}
	return osexec.CommandContext(ctx, "sh", "-c", command)
}

// bufWriter 简单封装 bytes.Buffer，便于扩展（如大小上限、truncation）。
type bufWriter struct {
	data []byte
}

func (b *bufWriter) Write(p []byte) (int, error) {
	// M1 上限 1MB，超出部分丢弃，防止 agent 打爆网关内存
	const max = 1 << 20
	if len(b.data)+len(p) > max {
		n := max - len(b.data)
		if n > 0 {
			b.data = append(b.data, p[:n]...)
		}
		return len(p), nil // 佯装全部写入成功
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bufWriter) String() string {
	return string(b.data)
}
