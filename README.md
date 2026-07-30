# 🌉 qoder-bridge

> **Pure-Go OpenAI-compatible proxy for [Qoder](https://qoder.com) API.**
> Zero dependencies. Zero cold start. Just works.

Uses COSY signing (RSA-2048 + AES-128-CBC + MD5) directly — **no qodercli, no Node.js, no npm, no WASM.**

---

## ⚡ Performance

| Metric | qodercli (Node.js) | **qoder-bridge (Go)** |
|--------|:-------------------:|:---------------------:|
| 🕐 Cold start | ~9–14s | **~50ms** |
| 💾 RAM usage | ~300 MB spike | **~8 MB constant** |
| 📦 Binary size | 200 MB+ (npm) | **~10 MB** |
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

Your `.env` — one PAT per line:

```env
pt-your-first-pat-here
pt-your-second-pat-here
```

### 3️⃣ Build & run

```bash
go build -o qoder-bridge .
./qoder-bridge
```

That's it. Bridge is live on `http://127.0.0.1:7100`.

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

## ⚙️ Configuration

All config is via `.env` file. No flags required.

### 📄 `.env` reference

```env
# ── Qoder PATs (required) ─────────────────────────────
# One PAT per line. Get yours from https://qoder.com/account/integrations
# Format: pt-xxxxxxxxxxxx
pt-your-first-pat-here
pt-your-second-pat-here

# ── Server ────────────────────────────────────────────
QODER_PORT=7100

# ── PAT rotation strategy ─────────────────────────────
# round-robin (default): cycles through PATs in order
# random: picks a random PAT each request
PAT_STRATEGY=round-robin

# ── API Key (optional) ───────────────────────────────
# Protect the bridge with a Bearer token.
# Use any sk-* key (recommended: 40+ chars).
# Clients must send: Authorization: Bearer sk-xxx
#QODER_API_KEY=sk-your-secret-key-here

# ── Combos (optional) ─────────────────────────────────
# Format: COMBO_<NAME>=model1,model2,model3
# First model is primary. On error, tries the next.
# Models are auto-prefixed with qd/ (case-insensitive).
COMBO_FAST=efficient,lite
COMBO_SMART=ultimate,qmodel_preview,dmodel
COMBO_CHEAP=lite,efficient,dfmodel
COMBO_DEFAULT=auto,ultimate,performance

# ── Proxy (optional) ─────────────────────────────────
# Route all Qoder API traffic through a proxy.
# Supports: socks5://, socks5h://, http://, https://
# Priority: QODER_PROXY > HTTPS_PROXY > ALL_PROXY
#QODER_PROXY=socks5://admin:pass@127.0.0.1:1080
```

### 🏁 CLI flags

| Flag | Default | Description |
|------|:-------:|-------------|
| `-port` | `7100` | Listen port (overrides `QODER_PORT`) |
| `-pats` | | Comma-separated PAT list (overrides `.env`) |
| `-env` | `.env` | Path to `.env` file |

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

### 📋 Models

```bash
curl http://127.0.0.1:7100/v1/models
```

### 📊 Quota

```bash
curl http://127.0.0.1:7100/v1/quota
```

### 🎯 Combos

```bash
curl http://127.0.0.1:7100/v1/combos
```

### ❤️ Health

```bash
curl http://127.0.0.1:7100/health
```

---

## 🧠 Available Models

All models use the `qd/` prefix. Prefix is **case-insensitive** — `QD/Auto`, `qd/AUTO`, `Qd/auto` all work.

### 🏷️ Tier Models (auto-routed by Qoder)

| Model ID | Display Name | Description | Credit Multiplier |
|----------|:------------:|-------------|:-----------------:|
| `qd/auto` | Auto | Automatic model selection | 1x (base) |
| `qd/ultimate` | Ultimate | Best quality, most expensive | Highest |
| `qd/performance` | Performance | High performance | High |
| `qd/efficient` | Efficient | Balanced cost/quality | Medium |
| `qd/lite` | Lite | Fastest, cheapest | Lowest |

### 🌐 Frontier Models (specific LLMs)

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

> 💡 **Model aliases:** Any prefix works. `qd/auto`, `qoder/auto`, `QD/Auto`, even `apore/auto` — all resolve to `auto`. If prefix isn't `qd` or `qoder`, a warning is logged but the model still works.

---

## 🎯 Combos

Combos are **fallback chains** — try the first model, and if it fails, automatically try the next one.

### 📝 Define combos in `.env`

```env
COMBO_FAST=efficient,lite
COMBO_SMART=ultimate,qmodel_preview,dmodel
COMBO_CHEAP=lite,efficient,dfmodel
COMBO_DEFAULT=auto,ultimate,performance
```

### 🔗 Use combos

```bash
# Auto-prefixed with qd/
curl http://127.0.0.1:7100/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "qd/combo-fast", "messages": [{"role": "user", "content": "Hi"}]}'

# Also works without prefix
curl ... -d '{"model": "combo-fast", ...}'

# Case-insensitive
curl ... -d '{"model": "COMBO_FAST", ...}'
```

### 🔄 How combo fallback works

```
Request: qd/combo-smart
  → try qd/ultimate ... ❌ error
  → try qd/qmodel_preview ... ✅ success → return response
```

If **all models fail**, returns the last error.

Unknown model names in combos are **tried anyway** (Qoder may add new models) — a warning is logged but execution continues.

---

## 💰 Qoder Pricing & Usage

Qoder uses a **credit-based system** with model multipliers:

| Category | Models | Multiplier |
|----------|--------|:----------:|
| 🏷️ Tier | auto, ultimate, performance, efficient, lite | Varies per tier |
| 🌐 Frontier | Each model has its own multiplier | Varies |

- **Pro Trial** accounts get credits that may show as `0` — this is normal (usage is unlimited/uncounted during trial).
- Credits **reset monthly**.
- Check your quota via API or CLI (see below).

### 📊 Check quota via API

```bash
curl http://127.0.0.1:7100/v1/quota
```

### 📊 Check quota via CLI

```bash
./qoder-bridge quota

# With custom .env:
./qoder-bridge quota -env /path/to/.env
```

Output example:

```
Fetching quota for 2 PAT(s)...

PAT             USED   REMAINING   LIMIT    RESET                STATUS
pt-7J45...8ab2  0      0           0        2026-08-10           ok
pt-QaO1...0a52  0      0           0        2026-08-10           ok
```

---

## 🌐 Caddy Reverse Proxy

### Basic setup

```caddyfile
qoder.yourdomain.com {
    reverse_proxy localhost:7100
}
```

Caddy auto-provisions HTTPS via Let's Encrypt. Done.

### 📋 Full example with headers

```caddyfile
qoder.yourdomain.com {
    reverse_proxy localhost:7100 {
        header_up Host {upstream_host}
        header_up X-Real-IP {remote_host}
        header_up X-Forwarded-For {remote_host}
        header_up X-Forwarded-Proto {scheme}
    }

    # Rate limiting (optional)
    rate_limit {
        zone qoder {
            key {remote_host}
            events 100
            window 1m
        }
    }
}
```

### ⚠️ Port reminder

> Default port is **7100**. If you change it in `.env`, update your Caddy config, docker-compose, and Hermes config too.
>
> Check if port is in use: `ss -tlnp | grep 7100`

---

## 🔧 Hermes Integration

Add to `~/.hermes/config.yaml`:

```yaml
providers:
  - name: Qoder
    type: openai
    baseUrl: http://127.0.0.1:7100/v1
    apiKey: "not-needed"
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

## 🌐 Proxy Support

Route all Qoder API traffic through a SOCKS5 or HTTP proxy. Useful for:

- **WARP proxy** — route through Cloudflare WARP for clean egress IPs
- **Privacy** — hide your VPS real IP from Qoder
- **Bypass restrictions** — if your VPS IP is blocked

### 🔧 Configure

Add to `.env`:

```env
# SOCKS5 proxy (e.g., MicroWARP)
QODER_PROXY=socks5://admin:pass@127.0.0.1:1080

# HTTP proxy
QODER_PROXY=http://127.0.0.1:8080

# SOCKS5 without auth
QODER_PROXY=socks5://127.0.0.1:1080

# SOCKS5h (DNS resolved by proxy)
QODER_PROXY=socks5h://admin:pass@127.0.0.1:1080
```

### 📋 Supported formats

| Format | Example | Description |
|--------|---------|-------------|
| `socks5://` | `socks5://user:pass@host:1080` | SOCKS5 with auth |
| `socks5h://` | `socks5h://host:1080` | SOCKS5, DNS resolved by proxy |
| `http://` | `http://host:8080` | HTTP CONNECT proxy |
| `https://` | `https://host:8080` | HTTPS CONNECT proxy |

### 🔍 Env priority

1. `QODER_PROXY` (highest — dedicated to qoder-bridge)
2. `HTTPS_PROXY` / `https_proxy`
3. `ALL_PROXY` / `all_proxy`

### 🚀 Quick: MicroWARP + qoder-bridge

```bash
# 1. Start MicroWARP (SOCKS5 proxy via Cloudflare WARP)
cd ~/microwarp && docker compose up -d

# 2. Add to qoder-bridge .env
echo 'QODER_PROXY=socks5://admin:pass@127.0.0.1:1080' >> ~/projects/qoder-bridge/.env

# 3. Restart qoder-bridge
systemctl --user restart qoder-bridge

# 4. Verify — startup log shows proxy info
journalctl --user -u qoder-bridge --since "5 sec ago" | grep proxy
```

---

## 🏗️ How It Works

```
1. 🔄 PAT Exchange     Your PAT (pt-...) → job token (jt-...) via openapi.qoder.sh
2. 👤 User ID          Fetched from Qoder userinfo endpoint
3. 📋 Model Config     Live-fetched from api3.qoder.sh/algo/api/v2/model/list
4. 📦 Request Build    Translated to Qoder internal format (session, business, chat_context, model_config)
5. 🔐 WAF Encoding     Body encoded: base64 → rearrange → character substitution
6. ✍️  COSY Signing    Encoded body signed with RSA-2048 + AES-128-CBC + MD5
7. 📡 SSE Unwrap       Qoder's {statusCodeValue, body} envelope → standard OpenAI SSE
```

---

## 🛠️ Build from Source

```bash
go build -o qoder-bridge .
```

Requires **Go 1.21+**. Zero external dependencies — pure Go stdlib.

---

## 🔄 Update & Restart

### Update (native)

```bash
cd ~/projects/qoder-bridge   # wherever you cloned it
git pull
go build -o qoder-bridge .
cp qoder-bridge ~/.local/bin/qoder-bridge
chmod +x ~/.local/bin/qoder-bridge
systemctl --user restart qoder-bridge
```

### Update (Docker)

```bash
cd ~/projects/qoder-bridge
git pull
docker compose build --no-cache
docker compose up -d
```

### After changing `.env`

`.env` changes require a **restart** — the bridge reads `.env` only at startup:

```bash
# Native
systemctl --user restart qoder-bridge

# Docker
docker compose restart
```

### Check status

```bash
# Native — check logs
journalctl --user -u qoder-bridge --since "1 min ago" --no-pager

# Docker
docker compose logs --tail 20
```

### How to run in foreground (debug)

```bash
./qoder-bridge                    # blocks terminal — use Ctrl+C to stop
./qoder-bridge -port 7102         # custom port
./qoder-bridge -env /path/.env    # custom .env path
```

> ⚠️ **Do NOT run `./qoder-bridge` directly in production** — use systemd (native) or Docker. Running in foreground blocks your terminal and the process dies when you close it.

---

## 📜 License

**MIT-0** (MIT No Attribution) — do whatever you want, no credit needed. See [LICENSE](LICENSE).

---

## 🙏 Credits

COSY signing algorithm reverse-engineered from the Qoder ecosystem. Built for the community.
