#!/usr/bin/env python3
"""Ultra-complex 7-scenario tool calling test."""
import json, time, urllib.request, sys

URL = "http://127.0.0.1:7101/v1/chat/completions"
MODELS = [
    "qd/auto", "qd/ultimate", "qd/performance", "qd/efficient", "qd/lite",
    "DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "GLM-5.2",
    "Kimi-K2.7-Code", "Kimi-K3", "MiniMax-M3",
    "Qwen3.7-Plus", "Qwen3.7-Max", "Qwen3.8-Max-Preview",
]

TOOLS = [
    {"type": "function", "function": {
        "name": "terminal",
        "description": "Execute any bash shell command.",
        "parameters": {"type": "object", "properties": {
            "command": {"type": "string", "description": "Bash command to execute"}
        }, "required": ["command"]}}},
    {"type": "function", "function": {
        "name": "read_file",
        "description": "Read file contents.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path"}
        }, "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "write_file",
        "description": "Write content to file.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path"},
            "content": {"type": "string", "description": "Content to write"}
        }, "required": ["path", "content"]}}}
]

SCENARIOS = [
    {"id": "S1_seq_wf", "system": "You are a DevOps engineer.",
     "user": "Run: python3 --version; find . -name '*.py'; du -sh *", "min": 1},
    {"id": "S2_cond_log", "system": "You are an intelligent assistant.",
     "user": "Check if README.md exists. If yes read it, else read package.json.", "min": 2},
    {"id": "S3_err_rec", "system": "You handle errors gracefully.",
     "user": "Try reading /nonexistent_xyz.txt, fallback to /etc/os-release.", "min": 2},
    {"id": "S4_data_agg", "system": "You gather comprehensive data.",
     "user": "Read /etc/hostname, run uname -a, run uptime, read /etc/os-release.", "min": 4},
    {"id": "S5_file_pipe", "system": "You create test artifacts.",
     "user": "Create /tmp/myapp/src/, write main.py with print(HelloWorld), verify with ls -laR.", "min": 3},
    {"id": "S6_search", "system": "You analyze logs.",
     "user": "Search /var/log/syslog for ERROR, count and show last 3 matches.", "min": 2},
    {"id": "S7_mixed", "system": "You diagnose systems.",
     "user": "Read /etc/os-release, run free -m, list CPU cores, write findings to /tmp/diag.txt.", "min": 4},
]

def call_bridge(model, messages):
    payload = {"model": model, "max_tokens": 4096, "messages": messages, "tools": TOOLS}
    req = urllib.request.Request(URL, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            data = json.loads(resp.read())
        msg = data["choices"][0].get("message", {})
        tc = msg.get("tool_calls") or []
        return {"success": len(tc) > 0, "tool_count": len(tc),
                "used_tools": [t["function"]["name"] for t in tc],
                "args_ok": all(t["function"].get("arguments") for t in tc)}
    except Exception as e:
        return {"success": False, "error": str(e)[:100]}

def run(model, scenario):
    msgs = [{"role": "system", "content": scenario["system"]}, {"role": "user", "content": scenario["user"]}]
    r = call_bridge(model, msgs)
    ok = r["success"] and r["tool_count"] >= scenario["min"] and r["args_ok"]
    return ok, r

print("="*95)
print("ULTRA-COMPLEX TEST (v2 — api2 routing)")
print(f"Models: {len(MODELS)}, Scenarios: {len(SCENARIOS)}")
print("="*95)

header = f"{'Model':<26} |"
for s in SCENARIOS:
    header += f" {s['id']:>9} |"
header += f" {'Score':>5} Verdict"
print(header)
print("-"*95)

for model in MODELS:
    sys.stdout.write(f"{model:<26} |")
    sys.stdout.flush()
    total_ok = 0
    for s in SCENARIOS:
        ok, r = run(model, s)
        total_ok += int(ok)
        sys.stdout.write(f" {'OK' if ok else 'FAIL':>7} |")
        sys.stdout.flush()
        time.sleep(0.3)
    pct = round(total_ok / len(SCENARIOS) * 100)
    verdict = "PERFECT" if pct == 100 else f"SCORE:{pct}%"
    print(f" {pct:>4}% {verdict}")

print("-"*95)
print("Done.")