#!/usr/bin/env python3
"""14-model × 10-try consistency test with PAT rotation comparison."""
import json, time, urllib.request, sys

URL = "http://127.0.0.1:7101/v1/chat/completions"
MODELS = [
    "qd/auto", "qd/ultimate", "qd/performance", "qd/efficient", "qd/lite",
    "DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "GLM-5.2",
    "Kimi-K2.7-Code", "Kimi-K3", "MiniMax-M3",
    "Qwen3.7-Plus", "Qwen3.7-Max", "Qwen3.8-Max-Preview",
]
TRIES = 10

TOOLS = [{
    "type": "function",
    "function": {
        "name": "terminal",
        "description": "Run a terminal command",
        "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}
    }
}]

SYS = "You are a helpful assistant. You MUST use the terminal tool to run commands."
USR = "Run: echo hello"

def test_model(model):
    results = []
    for i in range(TRIES):
        payload = {
            "model": model, "max_tokens": 2048,
            "messages": [{"role": "system", "content": SYS}, {"role": "user", "content": USR}],
            "tools": TOOLS,
        }
        try:
            req = urllib.request.Request(URL, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=120) as resp:
                data = json.loads(resp.read())
            msg = data["choices"][0].get("message", {})
            tc = msg.get("tool_calls")
            results.append("✅" if tc and len(tc) > 0 else "⚠️")
        except Exception as e:
            results.append("❌")
        time.sleep(0.5)
    return results

for model in MODELS:
    sys.stdout.write(f"\n{model}...")
    sys.stdout.flush()
    r = test_model(model)
    ok = r.count("✅")
    verdict = "PERFECT" if ok == TRIES else f"MIXED({ok}/{TRIES})" if ok > 0 else "NEVER"
    print(f" {verdict}  {''.join(r)}")
    sys.stdout.flush()

print("\nDone.")
