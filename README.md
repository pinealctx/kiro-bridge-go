# Kiro Bridge (Go)

Kiro Bridge 是一个 API 网关，将 Kiro CLI 的 CodeWhisperer 后端封装为标准的 OpenAI 和 Anthropic 兼容 API 接口。单一 Go 二进制文件，无运行时依赖。

## 工作原理

```
客户端 (OpenAI/Anthropic 格式)
  │
  ▼
Kiro Bridge (协议转换 + 身份清洗)
  │
  ▼
CodeWhisperer 后端 (AWS EventStream)
```

客户端发送标准的 OpenAI 或 Anthropic 格式请求，网关将其转换为 CodeWhisperer 的私有协议，解析 AWS EventStream 二进制流式响应，清除 IDE 身份注入和工具标记，最后以原始协议格式返回给客户端。

## 前置条件

- Go 1.25+
- Kiro CLI 已登录（网关从 Kiro CLI 的 SQLite 数据库读取认证令牌），或使用内置 PKCE 登录

## 快速开始

```bash
# 构建
git install git@github.com:pinealctx/kiro-bridge-go.git@latest

# 或者从源码安装
git clone git@github.com:pinealctx/kiro-bridge-go.git
go install .

# 登录（如果未通过 Kiro CLI 登录）
kiro-bridge-go login

# 启动（默认端口 8001）
kiro-bridge-go

# 指定端口 + 调试模式，调试模式会把请求和响应的所有报文都打印出来
kiro-bridge-go --port 8080 --debug

# 健康检查
curl http://localhost:8001/health
```

## API 端点

### OpenAI 兼容

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | Chat Completions（支持流式和非流式） |
| GET  | `/v1/models` | 列出可用模型 |

### Anthropic 兼容

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/messages` | Messages API（支持流式和非流式） |
| POST | `/v1/messages/count_tokens` | Token 计数 |

### 其他

| 方法 | 路径 | 说明 |
|------|------|------|
| GET  | `/health` | 健康检查（含令牌验证） |
| GET  | `/metrics` | Metrics（占位） |

## 使用示例

### OpenAI 格式

```bash
curl http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

### OpenAI 格式（流式）

```bash
curl http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

### Anthropic 格式

```bash
curl http://localhost:8001/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-key" \
  -d '{
    "model": "claude-sonnet-4.6",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

### 配合第三方工具

可以将 Kiro Bridge 作为 OpenAI 兼容后端接入各种工具：

```bash
# 作为 Claude Code 的 API 后端
ANTHROPIC_BASE_URL=http://localhost:8001 claude

# 作为 OpenAI 兼容客户端的后端
OPENAI_API_BASE=http://localhost:8001/v1 your-tool
```

## 配置

配置优先级：环境变量 > `config.toml` > 默认值

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `PORT` | `8001` | 服务端口 |
| `HOST` | `0.0.0.0` | 监听地址 |
| `API_KEY` | 无 | Bearer Token 认证（留空则不鉴权） |
| `LOG_LEVEL` | `info` | 日志级别 |
| `KIRO_DB_PATH` | 平台自动检测 | Kiro CLI SQLite 数据库路径 |
| `DEFAULT_MODEL` | `claude-opus-4-6` | 默认模型 |
| `TOKEN_FILE_PATH` | `~/.kiro-bridge/token.json` | PKCE 登录令牌存储路径 |
| `PROFILE_ARN` | 无 | CodeWhisperer Profile ARN |

也可以创建 `config.toml` 文件：

```toml
port = 8001
api_key = "your-secret-key"
log_level = "debug"
default_model = "claude-sonnet-4.6"

[model_map]
"gpt-4o" = "claude-sonnet-4.6"
"gpt-4" = "claude-opus-4.6"
```

## 模型映射

所有客户端传入的模型名称都会通过内置映射表转换。支持的模型包括：

- `claude-opus-4.6` / `claude-opus-4-6`
- `claude-opus-4.6-1m` / `claude-opus-4-6-1m`
- `claude-sonnet-4.6` / `claude-sonnet-4-6`
- `claude-sonnet-4.6-1m` / `claude-sonnet-4-6-1m`
- `claude-opus-4.5` / `claude-sonnet-4.5` / `claude-haiku-4.5`
- 以及更多历史版本

可通过 `config.toml` 的 `[model_map]` 添加自定义映射。

## 认证流程

网关支持三种认证令牌来源（按优先级）：

1. **PKCE 登录**（`./kiro-bridge login`）— 令牌保存在 `~/.kiro-bridge/token.json`，支持自动刷新
2. **External IdP**（Microsoft OAuth2）— 从 Kiro CLI SQLite 数据库读取
3. **Legacy Builder ID** — 从 Kiro CLI SQLite 数据库读取

令牌过期前 5 分钟自动刷新，刷新失败时带重试（1s → 3s → 10s）。

## 项目结构

```
├── main.go                  # 入口（serve + login 子命令）
├── config/config.go         # 配置加载（env > config.toml > default）
├── token/manager.go         # 令牌管理（SQLite + PKCE + 双刷新流程）
├── cw/
│   ├── client.go            # CodeWhisperer HTTP 客户端（含重试）
│   ├── eventstream.go       # AWS EventStream 二进制协议解析
│   └── converter.go         # OpenAI → CodeWhisperer 格式转换
├── sanitizer/sanitizer.go   # 三层响应清洗（XML标记/身份泄露/IDE工具）
├── counter/tokens.go        # Token 估算（CJK 启发式）
└── api/
    ├── server.go            # HTTP 服务、中间件（CORS/Auth/RequestID）
    ├── openai.go            # OpenAI 兼容端点
    └── anthropic.go         # Anthropic 兼容端点
```

## 特性

- OpenAI 和 Anthropic 双协议兼容
- 流式和非流式响应
- Tool Use / Function Calling 支持
- 图片输入支持（base64）
- 自动续写（上下文使用率 > 95% 时自动继续，最多 5 轮）
- 响应清洗：自动剥离 IDE 注入的身份信息和工具标记
- 令牌自动刷新与重试
- CORS 支持，可直接从浏览器调用

## License

Private / Internal Use
