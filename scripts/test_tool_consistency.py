#!/usr/bin/env python3
"""Test tool calling consistency across all Qoder models."""
import json
import time
import urllib.request

URL = "http://127.0.0.1:7101/v1/chat/completions"

TOOL_PAYLOAD = {
    "stream": False,
    "max_tokens": 256,
    "messages": [
        {"role": "system", "content": "You are a coding assistant. You MUST use the provided tools to answer. NEVER describe what you would do — actually call the tools."},
        {"role": "user", "content": "Use the terminal tool to run: echo hello"}
    ],
    "tools": [{
        "type": "function",
        "function": {
            "name": "terminal",
            "description": "Execute a shell command and return its output",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "The shell command to run"}
                },
                "required": ["command"]
            }
        }
    }]
}

MODELS = [
    "qd/auto", "qd/ultimate", "qd/performance", "qd/efficient", "qd/lite",
    "DeepSeek-V4-Flash", "DeepSeek-V4-Pro",
    "GLM-5.2",
    "Kimi-K2.7-Code", "Kimi-K3",
    "MiniMax-M3",
    "Qwen3.7-Plus", "Qwen3.7-Max", "Qwen3.8-Max-Preview",
]

TRIES = 3

def test_model(model, attempt):
    payload = dict(TOOL_PAYLOAD)
    payload["model"] = model
    data = json.dumps(payload).encode()
    req = urllib.request.Request(URL, data=data, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:
            body = json.loads(resp.read())
        choice = body.get("choices", [{}])[0]
        msg = choice.get("message", {})
        tc = msg.get("tool_calls")
        content = msg.get("content", "")[:120]
        fr = choice.get("finish_reason", "")
        if tc:
            name = tc[0].get("function", {}).get("name", "?")
            args = tc[0].get("function", {}).get("arguments", "")[:80]
            return "TOOL_CALL", f"name={name} args={args} fr={fr}"
        elif "<tool_call>" in (content or ""):
            return "RAW_XML", f"raw <tool_call> in content"
        elif "<function_calls" in (content or ""):
            return "RAW_ANTHROPIC", f"raw <function_calls in content"
        else:
            return "TEXT_ONLY", f"content={repr(content[:80])} fr={fr}"
    except Exception as e:
        return "ERROR", str(e)[:100]

print(f"{'Model':<25} {'T1':<12} {'T2':<12} {'T3':<12} {'Verdict':<10}")
print("-" * 80)

for model in MODELS:
    results = []
    details = []
    for i in range(TRIES):
        status, detail = test_model(model, i)
        results.append(status)
        details.append(detail)
        if i < TRIES - 1:
            time.sleep(2)

    tool_count = sum(1 for r in results if r == "TOOL_CALL")
    raw_count = sum(1 for r in results if r.startswith("RAW"))
    err_count = sum(1 for r in results if r == "ERROR")

    if tool_count == TRIES:
        verdict = "PERFECT"
    elif tool_count > 0:
        verdict = f"MIXED({tool_count}/{TRIES})"
    elif raw_count > 0:
        verdict = f"RAW({raw_count}/{TRIES})"
    elif err_count == TRIES:
        verdict = "ERROR"
    else:
        verdict = "NEVER"

    cols = []
    for r, d in zip(results, details):
        if r == "TOOL_CALL":
            cols.append("TOOL")
        elif r.startswith("RAW"):
            cols.append(r)
        elif r == "ERROR":
            cols.append("ERR")
        else:
            cols.append("TEXT")

    print(f"{model:<25} {cols[0]:<12} {cols[1]:<12} {cols[2]:<12} {verdict:<10}")

    # Print details for non-perfect models
    if verdict not in ("PERFECT",):
        for i, d in enumerate(details):
            if results[i] != "TOOL_CALL":
                print(f"  attempt {i+1}: {d}")

print()
print("=== SUMMARY ===")
