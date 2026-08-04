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
        }, "required": ["command"]}},
    {"type": "function", "function": {
        "name": "read_file",
        "description": "Read file contents.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path"}
        }, "required": ["path"]}},
    {"type": "function", "function": {
        "name": "write_file",
        "description": "Write content to file.",
        "parameters": {"type": "object", "properties": {
            "path": {"type": "string", "description": "File path"},
            "content": {"type": "string", "description": "Content to write"}
        }, "required": ["path", "content"]}}]

SCENARIOS = [
    {"id": "S1_seq_wf", "desc": "Sequential workflow (3 commands)", "system": "You are a DevOps engineer.", "user": "Run: python3 --version; find . -name '*.py'; du -sh *", "expected_tools_min": 3},
    {"id": "S2_cond_logic", "desc": "Conditional logic (if-else)", "system": "You are an intelligent assistant.", "user": "Check if README.md exists. If yes read it, else read package.json.", "expected_tools_min": 2},
    {"id": "S3_error_rec", "desc": "Error recovery (fallback)", "system": "You handle errors gracefully.", "user": "Try reading /nonexistent_xyz.txt, fallback to /etc/os-release.", "expected_tools_min": 2},
    {"id": "S4_data_agg", "desc": "Data aggregation (4 tools)", "system": "You gather comprehensive data.", "user": "Read /etc/hostname, run uname -a, run uptime, read /etc/os-release.", "expected_tools_min": 4},
    {"id": "S5_file_pipe", "desc": "File pipeline (mkdir+write+verify)", "system": "You create test artifacts.", "user": "Create /tmp/myapp/src/, write main.py with print(HelloWorld), verify with ls -laR.", "expected_tools_min": 3},
    {"id": "S6_search", "desc": "Complex search", "system": "You analyze logs.", "user": "Search /var/log/syslog for ERROR, count and show last 3 matches.", "expected_tools_min": 2},
    {"id": "S7_mixed_orch", "desc": "Mixed orchestration", "system": "You diagnose systems.", "user": "Read /etc/os-release, run free -m, list CPU cores, write findings to /tmp/full_diag.txt.", "expected_tools_min": 4}]

def call_bridge(model, messages):
    payload = {"model": model, "max_tokens": 4096, "messages": messages, "tools": TOOLS}
    req = urllib.request.Request(URL, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            data = json.loads(resp.read())
        msg = data["choices"][0].get("message", {})
        tc = msg.get("tool_calls") or []
        return {
            "success": len(tc) > 0, "tool_count": len(tc),
            "used_tools": [t["function"]["name"] for t in tc],
            "args_ok": all(t["function"].get("arguments") for t in tc)
        }
    except Exception as e:
        return {"success": False, "error": str(e)[:100]}

def run_scenario(model_name, scenario):
    messages = [{"role": "system", "content": scenario["system"]}, {"role": "user", "content": scenario["user"]}]
    expected_min = scenario.get("expected_tools_min", 1)
    result = call_bridge(model_name, messages)
    ok = result["success"] and result["tool_count"] >= expected_min and result["args_ok"]
    return ok, result

def main():
    print("="*95)
    print("ULTRA-COMPLEX 7-SCENARIO TOOL CALLING TEST")
    print(f"Models: {len(MODELS)}, Scenarios: {len(SCENARIOS)}")
    print(f"Total requests: {len(MODELS) * len(SCENARIOS)}")
    print("Testing: sequential workflows, conditional logic, error recovery")
    print("         data aggregation, file pipelines, complex search, mixed orchestration")
    print("="*95)
    
    header = f"{'Model':<26} | "
    for s in SCENARIOS:
        header += f"{s['id']:>10} | "
    header += f"{'Score':>6} Verdict"
    print(header)
    print("-"*95)
    
    results = {}
    for model in MODELS:
        results[model] = {}
        sys.stdout.write(f"{model:<26} | ")
        sys.stdout.flush()
        total_ok = 0
        total_expected = 0
        for scenario in SCENARIOS:
            sid = scenario["id"]
            expect_min = scenario.get("expected_tools_min", 1)
            ok, r = run_scenario(model, scenario)
            results[model][sid] = r
            total_ok += int(ok)
            total_expected += 1
            marker = "OK" if ok else "FAIL"
            sys.stdout.write(f" {marker:>8} | ")
            sys.stdout.flush()
            time.sleep(0.2)
        pct = round(total_ok / total_expected * 100)
        verdict = "PERFECT" if pct == 100 else f"SCORE:{pct}%"
        print(f" {pct:>5}% {verdict}")
        sys.stdout.flush()
    
    print("-"*95)
    print("Done.")

if __name__ == "__main__":
    main()
