# Kiro Bridge (Go)

[中文文档](README_zh.md)

Kiro Bridge is an API gateway that wraps Kiro CLI's CodeWhisperer backend into standard OpenAI and Anthropic-compatible API endpoints. Single Go binary, no runtime dependencies.

## How It Works

```
Client (OpenAI / Anthropic format)
  │
  ▼
Kiro Bridge (protocol conversion + response sanitization)
  │
  ▼
CodeWhisperer Backend (AWS EventStream)
```

The client sends a standard OpenAI or Anthropic request. The gateway converts it to CodeWhisperer's proprietary protocol, parses the AWS EventStream binary response, strips IDE identity injection and tool markup, and returns the result in the original protocol format.

## Prerequisites

- Go 1.25+
- Kiro CLI logged in (the gateway reads auth tokens from Kiro CLI's SQLite database), or use the built-in PKCE login

## Quick Start

```bash
# Install
go install github.com/pinealctx/kiro-bridge-go@latest

# Or build from source
git clone git@github.com:pinealctx/kiro-bridge-go.git
cd kiro-bridge-go
go install .

# Login (if not already logged in via Kiro CLI)
kiro-bridge-go login

# Start (default port 8001)
kiro-bridge-go

# Custom port + debug mode (prints full request/response payloads)
kiro-bridge-go --port 8080 --debug

# Health check
curl http://localhost:8001/health
```

Pre-built binaries for Linux, macOS, and Windows are available on the [Releases](https://github.com/pinealctx/kiro-bridge-go/releases) page.

## API Endpoints

### OpenAI Compatible

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/chat/completions` | Chat completions (streaming and non-streaming) |
| GET | `/v1/models` | List available models |

### Anthropic Compatible

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/messages` | Messages API (streaming and non-streaming) |
| POST | `/v1/messages/count_tokens` | Token counting |

### Utility

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check (includes token validation) |
| GET | `/metrics` | Metrics (placeholder) |

## Usage Examples

### OpenAI Format

```bash
curl http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

### OpenAI Format (Streaming)

```bash
curl http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

### Anthropic Format

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

### With Third-Party Tools

Use Kiro Bridge as an OpenAI/Anthropic-compatible backend for any tool:

```bash
# As a backend for Claude Code
ANTHROPIC_BASE_URL=http://localhost:8001 claude

# As an OpenAI-compatible backend
OPENAI_API_BASE=http://localhost:8001/v1 your-tool
```

## Configuration

Priority: environment variable > `config.toml` > default value.

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8001` | Server port |
| `HOST` | `0.0.0.0` | Listen address |
| `API_KEY` | none | Bearer token auth (no auth if empty) |
| `LOG_LEVEL` | `info` | Log level |
| `KIRO_DB_PATH` | auto-detect | Path to Kiro CLI SQLite database |
| `DEFAULT_MODEL` | `claude-opus-4-6` | Default model |
| `TOKEN_FILE_PATH` | `~/.kiro-bridge/token.json` | PKCE login token storage path |
| `PROFILE_ARN` | none | CodeWhisperer Profile ARN |

You can also create a `config.toml` file:

```toml
port = 8001
api_key = "your-secret-key"
log_level = "debug"
default_model = "claude-sonnet-4.6"

[model_map]
"gpt-4o" = "claude-sonnet-4.6"
"gpt-4" = "claude-opus-4.6"
```

## Model Mapping

All client model names are translated through a built-in mapping table. Supported models include:

- `claude-opus-4.6` / `claude-opus-4-6`
- `claude-sonnet-4.6` / `claude-sonnet-4-6`
- `claude-opus-4.5` / `claude-sonnet-4.5` / `claude-haiku-4.5`
- And more legacy versions

Custom mappings can be added via the `[model_map]` section in `config.toml`.

## Authentication

The gateway supports three token sources (by priority):

1. **PKCE Login** (`kiro-bridge-go login`) — tokens saved to `~/.kiro-bridge/token.json` with auto-refresh
2. **External IdP** (Microsoft OAuth2) — read from Kiro CLI's SQLite database
3. **Legacy Builder ID** — read from Kiro CLI's SQLite database

Tokens are automatically refreshed 5 minutes before expiry, with retry backoff (1s → 3s → 10s) on failure.

## Features

- OpenAI and Anthropic dual-protocol compatibility
- Streaming and non-streaming responses
- Tool use / function calling support
- Image input support (base64 and URL)
- Extended thinking / reasoning support
- Auto-continuation when context usage exceeds 95% (up to 5 rounds)
- Three-layer response sanitization: strips IDE-injected identity info, XML tool markup, and builtin tool calls
- Builtin tool remapping to client-provided tools
- Automatic token refresh with retry
- CORS support for browser access
- CJK-aware token estimation

## Project Structure

```
├── main.go                  # Entry point (serve + login subcommands)
├── config/config.go         # Config loading (env > config.toml > defaults)
├── token/manager.go         # Token management (SQLite + PKCE + dual refresh)
├── cw/
│   ├── client.go            # CodeWhisperer HTTP client with retry
│   ├── eventstream.go       # AWS EventStream binary protocol parser
│   └── converter.go         # OpenAI → CodeWhisperer format conversion
├── sanitizer/
│   ├── sanitizer.go         # Three-layer response sanitization
│   └── remap.go             # Builtin tool remapping logic
├── counter/tokens.go        # Token estimation (CJK heuristic)
├── thinking/
│   ├── config.go            # Thinking mode configuration
│   └── parser.go            # Thinking block parser
└── api/
    ├── server.go            # HTTP server, middleware (CORS, auth, request-id)
    ├── openai.go            # OpenAI-compatible endpoints
    └── anthropic.go         # Anthropic-compatible endpoints
```

## License

MIT
