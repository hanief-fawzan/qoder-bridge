# qoder-bridge

**OpenAI-compatible proxy for Qoder API.**

Use Qoder models from any AI client — Hermes Agent, Claude, Codex, Continue, Cline, Aider, or any tool that speaks the OpenAI protocol.

```
Client (OpenAI)  →  qoder-bridge  →  Qoder API (COSY)
             ←  OpenAI SSE  ←
```

## Features

- **Drop-in OpenAI proxy** — send standard requests, get standard responses
- **Tool calling** — parses 5 text formats from model output into proper OpenAI tool_calls
- **Multi-PAT rotation** — round-robin with cooldown on quota/rate-limit errors
- **Combo models** — named groups that cascade through model tiers
- **SOCKS5 / HTTP proxy** — route upstream traffic with round-robin
- **API key auth** — optional Bearer token with global toggle
- **Usage tracking** — per-PAT stats, latency, error counts in SQLite
- **TUI** — interactive terminal UI for everything
- **Graceful shutdown** — drains active connections on SIGTERM/SIGINT
- **Stream support** — full SSE streaming with include_usage, finish_reason variants

## Install

```bash
git clone https://github.com/hanief-fawzan/qoder-bridge.git
cd qoder-bridge

# One-shot: build + install + start systemd service
bash install.sh

# Rebuild + restart
bash install.sh restart
```

Binary installs to `~/.local/bin/qoder-bridge`. Systemd service runs on `127.0.0.1:<port>`.

## Configure

```bash
cp .env.example .env
```

```env
# Required: Qoder PATs (one per line)
pt-your-first-pat-here
pt-second-pat-here

# Server port (default: 7100)
QODER_PORT=7101

# Upstream proxy (optional)
QODER_PROXY=socks5://user:pass@127.0.0.1:1080

# Combo models (optional) — format: COMBO_<NAME>=model1,model2,...
COMBO_FAST=qd/efficiency,qd/auto
COMBO_SMART=qd/ultimate,qd/performance,qd/auto
COMBO_DEFAULT=qd/auto
```

## CLI

```
qoder-bridge                  Start daemon (background)
qoder-bridge run              Run in foreground
qoder-bridge run -port 8080   Custom port
qoder-bridge run -env /path   Custom .env path
qoder-bridge stop             Stop daemon
qoder-bridge restart          Restart daemon
qoder-bridge status           Service status
qoder-bridge quota            Check PAT quota
qoder-bridge config           Interactive TUI config
qoder-bridge usage            Usage statistics
qoder-bridge logs             View logs
qoder-bridge update           Git pull + rebuild + restart
qoder-bridge help             Show help
```

## API

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions (stream + non-stream) |
| `GET /v1/models` | Available models |
| `GET /v1/status` | Server status, PAT health, egress IP |
| `GET /v1/quota` | PAT quota usage |
| `GET /v1/combos` | Combo model configurations |
| `GET /health` | Health check |

### Quick test

```bash
curl http://127.0.0.1:7101/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qd/auto","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

### Model routing

| Input | Behavior |
|-------|----------|
| `qd/auto` | Auto-select best available model |
| `qd/ultimate`, `qd/performance`, `qd/efficiency` | Specific Qoder tier |
| `cheap`, `daily`, etc. | Combo: cascade through model list |
| Any string | Pass-through to upstream |

## Tool Calling

Bridge converts OpenAI `tools` schema into a text-based protocol injected into the system prompt. The model emits tool calls in text; bridge parses them into proper OpenAI `tool_calls` format with `finish_reason: "tool_calls"`.

Supported parser formats (priority order):

1. Fenced JSON blocks (primary)
2. XML-style tool_call tags
3. Anthropic XML function_calls/invoke format
4. Bare JSON with brace-counting extraction
5. Inline text format (last resort)

## Streaming

Full SSE streaming with `stream_options.include_usage`. Finish reasons: `tool_calls`, `stop`, `error`. Panic recovery prevents client hangs. Context cancellation on client disconnect.

## Auth (optional)

API key auth is off by default. Enable via TUI (config menu > API Keys). Generate keys, toggle global auth on/off. Keys stored in SQLite.

## Architecture

```
HTTP API
  ↓
Router (model / combo resolution)
  ↓
PAT Pool (round-robin + cooldown)
  ↓
Qoder API (COSY signing + WAF encoding)
  ↓
Response Parser (text + native tool_calls)
  ↓
OpenAI SSE → Client
```

## Build

```bash
go build -o qoder-bridge .                              # standard
go build -trimpath -ldflags="-s -w" -o qoder-bridge .   # optimized
go test -race -count=1 ./...                             # tests
```

## License

MIT
