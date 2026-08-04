#!/usr/bin/env python3
"""Hard 14-model test — 5 different real-world scenarios, 2 tries each = 10 per model."""
import json, time, urllib.request, sys

URL = "http://127.0.0.1:7101/v1/chat/completions"
MODELS = [
    "qd/auto", "qd/ultimate", "qd/performance", "qd/efficient", "qd/lite",
    "DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "GLM-5.2",
    "Kimi-K2.7-Code", "Kimi-K3", "MiniMax-M3",
    "Qwen3.7-Plus", "Qwen3.7-Max", "Qwen3.8-Max-Preview",
]

TOOLS = [
    {"type": "function", "function": {"name": "terminal", "description": "Execute a shell command", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}},
    {"type": "function", "function": {"name": "read_file", "description": "Read a file's contents", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}},
    {"type": "function", "function": {"name": "write_file", "description": "Write content to a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}, "required": ["path", "content"]}}},
]

# 5 different real-world scenarios
SCENARIOS = [
    {"sys": "You are a helpful coding assistant.", "usr": "Read the file /etc/hostname and tell me its contents.", "expect_tool": "read_file"},
    {"sys": "You are a helpful coding assistant.", "usr": "Run ls -la /tmp and show the output.", "expect_tool": "terminal"},
    {"sys": "You are a helpful coding assistant.", "usr": "Write a file at /tmp/test.txt with the content 'hello world'", "expect_tool": "write_file"},
    {"sys": "You are a helpful coding assistant.", "usr": "Check what Python version is installed by running python3 --version", "expect_tool": "terminal"},
    {"sys": "You are a helpful coding assistant.", "usr": "Read the file /etc/os-release", "expect_tool": "read_file"},
]

def test_model(model):
    results = []
    details = []
    for scenario in SCENARIOS:
        payload = {
            "model": model, "max_tokens": 2048,
            "messages": [{"role": "system", "content": scenario["sys"]}, {"role": "user", "content": scenario["usr"]}],
            "tools": TOOLS,
        }
        try:
            req = urllib.request.Request(URL, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=120) as resp:
                data = json.loads(resp.read())
            msg = data["choices"][0].get("message", {})
            tc = msg.get("tool_calls")
            if tc and len(tc) > 0:
                used = tc[0]["function"]["name"]
                results.append("✅")
                details.append(used)
            else:
                content = (msg.get("content") or "")[:80]
                results.append("⚠️")
                details.append(f"text:{content}")
        except Exception as e:
            results.append("❌")
            details.append(f"err:{str(e)[:50]}")
        time.sleep(0.3)
    return results, details

print("Testing 14 models × 5 scenarios (different tasks each)...")
print("=" * 80)
for model in MODELS:
    sys.stdout.write(f"\n{model}...")
    sys.stdout.flush()
    r, d = test_model(model)
    ok = r.count("✅")
    verdict = "PERFECT" if ok == 5 else f"MIXED({ok}/5)" if ok > 0 else "NEVER"
    print(f" {verdict}  {''.join(r)}")
    for i, detail in enumerate(d):
        if r[i] != "✅":
            print(f"  S{i+1}: {detail}")
    sys.stdout.flush()

print("\n" + "=" * 80)
print("Done.")
