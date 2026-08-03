# qoder-bridge

OpenAI-compatible proxy for Qoder API. Translates standard `/v1/chat/completions` requests into Qoder's proprietary wire format, enabling any OpenAI-compatible client to use Qoder models.

## What It Does

    Client (OpenAI format) -> qoder-bridge -> Qoder API (COSY format)
                      <- OpenAI SSE response <-

- Transparent proxy with COSY signing, WAF encoding, PAT rotation
- Text-based tool calling with multi-format parser
- Multi-PAT rotation with cooldown on quota errors
- Combo models: named groups that cascade through tiers
- SOCKS5/HTTP proxy support
- TUI management for PATs, API keys, usage, combos, proxy
- API key auth with global toggle
- Graceful shutdown with connection drain

## Quick Start

    # 1. Configure
    cp .env.example .env
    # Add your PATs: pt-xxx, pt-yyy (one per line)

    # 2. Build and run
    go build -o qoder-bridge .
    ./qoder-bridge run

    # Or install as systemd service
    bash install.sh restart

## Configuration (.env)

    # PATs (one per line, minimum 1)
    pt-your-pat-here
    pt-second-pat-here

    # Proxy (optional, SOCKS5 or HTTP)
    QODER_PROXY=socks5://user:pass@127.0.0.1:1080

    # Port (default: 7100)
    QODER_PORT=7101

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| POST /v1/chat/completions | Chat completions (OpenAI-compatible) |
| GET /v1/models | List available models |
| GET /v1/status | Server status, PAT health, egress IP |
| GET /v1/quota | PAT quota usage |
| GET /v1/combos | List combo model configurations |
| GET /health | Health check |

## Model Routing

| Prefix | Behavior |
|--------|----------|
| qd/auto | Auto-select best model |
| qd/ultimate, qd/performance, etc. | Specific Qoder tier |
| cheap, daily, etc. | Combo: cascade through model list |
| Any model string | Pass-through to upstream |

## Features

### Tool Calling

Bridge converts OpenAI tools schema into text-based tool protocol injected into system prompt.
Qoder model generates tool calls in various text formats. Bridge parses and emits proper
OpenAI tool_calls format with finish_reason: tool_calls.

Supported parser formats (priority order):
1. Fenced JSON blocks (primary)
2. XML-style tool_call tags
3. Anthropic XML function_calls/invoke format
4. Bare JSON with brace-counting extraction (fallback)
5. Inline text format (last resort)

### Streaming

Full SSE streaming with stream_options.include_usage, finish_reason variants,
panic recovery, and context cancellation on client disconnect.

### PAT Rotation

Round-robin with cooldown: 403 code 112 -> 5min, queue/rate-limit -> 2min, 401 -> not retryable.

### Combo Models

Named model groups configured via TUI. Bridge tries each model in order:
  cheap:    qd/efficiency -> qd/auto
  daily:    qd/auto -> qd/performance
  default:  qd/auto

## TUI

Run ./qoder-bridge without arguments for interactive TUI:
- PAT management (add/remove/list)
- API key management (generate/toggle/table view)
- Usage statistics with per-PAT breakdown and average latency
- Combo model configuration
- Proxy configuration

## Build

    # Standard
    go build -o qoder-bridge .

    # Optimized (production)
    go build -trimpath -ldflags="-s -w" -o qoder-bridge .

    # Tests
    go test -race -count=1 ./...

## License

MIT
