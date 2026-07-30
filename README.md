# 🌉 qoder-bridge

> **Pure-Go OpenAI-compatible proxy for [Qoder](https://qoder.com) API.**
> Minimal dependencies (pure-Go SQLite, no CGO). Zero cold start. Just works.

**v1.0.0 Stable** — this is a stable release. Expect minimal updates going forward.

Uses COSY signing (RSA-2048 + AES-128-CBC + MD5) directly — **no qodercli, no Node.js, no npm, no WASM.**

---

## ⚡ Performance

| Metric | qodercli (Node.js) | **qoder-bridge (Go)** |
|--------|:-------------------:|:---------------------:|
| 🕐 Cold start | ~9–14s | **~50ms** |
| 💾 RAM usage | ~300 MB spike | **~8 MB constant** |
| 📦 Binary size | 200 MB+ (npm) | **~11 MB** |
| 🔗 Dependencies | Node.js + npm | **none** |
| 📡 Streaming | ✅ | ✅ |

---

## 🚀 Quick Start

### 1️⃣ Get your PAT

Get a Personal Access Token from **[qoder.com/account/integrations](https://qoder.com/account/integrations)**.

It looks like `pt-xxxxxxxxxxxx`.

### 2️⃣ Clone & configure

```bash
git clone https://github.com/hanief-fawzan/qoder-bridge.git
cd qoder-bridge
cp .env.example .env
nano .env   # add your PATs
```

### 3️⃣ Build & start

```bash
# One-command install (builds, installs binary, creates systemd service)
bash install.sh
```

Or manually:

```bash
go build -o qoder-bridge .
./qoder-bridge
```

That's it. Bridge is live on `http://127.0.0.1:7100` running in background.

---

## 📋 Commands

| Command | Description |
|---------|-------------|
| `./qoder-bridge` | Start as background daemon |
| `./qoder-bridge run` | Run in foreground (for systemd/Docker) |
| `./qoder-bridge stop` | Stop the daemon |
| `./qoder-bridge status` | Check if running |
| `./qoder-bridge update` | Pull from git, rebuild, restart |
| `./qoder-bridge quota` | Check PAT quota and exit |
| `./qoder-bridge config` | Manage runtime config (see below) |
| `./qoder-bridge usage` | View token/credit usage (see below) |
| `./qoder-bridge logs` | View request logs (see below) |
| `./qoder-bridge help` | Show help |

---

## ⚙️ Configuration

### `.env` (server settings only)

```env
# ── Qoder PATs (required) ─────────────────────────────
pt-your-first-pat-here
pt-your-second-pat-here

# ── Server ────────────────────────────────────────────
QODER_PORT=7100

# ── PAT rotation strategy ─────────────────────────────
PAT_STRATEGY=round-robin

# ── Combos (optional) ─────────────────────────────────
COMBO_FAST=efficient,lite
COMBO_SMART=ultimate,Kimi-K3,DeepSeek-V4-Pro
```

### Runtime config via DB (`qoder-bridge config`)

Everything else is managed via the built-in SQLite DB — no restart needed for most changes:

```bash
# View all config
qoder-bridge config

# API Key
qoder-bridge config apikey gen       # Generate new sk-* key + auto-enable
qoder-bridge config apikey show      # Show current key
qoder-bridge config apikey on        # Enable auth (key stays, just toggles)
qoder-bridge config apikey off       # Disable auth (key stays, just toggles)
qoder-bridge config apikey clear     # Remove key entirely

# Anti-ban delay
qoder-bridge config delay set 1000   # Random 0-1000ms per request
qoder-bridge config delay off        # Disable

# Domain (for reverse proxy)
qoder-bridge config domain set qoder.example.com
qoder-bridge config domain clear

# Proxy (comma-separated for multi-proxy rotation)
qoder-bridge config proxy set socks5://user:pass@127.0.0.1:1080
qoder-bridge config proxy clear
```

---

## 📊 Usage & Logs

### Token & Credit Usage

```bash
# Today
qoder-bridge usage today

# This week
qoder-bridge usage week

# This month
qoder-bridge usage month

# This year
qoder-bridge usage year

# Custom date range (DD-MM-YYYY)
qoder-bridge usage custom 01-07-2026 30-07-2026
```

Output shows per-PAT, per-model breakdown with estimated tokens and credits.

### Request Logs

```bash
qoder-bridge logs today
qoder-bridge logs week
qoder-bridge logs custom 01-07-2026 30-07-2026
```

Shows: timestamp (WIB + UTC), PAT, model, stream, tokens, credits, status, latency.

### Credit Multipliers (from Qoder docs)

| Model | Standard | Off-Peak (14:00–00:00 UTC) |
|-------|:--------:|:--------------------------:|
| Ultimate | 1.0x | — |
| Kimi-K3 | 1.0x | — |
| DeepSeek-V4-Pro | 1.0x | — |
| Performance | 0.8x | — |
| Kimi-K2.7-Code | 0.8x | — |
| GLM-5.2 | 0.6x | 0.5x |
| Efficient | 0.5x | — |
| Qwen3.8-Max-Preview | 0.5x | 0.01x (98% off) |
| Qwen3.7-Max | 0.5x | 0.1x (80% off) |
| Qwen3.7-Plus | 0.1x | 0.04x (60% off) |
| MiniMax-M3 | 0.5x | — |
| DeepSeek-V4-Flash | 0.3x | — |
| Lite | 0.1x | — |

> Credit ≈ 1,000 tokens at 1x multiplier. Estimates are conservative (standard rates).

### Database

- **Location**: `~/.qoder-bridge/data.db` (SQLite, pure Go, no CGO)
- **Auto-cleanup**: Logs older than 365 days are automatically deleted every hour
- **Size cap**: If DB exceeds ~100MB, oldest 20% of logs are pruned
- **Config table** (`api_key`, `proxy`, `domain`, `delay`) is **never** cleaned up — only `request_logs` are affected by cleanup
- **Runs without DB**: If SQLite fails to init, bridge still works (just no logging/config)

---

## 🧠 Available Models

Prefix is **case-insensitive** — `QD/Auto`, `qd/AUTO`, `Qd/auto` all work.

### 🏷️ Tier Models

| Model ID | Display Name | Description | Credit Multiplier |
|----------|:------------:|-------------|:-----------------:|
| `qd/auto` | Auto | Automatic model selection | 1x |
| `qd/ultimate` | Ultimate | Best quality | 1x |
| `qd/performance` | Performance | High performance | 0.8x |
| `qd/efficient` | Efficient | Balanced | 0.5x |
| `qd/lite` | Lite | Fastest, cheapest | 0.1x |

### 🌐 Frontier Models

| Model ID | Display Name | Type | Credit Multiplier |
|----------|:------------:|------|:-----------------:|
| `qd/qmodel_preview` | Qwen3.8-Max-Preview | Reasoning | 0.5x |
| `qd/qmodel_latest` | Qwen3.7-Max | Reasoning | 0.5x |
| `qd/qmodel` | Qwen3.7-Plus | General | 0.1x |
| `qd/kmodel_latest` | Kimi-K3 | Code | 1.0x |
| `qd/kmodel` | Kimi-K2.7-Code | Code | 0.8x |
| `qd/gm51model` | GLM-5.2 | General | 0.6x |
| `qd/dmodel` | DeepSeek-V4-Pro | Reasoning | 1.0x |
| `qd/dfmodel` | DeepSeek-V4-Flash | Fast | 0.3x |
| `qd/mmodel` | MiniMax-M3 | General | 0.5x |

> 💡 Display names work everywhere: `"model": "Kimi-K3"` auto-maps to `qd/kmodel_latest`.

---

## 🔌 API Endpoints

| Endpoint | Path |
|----------|------|
| Chat | `POST /v1/chat/completions` |
| Models | `GET /v1/models` |
| Quota | `GET /v1/quota` |
| Combos | `GET /v1/combos` |
| Health | `GET /health` |

### Chat (streaming)

```bash
curl -N http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qd/auto",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

### Chat with tools (agent support)

```bash
curl http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Kimi-K3",
    "messages": [{"role": "user", "content": "Search for Go tutorials"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "web_search",
        "description": "Search the web",
        "parameters": {"type": "object", "properties": {"query": {"type": "string"}}}
      }
    }]
  }'
```

> Tool definitions are injected into the system prompt so the LLM knows about available tools. The LLM responds with tool call instructions in its text output. Agent capabilities are preserved even though Qoder doesn't natively support tool calling.

---

## 🎯 Combos

Fallback chains — try first model, on error try next:

```env
COMBO_FAST=efficient,lite
COMBO_SMART=ultimate,Kimi-K3,DeepSeek-V4-Pro
```

Use: `"model": "qd/combo-fast"` or `"model": "combo-fast"`

Display names work inside combos too: `COMBO_SMART=ultimate,Kimi-K3,DeepSeek-V4-Pro`

---

## 🌐 Proxy

```env
# Single
QODER_PROXY=socks5://user:pass@127.0.0.1:1080

# Multi (comma-separated, random rotation per request)
QODER_PROXY=socks5://a:pass1@1.2.3.4:1080,socks5://b:pass2@5.6.7.8:1080
```

Or configure via CLI: `qoder-bridge config proxy set socks5://...`

---

## 🔄 Update & Restart

```bash
./qoder-bridge update    # pull + build + restart
```

After changing `.env`: `./qoder-bridge stop && ./qoder-bridge`

---

## 🐳 Docker

```bash
docker compose up -d
```

---

## 🏗️ Build from Source

```bash
go build -o qoder-bridge .
```

Requires **Go 1.21+**. One external dependency (`golang.org/x/net` for SOCKS5 + `modernc.org/sqlite` for pure-Go SQLite).

---

## 📜 License

**MIT-0** (MIT No Attribution) — do whatever you want, no credit needed. See [LICENSE](LICENSE).
