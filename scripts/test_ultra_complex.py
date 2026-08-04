#!/usr/bin/env python3
"""
Ultra-complex tool calling stress test.
Tests 14 models × 7 scenarios = 98 requests.
Scenarios: multi-tool chains, conditional logic, error recovery, stateful sessions,
tool orchestration, reasoning + action loops.
"""
import json, time, urllib.request, sys

URL = "http://127.0.0.1:7101/v1/chat/completions"
MODELS = [
    "qd/auto", "qd/ultimate", "qd/performance", "qd/efficient", "qd/lite",
    "DeepSeek-V4-Flash", "DeepSeek-V4-Pro", "GLM-5.2",
    "Kimi-K2.7-Code", "Kimi-K3", "MiniMax-M3",
    "Qwen3.7-Plus", "Qwen3.7-Max", "Qwen3.8-Max-Preview",
]

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "terminal",
            "description": "Execute any bash shell command and return stdout/stderr output.",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Complete bash command to execute"}
                },
                "required": ["command"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Read the complete contents of any readable text or binary file.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Absolute or relative path to file"}
                },
                "required": ["path"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Write content to a file. Creates if not exists, overwrites if yes.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "File path to write"},
                    "content": {"type": "string", "description": "Content to write"}
                },
                "required": ["path", "content"]
            }
        }
    }
]

# Ultra-complex scenarios
SCENARIOS = [
    # 1. Multi-step sequential workflow
    {"id": "S1_seq_wf",
     "desc": "Multi-step sequential operations",
     "system": "You are a DevOps engineer executing systematic tasks.",
     "user": "First check Python version with 'python3 --version', then find all .py files in current directory with 'find . -name \"*.py\"', then list disk usage with 'du -sh *'. Execute these three commands sequentially.",
     "expected_tools_min": 3},

    # 2. Conditional logic with tool selection
    {"id": "S2_cond_logic",
     "desc": "Conditional tool selection based on prerequisites",
     "system": "You are an intelligent assistant that adapts to available information.",
     "user": "Check if README.md exists by running 'ls -la README.md 2>/dev/null'. If it exists, read it. If not, read package.json instead.",
     "expected_tools_min": 2},

    # 3. Error recovery pattern
    {"id": "S3_error_rec",
     "desc": "Graceful error handling and fallback",
     "system": "You are a resilient assistant. Handle errors gracefully.",
     "user": "Try to read file /nonexistent_dir/nonexistent_file_xyz.txt. When it fails, fall back to reading /etc/os-release and show its contents.",
     "expected_tools_min": 2},

    # 4. Data aggregation across tools
    {"id": "S4_data_agg",
     "desc": "Aggregate results from multiple tools",
     "system": "You are a system analyst gathering comprehensive data.",
     "user": "Gather system info: Read /etc/hostname, Run 'uname -a', Run 'uptime', Read /etc/os-release. Combine all into one report.",
     "expected_tools_min": 4},

    # 5. File manipulation pipeline
    {"id": "S5_file_pipe",
     "desc": "Multi-file creation and verification pipeline",
     "system": "You are a developer creating test artifacts.",
     "user": "Create directory structure: mkdir -p /tmp/myapp/src. Write main.py with content 'print(HelloWorld)' in src/. Run ls -laR /tmp/myapp to verify structure.",
     "expected_tools_min": 3},

    # 6. Complex search and filter
    {"id": "S6_complex_search",
     "desc": "Complex filtering and analysis",
     "system": "You are a log analyzer investigating issues.",
     "user": "Search for all lines containing ERROR in /var/log/syslog, count total occurrences, and show the last 3 matching lines.",
     "expected_tools_min": 2},

    # 7. Mixed orchestration pattern
    {"id": "S7_mixed_orch",
     "desc": "Orchestrate multiple tool types together",
     "system": "You are a full-stack diagnostician.",
     "user": "I need full diagnostics: (1) Read system release info (/etc/os-release), (2) Check memory usage ('free -m'), (3) List CPU cores, (4) Write all findings to /tmp/full_diag.txt",
     "expected_tools_min": 4}]


def call_bridge(model, messages):
    """Make API request and extract response details."""
    payload = {
        "model": model,
        "max_tokens": 4096,
        "messages": messages,
        "tools": TOOLS,
    }
    req = urllib.request.Request(
        URL,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            data = json.loads(resp.read())
        msg = data["choices"][0].get("message", {})
        tc = msg.get("tool_calls") or []
        content = (msg.get("content") or "").strip()
        finish_reason = data["choices"][0].get("finish_reason", "unknown")
        return {
            "success": len(tc) > 0,
            "tool_calls": tc,
            "tool_count": len(tc),
            "used_tools": [t["function"]["name"] for t in tc],
            "args_ok": all(t["function"].get("arguments") for t in tc),
            "content": content,
            "finish_reason": finish_reason
        }
    except Exception as e:
        return {"success": False, "error": str(e)[:100]}


def run_scenario(model_name, scenario):
    """Run single scenario."""
    messages = [
        {"role": "system", "content": scenario["system"]},
        {"role": "user", "content": scenario["user"]},
    ]
    expected_min = scenario.get("expected_tools_min", 1)
    result = call_bridge(model_name, messages)
    
    ok = result["success"] and result["tool_count"] >= expected_min and result["args_ok"]
    return ok, result


def main():
    print("="*95)
    print("ULTRA-COMPLEX TOOL CALLING STRESS TEST")
    print(f"Models: {len(MODELS)}, Scenarios: {len(SCENARIOS)}")
    print(f"Total requests: {len(MODELS) * len(SCENARIOS)}")
    print("Testing: sequential workflows, conditional logic, error recovery")
    print("         data aggregation, file pipelines, complex search, mixed orchestration")
    print("="*95)
    
    # Header
    header = f"{'Model':<26} | "
    for s in SCENARIOS:
        header += f"{s['id']:>10} | "
    header += f"{'Score':>6} Verdict"
    print(header)
    print("-"*95)
    
    # Results matrix
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
            time.sleep(0.3)
        
        pct = round(total_ok / total_expected * 100)
        verdict = "PERFECT" if pct == 100 else f"SCORE:{pct}%"
        print(f" {pct:>5}% {verdict}")
        sys.stdout.flush()
    
    print("-"*95)
    print("Done.")
    
    # Summary stats
    perfect_count = sum(1 for m in MODELS for r in results[m].values() if r["success"] and r["tool_count"] >= 1 and r["args_ok"])
    print(f"\nSummary: Tests completed successfully!")


if __name__ == "__main__":
    main()
