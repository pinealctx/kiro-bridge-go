# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**Kiro Gateway** — an API gateway that wraps Kiro CLI's CodeWhisperer backend into standard OpenAI and Anthropic-compatible API endpoints.

- **Go implementation** (primary): repo root — single binary, no runtime dependencies
- **Python implementation** (reference): `py_version/kiro-gateway/`

## Go Implementation

### Commands

```bash
# Build
go build -o kiro-gateway .

# Run (port 8000)
./kiro-gateway
./kiro-gateway --debug

# Health check
curl http://localhost:8000/health
```

### Directory Structure

```
├── main.go                  # Entry point
├── config/config.go         # Config (env > config.toml > default)
├── token/manager.go         # TokenManager (SQLite + two refresh flows)
├── cw/
│   ├── client.go            # HTTP client with retry
│   ├── eventstream.go       # AWS EventStream binary parser
│   └── converter.go         # OpenAI → CodeWhisperer conversion
├── sanitizer/sanitizer.go   # Three-layer response sanitization
├── counter/tokens.go        # Token estimation (CJK heuristic)
└── api/
    ├── server.go            # HTTP server, middleware (CORS, auth, request-id)
    ├── openai.go            # /v1/chat/completions, /v1/models
    └── anthropic.go         # /v1/messages, /v1/messages/count_tokens
```

## Python Implementation

All commands run from `py_version/kiro-gateway/`:

```bash
uv sync
uv run kiro-gateway
uv run pytest tests/ -v --ignore=tests/test_e2e.py
```

## Architecture

Request flow:

1. Client sends OpenAI (`/v1/chat/completions`) or Anthropic (`/v1/messages`) request
2. Anthropic format → OpenAI format conversion if needed
3. `converter.go` (`OpenAIToCW()`) transforms to CodeWhisperer's proprietary format:
   - All model names map to the backend model (see `config/config.go` `ModelMap`)
   - System prompt injected as first history turn (CW has no native system role)
   - Anti-prompt prepended to counteract Kiro IDE identity injection
4. `client.go` POSTs to `https://q.us-east-1.amazonaws.com/generateAssistantResponse` with AWS EventStream binary streaming
5. `eventstream.go` parses the binary EventStream response into typed events
6. `sanitizer.go` post-processes: strips IDE tool XML markup, scrubs "Kiro"/"CodeWhisperer"/"Amazon Q" identity references, filters IDE-only tool calls
7. Routes convert back to the requested protocol format

## Key Design Decisions

**Model mapping**: All client model names (including `gpt-4o`, `claude-sonnet-4`, etc.) are accepted and mapped via `config.ModelMap`. The actual backend model used depends on the mapping.

**Ignored parameters**: `temperature`, `top_p`, `stop`, `presence_penalty`, `frequency_penalty`, `max_tokens`, `n`, `seed`, `logprobs` are intentionally not forwarded to CodeWhisperer — it doesn't support them. This is by design, not a bug.

**Auto-continuation**: When `contextUsagePercentage > 0.95`, `shouldAutoContinue()` uses a 3-layer heuristic to detect genuine truncation vs. large-input false positives. Up to 5 continuation rounds, implemented in all 4 request paths (OpenAI/Anthropic × stream/non-stream).

**System prompt sanitization**: CodeWhisperer injects Kiro IDE identity and tool definitions (`readFile`, `fsWrite`, `webSearch`, etc.) into every response. The gateway counteracts this with an anti-prompt in the system message and post-processes responses to strip leaked IDE artifacts.

**chatTriggerType**: Must be `MANUAL` for normal chat (even with tools). Using `AUTO` for regular chat causes a 400 error.

## Configuration

Config priority: env var > `config.toml` > default. Key env vars:

| Var | Default | Description |
|-----|---------|-------------|
| `PORT` | `8000` | Server port |
| `API_KEY` | none | Optional bearer token auth |
| `KIRO_DB_PATH` | platform auto-detect | Path to Kiro CLI SQLite DB |
| `LOG_LEVEL` | `info` | Log level |

## Auth Token Flow

`TokenManager` supports two auth flows:
- **External IdP** (new): `kirocli:external-idp:token` key in SQLite, refreshes via Microsoft OAuth2 token endpoint
- **Legacy Builder ID**: `kirocli:odic:token` key, refreshes via `https://oidc.us-east-1.amazonaws.com/token`

The gateway auto-detects which flow is active. Kiro CLI must be logged in before starting the gateway.
