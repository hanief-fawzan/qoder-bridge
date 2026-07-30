package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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
		{"auth 401", &UpstreamError{StatusCode: 401}, true},
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
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("got %q, want %q", text, "Hello world")
	}
}

func TestUnwrapQoderSSENoDoneWithContent(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"Partial\\\"}}]}\"}\n\n"
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Partial" {
		t.Errorf("got %q, want %q", text, "Partial")
	}
}

func TestUnwrapQoderSSENoDoneEmpty(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"\"}\n\n"
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for empty stream without [DONE]")
	}
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
}

func TestUnwrapQoderSSEEnvelopeError(t *testing.T) {
	input := "data: {\"statusCodeValue\":500,\"body\":\"internal error\"}\n"
	_, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for 500 envelope")
	}
}

func TestUnwrapQoderSSEWithCallback(t *testing.T) {
	input := "data: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"chunk1\\\"}}]}\"}\n\ndata: {\"statusCodeValue\":200,\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"chunk2\\\"}}]}\"}\n\ndata: [DONE]\n"
	var chunks []string
	text, err := unwrapQoderSSE(strings.NewReader(input), func(s string) {
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
		if s, ok := m.Content.(string); ok && strings.Contains(s, "tool_result") && strings.Contains(s, "call_abc123") {
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
	if !ok || !strings.Contains(s, "assistant tool_calls") {
		t.Errorf("expected tool_calls serialization, got %v", out[0].Content)
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
	if !strings.Contains(system, "Tool Protocol") {
		t.Error("system prompt should contain [Tool Protocol]")
	}
	if !strings.Contains(system, "```json") {
		t.Error("system prompt should contain ```json format instructions")
	}
	if !strings.Contains(system, "tool_calls") {
		t.Error("system prompt should mention tool_calls format")
	}
	if !strings.Contains(system, "Available tools") {
		t.Error("system prompt should contain English instructions")
	}
	if !strings.Contains(system, "如需调用工具") {
		t.Error("system prompt should contain Chinese instructions")
	}
}
