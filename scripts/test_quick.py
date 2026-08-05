#!/usr/bin/env python3
"""Quick reliability test for qoder-bridge tool calling."""
import json, urllib.request

def test_model(model_name, num_requests=5):
    """Test model reliability."""
    tools = [{"type":"function","function":{"name":"terminal","description":"Execute shell command",
                "parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]
    URL = "http://127.0.0.1:7101/v1/chat/completions"
    
    success = 0
    for i in range(num_requests):
        try:
            req = urllib.request.Request(URL, data=json.dumps({
                "model": model_name, "max_tokens": 512,
                "messages": [{"role":"user","content":"Run: echo hello"}],
                "tools": tools
            }).encode(), headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=60) as resp:
                data = json.loads(resp.read())
            msg = data.get("choices")[0].get("message", {})
            tc = msg.get("tool_calls")
            if tc and len(tc) > 0:
                success += 1
        except Exception as e:
            pass
    
    return success / num_requests * 100 if num_requests > 0 else 0

print("
Qoder-Bridge Tool Calling Test")
print("="*75)

models = ["qd/auto", "Kimi-K3", "Qwen3.7-Plus", "DeepSeek-V4-Flash", "qd/performance", "qd/ultimate"]

for model in models:
    rate = test_model(model, 5)
    print(f"{model:20s}: {rate:.0f}% success rate (5 requests)")

print("="*75)
