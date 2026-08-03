package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── PAT Pool tests ───────────────────────────────────────────────────────

func TestPATPoolRoundRobin(t *testing.T) {
	p := NewPATPool([]string{"pt-a", "pt-b", "pt-c"}, "round-robin")
	got := []string{p.Next(), p.Next(), p.Next(), p.Next()}
	want := []string{"pt-a", "pt-b", "pt-c", "pt-a"}
	for i, g := range got {
		if g != want[i] {
			t.Errorf("round %d: got %q, want %q", i, g, want[i])
		}
	}
}

func TestPATPoolRandom(t *testing.T) {
	p := NewPATPool([]string{"pt-a", "pt-b"}, "random")
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		seen[p.Next()] = true
	}
	if !seen["pt-a"] || !seen["pt-b"] {
		t.Error("random: expected both PATs to be selected over 50 calls")
	}
}

func TestPATPoolEmpty(t *testing.T) {
	p := NewPATPool(nil, "round-robin")
	if p.Next() != "" {
		t.Error("empty pool should return empty string")
	}
}

func TestPATPoolNextAvoidNoRepeat(t *testing.T) {
	p := NewPATPool([]string{"pt-a", "pt-b", "pt-c"}, "round-robin")
	used := map[string]bool{}
	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		pat := p.NextAvoid(used)
		if pat == "" {
			break
		}
		if got[pat] {
			t.Fatalf("NextAvoid returned duplicate: %s", pat)
		}
		got[pat] = true
		used[pat] = true
	}
	if len(got) != 3 {
		t.Errorf("expected 3 distinct PATs, got %d: %v", len(got), got)
	}
}

func TestPATPoolNextAvoidEmptyWhenExhausted(t *testing.T) {
	p := NewPATPool([]string{"pt-a"}, "round-robin")
	used := map[string]bool{"pt-a": true}
	if pat := p.NextAvoid(used); pat != "" {
		t.Errorf("expected empty when all used, got %q", pat)
	}
}

func TestPATPoolCooldown(t *testing.T) {
	p := NewPATPool([]string{"pt-a", "pt-b"}, "round-robin")
	p.Cooldown("pt-a", 1*time.Hour)
	used := map[string]bool{}
	got := p.NextAvoid(used)
	if got != "pt-b" {
		t.Errorf("expected pt-b (pt-a in cooldown), got %q", got)
	}
}

func TestPATPoolCooldownExpiry(t *testing.T) {
	p := NewPATPool([]string{"pt-a"}, "round-robin")
	p.Cooldown("pt-a", 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	used := map[string]bool{}
	got := p.NextAvoid(used)
	if got != "pt-a" {
		t.Errorf("cooldown expired, should return pt-a, got %q", got)
	}
}

// ── Error classification tests ───────────────────────────────────────────

func TestIsPricingError(t *testing.T) {
	err := &UpstreamError{StatusCode: 403, Body: `{"code":"112","message":"quota exceeded"}`}
	if !isPricingError(err) {
		t.Error("expected isPricingError=true for 403+code-112")
	}
	err2 := &UpstreamError{StatusCode: 403, Body: `{"message":"forbidden"}`}
	if isPricingError(err2) {
		t.Error("expected isPricingError=false for plain 403")
	}
}

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  *UpstreamError
		want bool
	}{
		{"401", &UpstreamError{StatusCode: 401}, true},
		{"403 pricing", &UpstreamError{StatusCode: 403, Body: `{"code":"112"}`}, false},
		{"403 plain", &UpstreamError{StatusCode: 403, Body: `{"message":"forbidden"}`}, true},
		{"500", &UpstreamError{StatusCode: 500}, false},
	}
	for _, tt := range tests {
		got := isAuthError(tt.err)
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"pricing 112", &UpstreamError{StatusCode: 403, Body: `{"code":"112"}`}, true},
		{"auth 401", &UpstreamError{StatusCode: 401}, false}, // bad token — rotating won't help
		{"auth 403 non-pricing", &UpstreamError{StatusCode: 403, Body: `{"error":"forbidden"}`}, false},
		{"queue", &UpstreamError{StatusCode: 403, Body: `{"isQueued":true}`}, true},
		{"rate limit", &UpstreamError{StatusCode: 429}, true},
		{"server error", &UpstreamError{StatusCode: 500}, true},
		{"bad request", &UpstreamError{StatusCode: 400}, false},
		{"not found", &UpstreamError{StatusCode: 404}, false},
		{"network error", fmt.Errorf("connection refused"), true},
	}
	for _, tt := range tests {
		got := isRetryableError(tt.err)
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// ── SSE parser tests ─────────────────────────────────────────────────────

func TestUnwrapQoderSSENormalDone(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"Hello\\\"}}]}\"}\n\ndata: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\" world\\\"}}]}\"}\n\ndata: [DONE]\n"
	text, _, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("got %q, want %q", text, "Hello world")
	}
}

func TestUnwrapQoderSSENoDoneWithContent(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"Partial\\\"}}]}\"}\n\n"
	text, _, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Partial" {
		t.Errorf("got %q, want %q", text, "Partial")
	}
}

func TestUnwrapQoderSSENoDoneEmpty(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"\"}\n\n"
	text, _, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for empty stream without [DONE]")
	}
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
}

func TestUnwrapQoderSSEEnvelopeError(t *testing.T) {
	input := "data: {\"statusCodeValue\":500,\"body\":\"internal error\"}\n"
	_, _, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for 500 envelope")
	}
}

func TestUnwrapQoderSSEWithCallback(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"chunk1\\\"}}]}\"}\ndata: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"chunk2\\\"}}]}\"}\ndata: [DONE]\n"
	var chunks []string
	text, _, err := unwrapQoderSSE(strings.NewReader(input), func(s string) {
		chunks = append(chunks, s)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "chunk1chunk2" {
		t.Errorf("text: got %q", text)
	}
	if len(chunks) != 2 {
		t.Errorf("callback: expected 2 calls, got %d", len(chunks))
	}
}

func TestUnwrapQoderSSENativeToolCalls(t *testing.T) {
	// Simulate Qoder sending native tool_calls in SSE delta
	input := `data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"Let me search.\"}}]}"}
data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"tool_calls\":[{\"name\":\"web_search\",\"arguments\":{\"query\":\"test\"}}]}}]}"}
data: [DONE]
`
	text, nativeTC, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Let me search." {
		t.Errorf("text: got %q", text)
	}
	if len(nativeTC) != 1 {
		t.Fatalf("expected 1 native tool_call, got %d", len(nativeTC))
	}
	if name, _ := nativeTC[0]["name"].(string); name != "web_search" {
		t.Errorf("tool_call name: got %q", name)
	}
}

func TestParseInlineToolCalls(t *testing.T) {
	// Format 3: [assistant called tool: NAME with arguments: ARGS]
	input := `Oke, saya mulai.

[assistant called tool: read_file with arguments: {"limit":100,"offset":1,"path":"/home/ideagi/projects/qoder-bridge/qoder.go"}]`

	calls, cleanText := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 inline tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("name: got %q, want %q", calls[0].Function.Name, "read_file")
	}
	if !strings.Contains(calls[0].Function.Arguments, "qoder.go") {
		t.Errorf("arguments should contain path, got %q", calls[0].Function.Arguments)
	}
	if !strings.Contains(cleanText, "Oke, saya mulai") {
		t.Errorf("clean text should preserve prefix, got %q", cleanText)
	}
	if strings.Contains(cleanText, "[assistant called tool:") {
		t.Error("clean text should not contain tool call marker")
	}
}

func TestParseInlineToolCallsNestedBrackets(t *testing.T) {
	// Arguments contain arrays with ] — regex must not break
	input := `[assistant called tool: terminal with arguments: {"command":"echo 'test [1] [2]'","timeout":30}]`

	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "terminal" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "[1]") {
		t.Errorf("arguments should contain [1], got %q", calls[0].Function.Arguments)
	}
}

func TestParseInlineToolCallsMultiple(t *testing.T) {
	input := `[assistant called tool: web_search with arguments: {"query":"test"}]
[assistant called tool: read_file with arguments: {"path":"/tmp/x.go"}]`

	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 inline tool calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "web_search" {
		t.Errorf("call 0 name: got %q", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "read_file" {
		t.Errorf("call 1 name: got %q", calls[1].Function.Name)
	}
}

// ── Model resolution tests ──────────────────────────────────────────────

func TestResolveModelKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"qd/auto", "auto"},
		{"QD/Auto", "auto"},
		{"auto", "auto"},
		{"DeepSeek-V4-Pro", "dmodel"},
		{"Kimi-K3", "kmodel_latest"},
		{"combo-fast", "combo-fast"},
		{"qd/combo-fast", "combo-fast"},
	}
	for _, tt := range tests {
		got := resolveModelKey(tt.input)
		if got != tt.want {
			t.Errorf("resolveModelKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveCombo(t *testing.T) {
	combos = map[string][]string{"fast": {"lite", "efficient"}}
	defer func() { combos = nil }()

	tests := []struct {
		input   string
		wantOk  bool
		wantLen int
	}{
		{"qd/combo-fast", true, 2},
		{"combo-fast", true, 2},
		{"COMBO_FAST", true, 2},
		{"auto", false, 0},
	}
	for _, tt := range tests {
		models, ok := resolveCombo(tt.input)
		if ok != tt.wantOk {
			t.Errorf("resolveCombo(%q): ok=%v, want %v", tt.input, ok, tt.wantOk)
		}
		if len(models) != tt.wantLen {
			t.Errorf("resolveCombo(%q): len=%d, want %d", tt.input, len(models), tt.wantLen)
		}
	}
}

func TestResolveThinkingEffort(t *testing.T) {
	tests := []struct {
		req  ChatRequest
		want string
	}{
		{ChatRequest{ThinkingEffort: "high"}, "high"},
		{ChatRequest{ReasoningEffort: "ultra"}, "xhigh"},
		{ChatRequest{ReasoningEffort: "medium"}, "medium"},
		{ChatRequest{}, ""},
	}
	for _, tt := range tests {
		got := resolveThinkingEffort(tt.req)
		if got != tt.want {
			t.Errorf("resolveThinkingEffort(%+v) = %q, want %q", tt.req, got, tt.want)
		}
	}
}

func TestResolveContextWindow(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"kmodel_latest", 1000000},
		{"dmodel", 400000},
		{"auto", 200000},
	}
	for _, tt := range tests {
		got := resolveContextWindow(ChatRequest{}, tt.model)
		if got != tt.want {
			t.Errorf("resolveContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
	got := resolveContextWindow(ChatRequest{ContextWindow: 500000}, "auto")
	if got != 500000 {
		t.Errorf("explicit context window: got %d, want 500000", got)
	}
}

// ── Helpers tests ────────────────────────────────────────────────────────

func TestMaskPAT(t *testing.T) {
	got := maskPAT("pt-abcdefghijklmnopqrstuvwxyz")
	if len(got) < 10 {
		t.Errorf("mask too short: %q", got)
	}
	if got == "pt-abcdefghijklmnopqrstuvwxyz" {
		t.Error("PAT not masked")
	}
}

func TestExtractText(t *testing.T) {
	if got := extractText("hello"); got != "hello" {
		t.Errorf("string: got %q", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "part1"},
		map[string]interface{}{"type": "text", "text": "part2"},
	}
	if got := extractText(content); got != "part1\npart2" {
		t.Errorf("multipart: got %q", got)
	}
}

// ── Proxy round-robin tests ──────────────────────────────────────────────

func TestProxyClientFnRoundRobin(t *testing.T) {
	oldPool := proxyPool
	oldLabels := proxyLabels
	oldIdx := proxyIdx
	defer func() {
		proxyPool = oldPool
		proxyLabels = oldLabels
		proxyIdx = oldIdx
	}()

	proxyPool = []*http.Client{
		{Transport: streamingTransport()},
		{Transport: streamingTransport()},
	}
	proxyLabels = []string{"p1", "p2"}
	proxyIdx = 0

	c1 := proxyClientFn()
	c2 := proxyClientFn()
	c3 := proxyClientFn()
	if c1 != proxyPool[0] || c2 != proxyPool[1] || c3 != proxyPool[0] {
		t.Error("proxy round-robin not cycling correctly")
	}
}

func TestProxyCount(t *testing.T) {
	oldPool := proxyPool
	defer func() { proxyPool = oldPool }()

	proxyPool = nil
	if got := proxyCount(); got != 1 {
		t.Errorf("empty pool: got %d, want 1", got)
	}
	proxyPool = []*http.Client{{}, {}, {}}
	if got := proxyCount(); got != 3 {
		t.Errorf("3 proxies: got %d, want 3", got)
	}
}

func TestLoadEnvParsing(t *testing.T) {
	cfg := loadEnv("/nonexistent/.env")
	if cfg.port != 7100 {
		t.Errorf("default port: got %d, want 7100", cfg.port)
	}
	if cfg.strategy != "round-robin" {
		t.Errorf("default strategy: got %q", cfg.strategy)
	}
}

// ── Tool call parsing tests (balanced JSON) ──────────────────────────────

func TestParseToolCallsFromText_JSONBlock(t *testing.T) {
	text := `Let me search for that.

` + "```" + `json
{"tool_calls": [{"name": "web_search", "arguments": {"query": "hello world"}}]}
` + "```" + `

Done.`
	calls, clean := parseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "web_search" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
	if len(calls[0].ID) < 10 {
		t.Errorf("tool call ID too short: %q", calls[0].ID)
	}
	if !strings.HasPrefix(calls[0].ID, "call_") {
		t.Errorf("ID should start with call_: %q", calls[0].ID)
	}
	if clean == text {
		t.Error("tool_call block should be removed from clean text")
	}
}

func TestParseToolCallsFromText_BareJSON(t *testing.T) {
	text := `Some text {"tool_calls": [{"name": "terminal", "arguments": {"command": "ls -la"}}]} more text`
	calls, clean := parseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "terminal" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
	if !strings.Contains(clean, "Some text") {
		t.Error("prefix should be preserved")
	}
}

func TestParseToolCallsFromText_MultipleCalls(t *testing.T) {
	text := "```json\n" + `{"tool_calls": [{"name": "read_file", "arguments": {"path": "/tmp/a.go"}}, {"name": "terminal", "arguments": {"command": "ls"}}]}` + "\n```"
	calls, _ := parseToolCallsFromText(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" || calls[1].Function.Name != "terminal" {
		t.Errorf("wrong names: %q, %q", calls[0].Function.Name, calls[1].Function.Name)
	}
	// IDs should be unique
	if calls[0].ID == calls[1].ID {
		t.Error("tool call IDs should be unique")
	}
}

func TestParseToolCallsFromText_NestedJSON(t *testing.T) {
	// Nested objects in arguments — should be handled by brace counting
	text := `{"tool_calls": [{"name": "delegate_task", "arguments": {"goal": "test", "context": {"files": ["a.go", "b.go"], "nested": {"deep": true}}}}]}`
	calls, _ := parseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "delegate_task" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
	// Verify arguments contain nested JSON
	var args map[string]interface{}
	json.Unmarshal([]byte(calls[0].Function.Arguments), &args)
	if args["goal"] != "test" {
		t.Errorf("arguments wrong: %v", args)
	}
}

func TestParseToolCallsFromText_NoCalls(t *testing.T) {
	text := "Just a normal response without any tool calls."
	calls, clean := parseToolCallsFromText(text)
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
	if clean != text {
		t.Error("text should be unchanged when no tool calls")
	}
}

func TestParseToolCallsFromText_BareArray(t *testing.T) {
	text := "```json\n[{\"name\": \"test_fn\", \"arguments\": {\"x\": 1}}]\n```"
	calls, _ := parseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "test_fn" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
}

func TestGenerateCallID(t *testing.T) {
	id1 := generateCallID()
	id2 := generateCallID()
	if id1 == id2 {
		t.Error("IDs should be unique")
	}
	if !strings.HasPrefix(id1, "call_") {
		t.Errorf("should start with call_: %q", id1)
	}
	if len(id1) != 29 { // "call_" (5) + 24 hex chars
		t.Errorf("expected 29 chars, got %d: %q", len(id1), id1)
	}
}

// ── Tool result + assistant preservation ──────────────────────────────────

func TestNormalizeMessagesPreservesAssistant(t *testing.T) {
	// Empty assistant without tool_calls should be skipped (correct behavior)
	msgs := []ChatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "Follow up"},
	}
	out, _ := normalizeMessages(msgs, nil)
	// Empty assistant (no tool_calls, no content) = 0 output messages
	assistantCount := 0
	for _, m := range out {
		if m.Role == "assistant" {
			assistantCount++
		}
	}
	if assistantCount != 0 {
		t.Errorf("empty assistant without tool_calls should be skipped, got %d", assistantCount)
	}

	// Non-empty assistant should be preserved
	msgs2 := []ChatMessage{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "user", Content: "Follow up"},
	}
	out2, _ := normalizeMessages(msgs2, nil)
	found := false
	for _, m := range out2 {
		if m.Role == "assistant" {
			found = true
		}
	}
	if !found {
		t.Error("non-empty assistant should be preserved")
	}
}

func TestNormalizeMessagesToolResultWithID(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "Search for X"},
		{Role: "tool", Content: "Found: result123", Extra: map[string]interface{}{"tool_call_id": "call_abc123"}},
	}
	out, _ := normalizeMessages(msgs, nil)
	found := false
	for _, m := range out {
		if s, ok := m.Content.(string); ok && strings.Contains(s, "Tool Result") && strings.Contains(s, "call_abc123") {
			found = true
		}
	}
	if !found {
		t.Error("tool result should contain tool_result tag with call ID")
	}
}

func TestNormalizeMessagesAssistantWithToolCalls(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: "", Extra: map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "call_1", "function": map[string]interface{}{"name": "search", "arguments": "{}"}},
			},
		}},
	}
	out, _ := normalizeMessages(msgs, nil)
	if len(out) == 0 {
		t.Fatal("assistant with tool_calls should produce output")
	}
	s, ok := out[0].Content.(string)
	if !ok || !strings.Contains(s, "\"search\"") {
		t.Errorf("expected tool_calls json block, got %v", out[0].Content)
	}
	if !strings.Contains(s, "search") {
		t.Error("should contain tool name")
	}
}

func TestToolCallsInMessageNotChoice(t *testing.T) {
	// Hermes (and OpenAI clients) expect tool_calls inside message, not choice.
	input := `Here is the result.

` + "```" + `json
{"tool_calls": [{"name": "web_search", "arguments": {"query": "qoder bridge"}}]}
` + "```"

	calls, cleanText := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	// Simulate building a non-stream response
	choices := []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: cleanText, ToolCalls: calls}, FinishReason: "tool_calls"}}
	resp := ChatResponse{
		ID: "chatcmpl-test", Object: "chat.completion", Created: 1, Model: "qd/test",
		Choices: choices,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(b, &parsed)
	choices0 := parsed["choices"].([]interface{})[0].(map[string]interface{})
	if _, ok := choices0["tool_calls"]; ok {
		t.Error("tool_calls MUST NOT be at choice level")
	}
	msg := choices0["message"].(map[string]interface{})
	if _, ok := msg["tool_calls"]; !ok {
		t.Errorf("tool_calls MUST be inside message, got response: %s", string(b))
	}
	if msg["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "web_search" {
		t.Error("tool call function name wrong")
	}
}

func TestNormalizeMessagesToolsInSystemPrompt(t *testing.T) {
	tools := []ToolDef{
		{Function: ToolFunctionDef{Name: "read_file", Description: "Read a file", Parameters: map[string]interface{}{"path": "string"}}},
	}
	msgs := []ChatMessage{{Role: "user", Content: "test"}}
	_, system := normalizeMessages(msgs, tools)
	if !strings.Contains(system, "read_file") {
		t.Error("system prompt should contain tool name")
	}
	if !strings.Contains(system, "Tool Calling Protocol") {
		t.Error("system prompt should contain [CRITICAL: Tool Calling Protocol]")
	}
	if !strings.Contains(system, "```json") {
		t.Error("system prompt should contain ```json format instructions")
	}
	if !strings.Contains(system, "tool_calls") {
		t.Error("system prompt should mention tool_calls format")
	}
	if !strings.Contains(system, "TOOL CALLING RULES") {
		t.Error("system prompt should contain TOOL CALLING RULES")
	}
	if !strings.Contains(system, "NEVER describe") {
		t.Error("system prompt should contain NEVER describe instruction")
	}
}

// ── Tool Calling E2E Tests ────────────────────────────────────────────────

func TestParseToolCallsFromText_DelegateTask(t *testing.T) {
	input := "I'll delegate this to a subagent.\n\n" + "```" + `json
{"tool_calls": [{"name": "delegate_task", "arguments": {"goal": "Search for recent papers about LLM agents", "role": "leaf", "tasks": [{"goal": "Search arxiv", "context": "Focus on 2024-2025 papers"}]}}]}
` + "```"
	calls, cleanText := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	tc := calls[0]
	if tc.Function.Name != "delegate_task" {
		t.Errorf("expected delegate_task, got %s", tc.Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("failed to parse arguments: %v", err)
	}
	if args["goal"] == nil {
		t.Error("missing 'goal' in arguments")
	}
	if args["tasks"] == nil {
		t.Error("missing 'tasks' in arguments")
	}
	if !strings.Contains(cleanText, "delegate") {
		t.Error("clean text should preserve prefix")
	}
}

func TestParseToolCallsFromText_Terminal(t *testing.T) {
	input := "Let me check that.\n\n" + "```" + `json
{"tool_calls": [{"name": "terminal", "arguments": {"command": "go test -v ./..."}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "terminal" {
		t.Errorf("expected terminal, got %s", calls[0].Function.Name)
	}
	var args map[string]interface{}
	json.Unmarshal([]byte(calls[0].Function.Arguments), &args)
	if args["command"] != "go test -v ./..." {
		t.Errorf("wrong command: %v", args["command"])
	}
}

func TestParseToolCallsFromText_ReadFile(t *testing.T) {
	input := "```" + `json
{"tool_calls": [{"name": "read_file", "arguments": {"path": "/home/ideagi/projects/qoder-bridge/main.go", "offset": 100, "limit": 50}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("expected read_file, got %s", calls[0].Function.Name)
	}
}

func TestParseToolCallsFromText_SkillManage(t *testing.T) {
	input := "```" + `json
{"tool_calls": [{"name": "skill_manage", "arguments": {"action": "create", "name": "my-skill", "content": "---\nname: my-skill\n---\n# Skill"}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "skill_manage" {
		t.Errorf("expected skill_manage, got %s", calls[0].Function.Name)
	}
}

func TestParseToolCallsFromText_MultipleTools(t *testing.T) {
	input := "I'll read the file and then search for patterns.\n\n" + "```" + `json
{"tool_calls": [{"name": "read_file", "arguments": {"path": "/tmp/test.go"}}, {"name": "search_files", "arguments": {"pattern": "func main", "path": "/tmp"}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("first call should be read_file, got %s", calls[0].Function.Name)
	}
	if calls[1].Function.Name != "search_files" {
		t.Errorf("second call should be search_files, got %s", calls[1].Function.Name)
	}
}

func TestParseToolCallsFromText_BareJsonNoFence(t *testing.T) {
	// Model sometimes outputs JSON without ``` fences
	input := `{"tool_calls": [{"name": "terminal", "arguments": {"command": "ls -la"}}]}`
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call from bare JSON, got %d", len(calls))
	}
	if calls[0].Function.Name != "terminal" {
		t.Errorf("expected terminal, got %s", calls[0].Function.Name)
	}
}

func TestParseToolCallsFromText_PatchFile(t *testing.T) {
	// Use single-line arguments to avoid backtick raw string issues
	input := "```" + `json
{"tool_calls": [{"name": "patch", "arguments": {"mode": "replace", "path": "/tmp/test.go", "old_string": "func main()", "new_string": "func main() { return 42 }"}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "patch" {
		t.Errorf("expected patch, got %s", calls[0].Function.Name)
	}
	var args map[string]interface{}
	json.Unmarshal([]byte(calls[0].Function.Arguments), &args)
	newStr, _ := args["new_string"].(string)
	if !strings.Contains(newStr, "return 42") {
		t.Errorf("patch new_string should contain 'return 42', got %q", newStr)
	}
}

func TestParseToolCallsFromText_BrowserNavigate(t *testing.T) {
	input := "```" + `json
{"tool_calls": [{"name": "browser_navigate", "arguments": {"url": "https://example.com"}}]}
` + "```"
	calls, _ := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "browser_navigate" {
		t.Errorf("expected browser_navigate, got %s", calls[0].Function.Name)
	}
}

func TestParseToolCallsFromText_TextPrefixCleaned(t *testing.T) {
	input := "Let me analyze the code first.\n\n" + "```" + `json
{"tool_calls": [{"name": "read_file", "arguments": {"path": "/tmp/test.go"}}]}
` + "```"
	calls, cleanText := parseToolCallsFromText(input)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !strings.Contains(cleanText, "analyze") {
		t.Error("clean text should preserve prefix text about analysis")
	}
	if strings.Contains(cleanText, "read_file") {
		t.Error("clean text should NOT contain tool call JSON")
	}
}

func TestNormalizeMessages_PreservesToolResultChain(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "read file foo.txt"},
		{Role: "assistant", Content: "", Extra: map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "call_abc123", "function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"foo.txt"}`}},
			},
		}},
		{Role: "tool", Content: "file content here", Extra: map[string]interface{}{
			"tool_call_id": "call_abc123",
		}},
	}
	out, _ := normalizeMessages(msgs, nil)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (user, assistant+tool, tool-result), got %d", len(out))
	}
	// Check assistant has tool_calls in <tool_call> format
	aMsg, ok := out[1].Content.(string)
	if !ok {
		t.Fatal("assistant content should be string")
	}
	if !strings.Contains(aMsg, "read_file") {
		t.Error("assistant msg should contain tool call name")
	}
	if !strings.Contains(aMsg, "tool_call") {
		t.Error("assistant msg should have tool_call format")
	}
}

func TestNormalizeMessages_HandlesObjectArguments(t *testing.T) {
	// Anthropic / Gemini protocol sends arguments as a nested object,
	// not a JSON string. The bridge must accept both — silently dropping
	// to {} would erase user intent on tool calls.
	msgs := []ChatMessage{{
		Role: "assistant",
		Content: "",
		Extra: map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"name": "terminal",
						"arguments": map[string]interface{}{
							"command": "ls -la",
						},
					},
				},
			},
		},
	}}
	out, _ := normalizeMessages(msgs, nil)
	if len(out) == 0 {
		t.Fatal("expected one normalized message")
	}
	got := out[0].Content.(string)
	if !strings.Contains(got, `"command"`) || !strings.Contains(got, `"ls -la"`) {
		t.Errorf("expected command in tool call args, got: %s", got)
	}
	if !strings.Contains(got, "terminal") {
		t.Errorf("expected terminal tool name, got: %s", got)
	}
}

func TestRecoverPanicSendsErrorChunk(t *testing.T) {
	// After SSE headers are sent, a panic in the handler must NOT leave
	// the client hanging. recoverPanic must emit a final error chunk +
	// [DONE] before propagating.
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := &flushingRecorder{w, true}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic to be recovered")
		}
	}()
	func() {
		defer func() {
			if recoverPanic(w, flusher, "chatcmpl-x", 1, "test") {
				return
			}
			panic("simulated panic")
		}()
		panic("simulated panic")
	}()

	body := w.Body.String()
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Errorf("expected error finish_reason in SSE output, got: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("expected [DONE] terminator in SSE output, got: %s", body)
	}
}

func TestAccumulateNativeToolCalls(t *testing.T) {
	// Simulates upstream sending 3 incremental deltas for index 0:
	//   delta 1: id + name + arguments="{"
	//   delta 2: arguments="\"command\":\"ls\""
	//   delta 3: arguments="}"
	// Expected: single tool call with full arguments.
	deltas := []map[string]interface{}{
		{"index": 0, "id": "call_abc123", "type": "function", "function": map[string]interface{}{"name": "terminal", "arguments": "{"}},
		{"index": 0, "function": map[string]interface{}{"arguments": "\"command\":\"ls\""}},
		{"index": 0, "function": map[string]interface{}{"arguments": "}"}},
	}
	merged := accumulateNativeToolCalls(deltas)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged tool call, got %d", len(merged))
	}
	tc := merged[0]
	id, _ := tc["id"].(string)
	if id != "call_abc123" {
		t.Errorf("expected id call_abc123, got %s", id)
	}
	fn, _ := tc["function"].(map[string]interface{})
	name, _ := fn["name"].(string)
	if name != "terminal" {
		t.Errorf("expected name terminal, got %s", name)
	}
	args, _ := fn["arguments"].(string)
	expected := "{\"command\":\"ls\"}"
	if args != expected {
		t.Errorf("expected arguments %s, got %s", expected, args)
	}
}

func TestAccumulateNativeToolCallsMultipleIndices(t *testing.T) {
	// Two parallel tool calls at index 0 and 1.
	deltas := []map[string]interface{}{
		{"index": 0, "id": "call_a", "type": "function", "function": map[string]interface{}{"name": "read_file", "arguments": "{\"path\":\"/tmp/x\"}"}},
		{"index": 1, "id": "call_b", "type": "function", "function": map[string]interface{}{"name": "terminal", "arguments": "{\"command\":\"pwd\"}"}},
	}
	merged := accumulateNativeToolCalls(deltas)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged tool calls, got %d", len(merged))
	}
	fn0, _ := merged[0]["function"].(map[string]interface{})
	if fn0["name"] != "read_file" {
		t.Errorf("expected index 0 name=read_file, got %v", fn0["name"])
	}
	fn1, _ := merged[1]["function"].(map[string]interface{})
	if fn1["name"] != "terminal" {
		t.Errorf("expected index 1 name=terminal, got %v", fn1["name"])
	}
}

// flushingRecorder satisfies http.Flusher by delegating to the underlying
// httptest.ResponseRecorder's write calls. httptest.ResponseRecorder does
// NOT implement Flusher by default.
type flushingRecorder struct {
	*httptest.ResponseRecorder
	canFlush bool
}

func (f *flushingRecorder) Flush() {
	if f.canFlush {
		// no-op — recorder buffers, but Flush signals "ready for next chunk"
	}
}

func TestNormalizeMessages_ToolProtocolInSystemPrompt(t *testing.T) {
	tools := []ToolDef{
		{Function: ToolFunctionDef{Name: "terminal", Description: "Run shell command", Parameters: map[string]interface{}{"command": "string"}}},
		{Function: ToolFunctionDef{Name: "read_file", Description: "Read file", Parameters: map[string]interface{}{"path": "string"}}},
		{Function: ToolFunctionDef{Name: "delegate_task", Description: "Delegate to subagent", Parameters: map[string]interface{}{"goal": "string"}}},
	}
	msgs := []ChatMessage{{Role: "user", Content: "test"}}
	_, system := normalizeMessages(msgs, tools)
	// Check all tools mentioned
	for _, name := range []string{"terminal", "read_file", "delegate_task"} {
		if !strings.Contains(system, name) {
			t.Errorf("system prompt should mention tool: %s", name)
		}
	}
	// Check examples present
	if !strings.Contains(system, "read_file") {
		t.Error("should have read_file example")
	}
	if !strings.Contains(system, "terminal") {
		t.Error("should have terminal example")
	}
}
