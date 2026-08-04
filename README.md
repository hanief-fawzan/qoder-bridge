# 🌉 qoder-bridge

> **OpenAI-compatible proxy for Qoder API** — use Qoder models from any AI client.

![AI Coded](https://img.shields.io/badge/AI%20Coded-Generated%20with%20AI-blue?style=flat-square)
![Use with precaution](https://img.shields.io/badge/⚠️-Use%20with%20precaution-yellow?style=flat-square)
![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat-square&logo=go)
![License](https://img.shields.io/badge/License-MIT-green?style=flat-square)

Hermes Agent · Claude · Codex · Continue · Cline · Aider — any client that speaks the OpenAI protocol.

```
┌────────────────┐      ┌──────────────────┐      ┌──────────────┐
│  Client        │      │  qoder-bridge    │      │  Qoder API   │
│  (OpenAI)      │ ───→ │  COSY signing    │ ───→ │  (COSY)      │
│                │ ←─── │  Tool parsing    │ ←─── │              │
│                │      │  PAT rotation    │      │              │
└────────────────┘      └──────────────────┘      └──────────────┘
```

---

## ✨ Features

| Feature | Description |
|---------|-------------|
| 🔌 **Drop-in proxy** | Standard OpenAI `/v1/chat/completions` — works out of the box |
| 🛠️ **Tool calling** | Parses 5 text formats from model output → proper OpenAI `tool_calls` |
| 🔄 **Multi-PAT rotation** | Round-robin with cooldown on quota / rate-limit errors |
| 📦 **Combo models** | Named groups that cascade through model tiers |
| 🌐 **SOCKS5 / HTTP proxy** | Route upstream traffic, multi-proxy round-robin |
| 🔑 **API key auth** | Optional Bearer token with global toggle |
| 📊 **Usage tracking** | Per-PAT stats, latency, error counts in SQLite |
| 🖥️ **TUI** | Interactive terminal UI for all configuration |
| 📡 **Full SSE streaming** | `stream_options.include_usage`, all `finish_reason` variants |
| 🛡️ **Panic recovery** | Never leaves client hanging |
| ⚡ **Graceful shutdown** | Drains active connections on SIGTERM/SIGINT |

---

## 🚀 Quick Start

```bash
git clone https://github.com/hanief-fawzan/qoder-bridge.git
cd qoder-bridge

# One-shot: build + install + start systemd service
bash install.sh

# Rebuild + restart
bash install.sh restart
```

Binary: `~/.local/bin/qoder-bridge` · Service: `127.0.0.1:<port>`

---

## ⚙️ Configure

```bash
cp .env.example .env
```

That's it! Only PATs and port needed:

```env
# Required: Qoder PATs (one per line)
pt-your-first-pat-here
pt-second-pat-here

# Server port (default: 7100)
QODER_PORT=7101

# Combo models (optional) — format: COMBO_<NAME>=model1,model2,...
# COMBO_FAST=qd/efficiency,qd/auto
# COMBO_SMART=qd/ultimate,qd/performance,qd/auto
# COMBO_DEFAULT=qd/auto
```

Everything else (proxy, API keys, auth, strategy, domain, delay) is configured via the **TUI**.

---

## 🖥️ TUI

Run `./qoder-bridge` without arguments for the interactive menu:

```
┌─ qoder-bridge ─────────────────────────┐
│                                         │
│  📋 PATs          Add, remove, list     │
│  🔑 API Keys      Generate, toggle      │
│  📊 Usage         Per-PAT stats, latency │
│  📦 Combos        Model group config     │
│  🌐 Proxy         SOCKS5 / HTTP setup    │
│  🌍 Domain        Custom API domain      │
│  ⏱️  Request Delay Throttle requests      │
│  🎯 Strategy      Round-robin / other    │
│  📡 Endpoints     Test API endpoints     │
│  🔄 Update        Git pull + rebuild     │
│  ↩️  Restart       Restart service        │
│                                         │
└─────────────────────────────────────────┘
```

---

## 📡 API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/chat/completions` | POST | Chat completions (stream + non-stream) |
| `/v1/models` | GET | Available models |
| `/v1/status` | GET | Server status, PAT health, egress IP |
| `/v1/quota` | GET | PAT quota usage |
| `/v1/combos` | GET | Combo model configurations |
| `/health` | GET | Health check |

### 💡 Quick test

```bash
curl http://127.0.0.1:7101/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qd/auto","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

### 🎯 Model routing

| Input | Behavior |
|-------|----------|
| `qd/auto` | Auto-select best available model |
| `qd/ultimate`, `qd/performance`, `qd/efficiency` | Specific Qoder tier |
| `cheap`, `daily`, etc. | Combo: cascade through model list |
| Any string | Pass-through to upstream |

---

## 🛠️ Tool Calling

Bridge converts OpenAI `tools` schema into a text-based protocol injected into the system prompt. The model emits tool calls in text; bridge parses them into proper OpenAI `tool_calls` format with `finish_reason: "tool_calls"`.

### Supported parser formats (priority order)

| # | Format | Example |
|---|--------|---------|
| 1 | Fenced JSON blocks | ` ```json {"tool_calls": [...]} ``` ` |
| 2 | XML-style tags | `<tool_call>{"name":"tool","arguments":{...}}</tool_call>` |
| 3 | Anthropic XML | `<function_calls><invoke name="tool">...` |
| 4 | Bare JSON | Brace-counting extraction from raw text |
| 5 | Inline text | `[assistant called tool: name with arguments: {...}]` |

---

## 📡 Streaming

Full SSE streaming with:

- `stream_options.include_usage` — final usage chunk before `[DONE]`
- `finish_reason`: `tool_calls` · `stop` · `error`
- Panic recovery — never leaves client hanging
- Context cancellation — respects client disconnect

---

## 🔑 Auth (optional)

API key auth is **off by default**. Configure via TUI:

1. Open TUI → **API Keys** menu
2. Generate a key
3. Toggle global auth **on**
4. Client sends `Authorization: Bearer sk-xxx`

Keys stored in SQLite. Disabled keys are treated as nonexistent for auth.

---

## 📦 Combo Models

Named model groups configured via TUI → **Combos** menu. Bridge tries each model in order:

```
cheap:    qd/efficiency → qd/auto
smart:    qd/ultimate → qd/performance → qd/auto
default:  qd/auto
```

---

## 🏗️ Architecture

```
HTTP API
  ↓
Router (model / combo resolution)
  ↓
PAT Pool (round-robin + cooldown)
  ↓
Qoder API (COSY signing + WAF encoding)
  ↓
Response Parser (5 text formats + native tool_calls)
  ↓
OpenAI SSE → Client
```

### 🔒 Security

- Slowloris protection: `ReadHeaderTimeout: 10s`
- Idle connection cleanup: `IdleTimeout: 120s`
- Body size limit: 10MB (returns 413)
- Graceful shutdown: 15s drain period
- SQLite connection pool capped: 8 open, 4 idle
- PAT credential never logged (masked in output)

---

## 🔧 CLI

```
qoder-bridge                  Start daemon (background)
qoder-bridge run              Run in foreground
qoder-bridge run -port 8080   Custom port
qoder-bridge run -env /path   Custom .env path
qoder-bridge stop             Stop daemon
qoder-bridge restart          Restart daemon
qoder-bridge status           Service status
qoder-bridge quota            Check PAT quota
qoder-bridge config           Interactive TUI
qoder-bridge usage            Usage statistics
qoder-bridge logs             View logs
qoder-bridge update           Git pull + rebuild + restart
qoder-bridge help             Show help
```

---

## 🏗️ Build

```bash
go build -o qoder-bridge .                              # standard
go build -trimpath -ldflags="-s -w" -o qoder-bridge .   # optimized (production)
go test -race -count=1 ./...                             # tests
```

---

## 📄 License

MIT — use however you like.
