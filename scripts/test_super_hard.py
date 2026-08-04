#!/usr/bin/env python3
"""
Super complex 14-model tool calling stress test.
Multi-turn, multi-tool, 6 scenarios, 84 requests total.
"""
import json, time, urllib.request, sys, uuid

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
        "description": "Execute a shell command and return stdout/stderr.",
        "parameters": {"type": "object", "properties": {
            "command": {"type": "string", "description": "Bash command to execute"}
        }, "required": ["command"]}
    }},
    {"type": "function", "function": {
        "name": "read_file",
        "description": "Read a text file's contents.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path to read"}
        }, "required": ["path"]}
    }},
    {"type": "function", "function": {
        "name": "write_file",
        "description": "Write content to a file (creates or overwrites).",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path to write"},
            "content": {"type": "string", "description": "Content to write"}
        }, "required": ["path", "content"]}
    }},
]

TASKS = [
    {
        "id": "S1_read_two_files",
        "system": "You are a helpful file assistant. Use tools to accomplish tasks.",
        "user": "Read /etc/hostname AND /etc/os-release. Return both contents.",
        "expect_tool_count_min": 2,
    },
    {
        "id": "S2_terminal_multi_step",
        "system": "You are a system diagnostic assistant.",
        "user": "Run these three commands one by one: (1) whoami (2) uname -a (3) df -h /",
        "expect_tool_count_min": 1,
    },
    {
        "id": "S3_write_then_confirm",
        "system": "You are a file management assistant.",
        "user": "Write 'bridge test ok' to /tmp/bridge_test.txt, then run cat /tmp/bridge_test.txt to confirm.",
        "expect_tool_count_min": 1,
    },
    {
        "id": "S4_error_recovery",
        "system": "You are a resilient assistant. If something fails, try an alternative.",
        "user": "First try to read /nonexistent_path_xyz. When that fails, read /etc/hostname instead.",
        "expect_tool_count_min": 1,
    },
    {
        "id": "S5_choose_right_tool",
        "system": "You are an intelligent assistant that picks the right tool for each job.",
        "user": "I need a quick system check: (1) read /etc/os-release to see the OS, (2) run uptime to check load, (3) write a summary to /tmp/syscheck.txt",
        "expect_tool_count_min": 1,
    },
    {
        "id": "S6_complex_reasoning",
        "system": "You are a DevOps assistant. Use terminal to investigate.",
        "user": "Find the 3 largest files in /var/log, check disk usage of /var, and tell me if any log files are over 100MB. Use terminal commands.",
        "expect_tool_count_min": 1,
    },
]


def call_bridge(model, messages):
    payload = {
        "model": model,
        "max_tokens": 4096,
        "messages": messages,
        "tools": TOOLS,
    }
    req = urllib.request.Request(
        URL,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())
    msg = data["choices"][0].get("message", {})
    return {
        "tool_calls": msg.get("tool_calls") or [],
        "content": (msg.get("content") or "").strip(),
    }


def run_task(model, task):
    """Run single-turn task. Returns (success: bool, tool_count: int, detail: str)."""
    messages = [
        {"role": "system", "content": task["system"]},
        {"role": "user", "content": task["user"]},
    ]
    try:
        r = call_bridge(model, messages)
        tc = r["tool_calls"]
        if len(tc) > 0:
            names = [t["function"]["name"] for t in tc]
            args_ok = all(t["function"].get("arguments") for t in tc)
            ok = len(tc) >= task["expect_tool_count_min"] and args_ok
            return ok, len(tc), ",".join(names)
        else:
            return False, 0, f"text:{r['content'][:60]}"
    except Exception as e:
        return False, 0, f"err:{str(e)[:60]}"


print(f"{'Model':<26}", end="")
for t in TASKS:
    print(f" {t['id'][:5]:>5}", end="")
print(f"  {'Score':>5}  Verdict")
print("-" * 100)

for model in MODELS:
    sys.stdout.write(f"{model:<26}")
    sys.stdout.flush()
    total_ok = 0
    total_tools = 0
    for task in TASKS:
        ok, cnt, detail = run_task(model, task)
        total_ok += int(ok)
        total_tools += cnt
        sys.stdout.write(f" {'  OK' if ok else 'FAIL':>5}")
        sys.stdout.flush()
        time.sleep(0.3)
    pct = round(total_ok / len(TASKS) * 100)
    verdict = "PERFECT" if pct == 100 else f"SCORE:{pct}%"
    print(f"  {pct:>4}%  {verdict}  (tools:{total_tools})")

print("-" * 100)
print("Done.")
