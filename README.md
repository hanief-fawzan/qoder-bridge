# 🌉 qoder-bridge

Pure-Go OpenAI-compatible proxy for [Qoder](https://qoder.com) API. Uses COSY signing (RSA-2048 + AES-128-CBC + MD5) directly — **no qodercli, no Node.js, no cold start.**

---

## ⚡ Performance

| Metric | qodercli (Node.js) | **qoder-bridge (Go)** |
|--------|:-------------------:|:---------------------:|
| Cold start | ~9–14s | **~50ms** |
| RAM usage | ~300 MB spike | **~8 MB constant** |
| Binary size | 200 MB+ (npm) | **~10 MB** |
| Dependencies | Node.js + npm | **none** |

---

## 🚀 Quick Start

### 1. Get your PAT

Get a Personal Access Token from **[qoder.com/account/integrations](https://qoder.com/account/integrations)**.

It looks like `pt-xxxxxxxxxxxx`.

### 2. Clone & configure

```bash
git clone https://github.com/hanief-fawzan/qoder-bridge.git
cd qoder-bridge
cp .env.example .env
nano .env   # add your PATs
```

Your `.env`:

```env
# ── Qoder PATs (required) ──────────────────────────
pt-your-first-pat-here
pt-your-second-pat-here
```

### 3. Build & start

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

```
qoder-bridge started (PID 12345)
  logs:    tail -f /home/user/.qoder-bridge.log
  stop:    qoder-bridge stop
  status:  qoder-bridge status
```

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
| `./qoder-bridge help` | Show help |

### Flags (for `run` and default mode)

| Flag | Default | Description |
|------|:-------:|-------------|
| `-env` | auto | Path to `.env` file (checks `./.env`, then `~/.env`) |
| `-port` | `7100` | Listen port (overrides `QODER_PORT` in `.env`) |
| `-pats` | | Comma-separated PAT list (overrides `.env`) |

---

## ⚙️ Configuration

All config is via `.env` file.

```env
# ── Qoder PATs (required) ──────────────────────────
# One PAT per line. Get yours from https://qoder.com/account/integrations
pt-your-first-pat-here
pt-your-second-pat-here

# ── Server ──────────────────────────────────────────
QODER_PORT=7100

# ── PAT rotation strategy ───────────────────────────
# round-robin (default): cycles through PATs in order
# random: picks a random PAT each request
PAT_STRATEGY=round-robin

# ── API Key (optional) ─────────────────────────────
# Protect the bridge with a Bearer token.
# If set, clients MUST send: Authorization: Bearer sk-xxx
# If not set, the bridge is open (no auth required).
# Recommended when exposing via Caddy/domain.
#QODER_API_KEY=sk-your-secret-key-here

# ── Combos (optional) ───────────────────────────────
# Format: COMBO_<NAME>=model1,model2,model3
# First model is primary. On error, tries the next.
# Models auto-prefix with qd/ (case-insensitive).
# Unknown model names log a warning but still try.
# Examples:
# COMBO_FAST=efficient,lite
# COMBO_SMART=ultimate,qmodel_preview,dmodel
# COMBO_CHEAP=lite,efficient,dfmodel
# COMBO_DEFAULT=auto,ultimate,performance

# ── Proxy (optional) ───────────────────────────────
# Route all API traffic through a proxy.
# Supports: socks5://, socks5h://, http://, https://
# Priority: QODER_PROXY > HTTPS_PROXY > ALL_PROXY
#QODER_PROXY=socks5://user:pass@127.0.0.1:1080
```

---

## 🔌 API Endpoints

### 💬 Chat Completions (OpenAI-compatible)

```bash
curl http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qd/auto",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

**With API key:**

```bash
curl http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{"model": "qd/auto", "messages": [{"role": "user", "content": "Hello!"}]}'
```

**Streaming:**

```bash
curl -N http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qd/auto",
    "messages": [{"role": "user", "content": "Count to 5"}],
    "stream": true
  }'
```

### 📋 List Models

```bash
curl http://127.0.0.1:7100/v1/models
```

### 📊 Check Quota

```bash
curl http://127.0.0.1:7100/v1/quota

# Or via CLI:
./qoder-bridge quota
```

### 🎯 List Combos

```bash
curl http://127.0.0.1:7100/v1/combos
```

### ❤️ Health Check

```bash
curl http://127.0.0.1:7100/health
```

---

## 🧠 Available Models

All models use the `qd/` prefix. Prefix is **case-insensitive** — `QD/Auto`, `qd/AUTO`, `Qd/auto` all work.

### Tier Models (auto-routed by Qoder)

| Model ID | Display Name | Description |
|----------|:------------:|-------------|
| `qd/auto` | Auto | Automatic model selection (recommended) |
| `qd/ultimate` | Ultimate | Best quality, most expensive |
| `qd/performance` | Performance | High performance |
| `qd/efficient` | Efficient | Balanced cost/quality |
| `qd/lite` | Lite | Fastest, cheapest |

### Frontier Models (specific LLMs)

| Model ID | Display Name | Type |
|----------|:------------:|------|
| `qd/qmodel_preview` | Qwen3.8-Max-Preview | Reasoning |
| `qd/qmodel_latest` | Qwen3.7-Max | Reasoning |
| `qd/qmodel` | Qwen3.7-Plus | General |
| `qd/kmodel_latest` | Kimi-K3 | Code |
| `qd/kmodel` | Kimi-K2.7-Code | Code |
| `qd/gm51model` | GLM-5.2 | General |
| `qd/dmodel` | DeepSeek-V4-Pro | Reasoning |
| `qd/dfmodel` | DeepSeek-V4-Flash | Fast |
| `qd/mmodel` | MiniMax-M3 | General |

### Model Aliases

Any prefix works — all auto-convert to `qd/`:

```
qd/auto     → auto ✅
QD/Auto     → auto ✅
qoder/auto  → auto ✅
apore/auto  → auto ✅ (logged as warning)
```

---

## 🎯 Combos

Combos are **fallback chains** — try the first model, and if it fails, automatically try the next.

### Configure

```env
COMBO_FAST=efficient,lite
COMBO_SMART=ultimate,qmodel_preview,dmodel
COMBO_CHEAP=lite,efficient,dfmodel
```

### Use

```bash
# Auto-prefixed with qd/
curl ... -d '{"model": "qd/combo-fast", ...}'

# Also works without prefix
curl ... -d '{"model": "combo-fast", ...}'

# Case-insensitive
curl ... -d '{"model": "COMBO_FAST", ...}'
```

### How it works

```
Request: qd/combo-smart
  → try qd/ultimate ... error
  → try qd/qmodel_preview ... success → return
```

If **all models fail**, returns the last error. Unknown model names are still tried (Qoder may add new models) — a warning is logged.

---

## 🌐 Proxy Support

Route all Qoder API traffic through a proxy. Supports `socks5://`, `socks5h://`, `http://`, `https://`.

```env
# SOCKS5 proxy (e.g., MicroWARP, Cloudflare WARP)
QODER_PROXY=socks5://user:pass@127.0.0.1:1080

# HTTP proxy
QODER_PROXY=http://127.0.0.1:8080
```

**Env priority:** `QODER_PROXY` > `HTTPS_PROXY` > `ALL_PROXY`

---

## 🔒 API Key Authentication

When exposing the bridge publicly (e.g., via Caddy + domain), enable API key auth:

```env
QODER_API_KEY=sk-your-secret-key-here
```

**Behavior:**
- **No key set** → bridge is open, no auth needed
- **Key set** → clients MUST send `Authorization: Bearer sk-your-key`
- Without valid key → `401 Invalid API key`

Use in Hermes config:
```yaml
apiKey: "sk-your-secret-key-here"
```

---

## 🌐 Caddy Reverse Proxy

```caddyfile
qoder.yourdomain.com {
    reverse_proxy localhost:7100
}
```

Caddy auto-provisions HTTPS via Let's Encrypt.

> **Important:** When exposing via Caddy/domain, always set `QODER_API_KEY` in `.env`.

### Full example

```caddyfile
qoder.yourdomain.com {
    reverse_proxy localhost:7100 {
        header_up Host {upstream_host}
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-For {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

---

## 🐳 Docker

```bash
# Edit .env first, then:
docker compose up -d

# Or manually:
docker build -t qoder-bridge .
docker run -d --name qoder-bridge \
  -p 127.0.0.1:7100:7100 \
  --env-file .env \
  qoder-bridge
```

---

## 💰 Qoder Pricing & Usage

Qoder uses a **credit-based system** with model multipliers:

| Category | Models | Multiplier |
|----------|--------|:----------:|
| Tier | auto, ultimate, performance, efficient, lite | Varies per tier |
| Frontier | Each model has its own multiplier | Varies |

- **Pro Trial** accounts get credits that may show as `0` — this is normal (usage is unlimited/uncounted during trial).
- Credits **reset monthly**.
- Check your quota via API: `curl http://127.0.0.1:7100/v1/quota`
- Or via CLI: `./qoder-bridge quota`

---

## 🔧 Hermes Integration

Add to `~/.hermes/config.yaml`:

```yaml
providers:
  - name: Qoder
    type: openai
    baseUrl: http://127.0.0.1:7100/v1
    apiKey: "sk-your-key"    # or "not-needed" if no key set
    models:
      - qd/auto
      - qd/ultimate
      - qd/performance
      - qd/efficient
      - qd/lite
      - qd/qmodel_preview
      - qd/qmodel_latest
      - qd/qmodel
      - qd/kmodel_latest
      - qd/kmodel
      - qd/gm51model
      - qd/dmodel
      - qd/dfmodel
      - qd/mmodel
      - qd/combo-fast
      - qd/combo-smart
      - qd/combo-cheap
```

---

## 🔄 Update & Restart

### Update

```bash
./qoder-bridge update
```

This pulls from git, rebuilds, copies binary, and restarts.

### Restart after changing `.env`

`.env` is read only at startup — restart required:

```bash
./qoder-bridge stop
./qoder-bridge
```

### Check status

```bash
./qoder-bridge status
```

### View logs

```bash
tail -f ~/.qoder-bridge.log
```

---

## ⚠️ Port Caution

Default port is **7100**. If you change it:

1. Update `QODER_PORT` in `.env`
2. Update Caddy config (if using)
3. Update Hermes config
4. Update Docker port mapping

Check if port is in use: `ss -tlnp | grep 7100`

---

## 🏗️ How It Works

```
1. PAT Exchange     Your PAT (pt-...) → job token (jt-...)
2. User ID          Fetched from Qoder userinfo endpoint
3. Model Config     Live-fetched from Qoder model list
4. Request Build    Full Qoder format (session, business, chat_context, model_config)
5. WAF Encoding     Body encoded: base64 → rearrange → character substitution
6. COSY Signing     Encoded body signed with RSA-2048 + AES-128-CBC + MD5
7. SSE Unwrap       Qoder's {statusCodeValue, body} envelope → standard OpenAI SSE
```

---

## 📜 License

**MIT-0** (MIT No Attribution) — do whatever you want, no credit needed. See [LICENSE](LICENSE).
