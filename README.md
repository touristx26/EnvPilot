# EnvPilot

轻量级环境网关：把你的 Linux 环境（虚拟机、容器、物理机均可，网关不区分）注册到一个网关上，再以 **MCP 协议**暴露给 AI Agent（Trae / Claude / Cursor 等），让 Agent 能够发现环境、执行命令、读写文件。

- **Go 单二进制**：网关与 agent 各一个可执行文件，交叉编译后 `scp` 即部署
- **单 token 认证**：全程只用一个令牌（启动网关时指定），环境注册、在线安装、MCP 调用共用
- **NAT 友好**：环境侧只发起出站 WebSocket 连接，无需在环境上开放任何入站端口
- **在线一键注册**：环境侧一条 `curl | bash` 完成下载、安装、注册

```
AI Agent ──(HTTP + Bearer token)──> /mcp     ──┐
Linux 环境 ──(curl install.sh)────> /bootstrap ├──> envpilot-gateway ──> SQLite（仅环境表）
Linux 环境 ──(出站 WS + token)────> /ws       ──┘
```

---

## 1. 编译

依赖 Go 1.22+，无 CGO（Windows/Linux 均可直接编译）。

```bash
# 网关（当前平台）
go build -o bin/envpilot-gateway ./cmd/gateway

# agent（各平台；bootstrap 分发依赖此命名规则）
go build -o bin/envpilot-agent.exe ./cmd/agent                        # Windows
GOOS=linux GOARCH=amd64 go build -o bin/envpilot-agent-linux-amd64 ./cmd/agent
GOOS=linux GOARCH=arm64 go build -o bin/envpilot-agent-linux-arm64 ./cmd/agent
```

> `--agent-bindir` 目录（默认 `bin/`）下的二进制会被网关自托管分发，文件名必须是 `envpilot-agent-<goos>-<goarch>[.exe]`。Linux 环境通过在线安装时需要 `linux-amd64` / `linux-arm64` 两个文件。

---

## 2. 启动网关

`--agent-bindir` 是相对**工作目录**解析的。第 1 步编译产物都在根目录的 `bin/` 下，因此只要**在项目根目录启动**即可，无需显式指定参数：

```bash
# 目录布局（编译后的项目根目录）：
#   bin/envpilot-gateway
#   bin/envpilot-agent-linux-amd64
#   bin/envpilot-agent-linux-arm64
./bin/envpilot-gateway --addr :8080 --token my-secret-token
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--addr` | `:8080` | HTTP 监听地址 |
| `--token` | `dev-token` | **唯一认证令牌**：agent 注册、安装脚本、MCP 调用共用 |
| `--db` | `envpilot.db` | SQLite 路径（相对工作目录；仅存环境元数据，重启后环境 ID 复用） |
| `--agent-bindir` | `bin` | 各平台 agent 二进制目录（相对工作目录；供在线安装下载） |

网关提供 3 个端点：

| 端点 | 用途 | 认证方式 |
|------|------|----------|
| `/ws` | agent 反向 WebSocket 注册 | REGISTER 帧携带 token |
| `/bootstrap/install.sh`、`/bootstrap/agent` | 在线安装脚本 / 二进制下载 | `?token=` 参数 |
| `/mcp` | MCP Server (Streamable HTTP) | `Authorization: Bearer <token>` |

> 生产环境建议置于 Nginx/Caddy 反代之后终结 TLS（`wss://` / `https`）。

---

## 3. 注册 Linux 环境

### 方式一：在线一键安装（推荐）

在目标 Linux 环境上执行一条命令，自动完成：探测架构 → 下载 agent → root+systemd 则安装为系统服务（开机自启），否则 nohup 后台运行：

```bash
curl -sSL "http://<网关地址>:8080/bootstrap/install.sh?token=my-secret-token&name=nic-01" | bash
```

| 参数 | 必填 | 说明 |
|------|------|------|
| `token` | 是 | 网关令牌 |
| `name` | 否 | 环境名（网关内唯一，重连复用；默认 hostname） |
| `tags` | 否 | 标签，逗号分隔 k=v，如 `role=nettest,lab=nic` |
| `env` | 否 | 注入 agent 进程的环境变量，`K1=V1,K2=V2` 格式 |
| `type` | 否 | 环境类型标签，默认 `linux` |
| `gw` | 否 | 覆盖网关地址（安装脚本里默认取请求方看到的 Host） |

**注入 Docker 远程管理接口**（agent 在环境上执行 `docker` 命令时走该接口）：

```bash
curl -sSL "http://gw:8080/bootstrap/install.sh?token=my-secret-token&name=docker-host-01&tags=role=dockerhost&env=DOCKER_HOST=tcp://10.0.0.5:2375" | bash
# 之后 Agent 通过 exec_command 在该环境执行 docker ps 即操作远程 Docker
```

调试时前台运行（不装服务、不后台）：

```bash
FOREGROUND=1 curl -sSL "http://gw:8080/bootstrap/install.sh?token=...&name=test-01" | bash
```

### 方式二：手动运行（已有 agent 二进制）

```bash
./envpilot-agent --gateway ws://gw:8080/ws --token my-secret-token \
    --name nic-01 --tags role=nettest
# 也可用环境变量：ENVPILOT_GATEWAY / ENVPILOT_TOKEN / ENVPILOT_NAME / ENVPILOT_TYPE / ENVPILOT_TAGS
```

注册成功后 agent 每 10 秒心跳一次，网关 30 秒未收到心跳则标记离线（元数据保留，agent 重连后自动恢复在线）。

---

## 4. AI Agent 接入 MCP

MCP 端点（Streamable HTTP，需客户端支持自定义 HTTP 头）：

```
URL:            http://<网关地址>:8080/mcp
Authorization:  Bearer my-secret-token
```

以 Trae / Claude 等 MCP 客户端的 HTTP 接入配置为例：

```json
{
  "mcpServers": {
    "envpilot": {
      "url": "http://gw:8080/mcp",
      "headers": {
        "Authorization": "Bearer my-secret-token"
      }
    }
  }
}
```

可用工具共 8 个：

| 工具 | 参数 | 说明 |
|------|------|------|
| `list_environments` | `type?`, `online?` | 列出环境（可按类型/在线过滤） |
| `get_environment` | `env_id` | 环境详情（OS、架构、标签、在线状态） |
| `exec_command` | `env_id`, `command`, `timeout_seconds?` | 执行 shell 命令，返回 stdout/stderr/exit_code |
| `read_file` | `env_id`, `path` | 读文本文件（≤2MB） |
| `write_file` | `env_id`, `path`, `content`, `append?` | 写文件（≤20MB，自动创建父目录） |
| `list_dir` | `env_id`, `path` | 列目录 |
| `file_stat` | `env_id`, `path` | 文件元信息 |
| `download_file` | `env_id`, `path` | 取回文件（≤20MB，base64，适合日志/pcap） |

典型使用流程：Agent 先 `list_environments` 找到目标 `env_id`，再 `exec_command` 执行命令或用文件工具读写文件。

---

## 5. 最小验证流程

```bash
# 1. 在项目根目录启动网关（bin/ 默认相对当前目录）
./bin/envpilot-gateway --addr :8080 --token test-token

# 2. 在任一 Linux 环境注册
curl -sSL "http://<gw>:8080/bootstrap/install.sh?token=test-token&name=env-01" | bash

# 3. 用 curl 快速验证 MCP（initialize → initialized → tools/call）
curl -sS -X POST http://gw:8080/mcp \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -H "Authorization: Bearer test-token" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
# 记下响应头 Mcp-Session-Id，随后携带该头调用 tools/call list_environments 即可看到 env-01
```

---

## 6. 安全说明

- **单 token 模型**：持有 token 即可注册环境并访问全部环境的全部工具，无环境隔离；token 更换需重启网关，并同步更新所有 agent 与 MCP 客户端
- **定位**：受信内网工具，命令不做内容过滤；请勿将网关直接暴露到公网
- **建议**：agent 以非 root 运行；网关置于反代 TLS 之后；token 使用足够长的随机串（如 `openssl rand -hex 24`）

---

## 7. 项目结构

```
EnvPilot/
├── cmd/
│   ├── gateway/          # envpilot-gateway 入口
│   └── agent/            # envpilot-agent 入口
├── internal/
│   ├── proto/            # WS 帧定义（JSON 帧 + ID 多路复用）
│   ├── gateway/
│   │   ├── auth/         # 单 token Bearer 中间件
│   │   ├── bootstrap/    # 在线安装（install.sh 生成 + 二进制分发）
│   │   ├── mcpserver/    # MCP Server（Streamable HTTP, 8 工具）
│   │   ├── hub/          # WS 连接管理 / 请求路由
│   │   ├── registry/     # 环境注册表 + 心跳状态机
│   │   └── store/        # SQLite（仅 environments 表）
│   └── agent/
│       ├── client/       # WS 客户端 / 重连 / 心跳
│       ├── exec/         # 命令执行
│       └── files/        # 文件操作
└── docs/architecture.md  # 架构设计文档（10 Slide）
```

主要依赖：[coder/websocket](https://github.com/coder/websocket)、[mark3labs/mcp-go](https://github.com/mark3labs/mcp-go)、[modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（纯 Go 无 CGO）。
