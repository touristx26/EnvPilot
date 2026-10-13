// Package mcpserver 将网关能力以 MCP 工具形式暴露给 AI Agent。
// 工具：list_environments / get_environment / exec_command
// 以及文件操作：read_file / write_file / list_dir / file_stat / download_file。
// 端点认证由外层 auth.Middleware 完成（单 token Bearer），工具内不再做环境隔离。
package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"envpilot/internal/gateway/hub"
	"envpilot/internal/gateway/registry"
)

// New 构建 MCP Server 并注册工具。
func New(reg *registry.Registry, h *hub.Hub) *server.MCPServer {
	s := server.NewMCPServer(
		"envpilot-gateway",
		"0.1.0",
		server.WithToolCapabilities(false),
	)

	s.AddTool(listEnvironmentsTool(), listEnvironmentsHandler(reg))
	s.AddTool(getEnvironmentTool(), getEnvironmentHandler(reg))
	s.AddTool(execCommandTool(), execCommandHandler(reg, h))

	s.AddTool(readFileTool(), readFileHandler(reg, h))
	s.AddTool(writeFileTool(), writeFileHandler(reg, h))
	s.AddTool(listDirTool(), listDirHandler(reg, h))
	s.AddTool(fileStatTool(), fileStatHandler(reg, h))
	s.AddTool(downloadFileTool(), downloadFileHandler(reg, h))

	return s
}

// ---------- list_environments ----------

func listEnvironmentsTool() mcp.Tool {
	return mcp.NewTool("list_environments",
		mcp.WithDescription("列出网关上注册的所有环境（Linux 服务器、虚拟机、容器等），支持按类型和在线状态过滤"),
		mcp.WithString("type", mcp.Description("按环境类型过滤，如 linux"),
		),
		mcp.WithBoolean("online", mcp.Description("仅返回在线环境（true）或全部（不填）"),
		),
	)
}

func listEnvironmentsHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envs := reg.List()

		args := req.GetArguments()
		if t, ok := args["type"].(string); ok && t != "" {
			filtered := envs[:0]
			for _, e := range envs {
				if e.Type == t {
					filtered = append(filtered, e)
				}
			}
			envs = filtered
		}
		if v, ok := args["online"].(bool); ok && v {
			filtered := envs[:0]
			for _, e := range envs {
				if e.Online {
					filtered = append(filtered, e)
				}
			}
			envs = filtered
		}

		b, err := json.MarshalIndent(map[string]any{"environments": envs, "count": len(envs)}, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

// ---------- get_environment ----------

func getEnvironmentTool() mcp.Tool {
	return mcp.NewTool("get_environment",
		mcp.WithDescription("获取指定环境的详细信息（OS、架构、标签、能力、在线状态）"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("环境 ID，可通过 list_environments 获取")),
	)
}

func getEnvironmentHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		env, ok := reg.Get(envID)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		b, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

// ---------- exec_command ----------

func execCommandTool() mcp.Tool {
	return mcp.NewTool("exec_command",
		mcp.WithDescription("在指定环境上执行 shell 命令并返回 stdout/stderr/exit_code"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("command", mcp.Required(), mcp.Description("要执行的 shell 命令")),
		mcp.WithNumber("timeout_seconds", mcp.Description("命令超时时间，默认 30 秒，最大 600 秒")),
	)
}

func execCommandHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		command, err := req.RequireString("command")
		if err != nil {
			return mcp.NewToolResultError("command is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		timeout := 30.0
		if v, ok := req.GetArguments()["timeout_seconds"].(float64); ok && v > 0 {
			timeout = min(v, 600)
		}

		res, err := h.Exec(ctx, envID, command, time.Duration(timeout*float64(time.Second)))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

// ---------- 文件操作工具 ----------

// readFileLimit 是 read_file 的文本大小上限（文本场景够用；更大用 download_file）。
const readFileLimit = 2 << 20 // 2MB

// downloadFileLimit 是 download_file 的大小上限（适合日志/pcap 等产物）。
const downloadFileLimit = 20 << 20 // 20MB

func readFileTool() mcp.Tool {
	return mcp.NewTool("read_file",
		mcp.WithDescription("读取环境上的文本文件内容（≤2MB），如配置文件、日志"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
	)
}

func readFileHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("path is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		res, err := h.FileRead(ctx, envID, path, readFileLimit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		content, decErr := base64.StdEncoding.DecodeString(res.ContentBase64)
		if decErr != nil {
			return mcp.NewToolResultError(decErr.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("--- %s (%d bytes) ---\n%s", path, res.Size, string(content))), nil
	}
}

func writeFileTool() mcp.Tool {
	return mcp.NewTool("write_file",
		mcp.WithDescription("将文本内容写入环境上的文件（默认覆盖，可选追加），自动创建父目录"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目标文件路径")),
		mcp.WithString("content", mcp.Required(), mcp.Description("要写入的文本内容")),
		mcp.WithBoolean("append", mcp.Description("追加模式（true）或覆盖模式（默认 false）")),
	)
}

func writeFileHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("path is required"), nil
		}
		content, err := req.RequireString("content")
		if err != nil {
			return mcp.NewToolResultError("content is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}
		appendMode, _ := req.GetArguments()["append"].(bool)

		res, err := h.FileWrite(ctx, envID, path,
			base64.StdEncoding.EncodeToString([]byte(content)), appendMode)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("written %d bytes to %s", res.BytesWritten, path)), nil
	}
}

func listDirTool() mcp.Tool {
	return mcp.NewTool("list_dir",
		mcp.WithDescription("列出环境上指定目录的内容（名称、大小、类型、修改时间）"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("path", mcp.Required(), mcp.Description("目录路径")),
	)
}

func listDirHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("path is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		res, err := h.ListDir(ctx, envID, path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, err := json.MarshalIndent(map[string]any{"path": path, "entries": res.Entries, "count": len(res.Entries)}, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

func fileStatTool() mcp.Tool {
	return mcp.NewTool("file_stat",
		mcp.WithDescription("查询环境上文件/目录的元信息（大小、权限、修改时间）"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件或目录路径")),
	)
}

func fileStatHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("path is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		res, err := h.FileStat(ctx, envID, path)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}
}

func downloadFileTool() mcp.Tool {
	return mcp.NewTool("download_file",
		mcp.WithDescription("从环境取回文件（≤20MB，二进制安全，base64 编码返回），适合取回测试日志、pcap 等产物"),
		mcp.WithString("env_id", mcp.Required(), mcp.Description("目标环境 ID")),
		mcp.WithString("path", mcp.Required(), mcp.Description("文件的绝对路径")),
	)
}

func downloadFileHandler(reg *registry.Registry, h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envID, err := req.RequireString("env_id")
		if err != nil {
			return mcp.NewToolResultError("env_id is required"), nil
		}
		path, err := req.RequireString("path")
		if err != nil {
			return mcp.NewToolResultError("path is required"), nil
		}
		if _, ok := reg.Get(envID); !ok {
			return mcp.NewToolResultError(fmt.Sprintf("environment %s not found", envID)), nil
		}

		res, err := h.FileRead(ctx, envID, path, downloadFileLimit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("file: %s\nsize: %d bytes\ncontent_base64:\n%s",
			path, res.Size, res.ContentBase64)), nil
	}
}
