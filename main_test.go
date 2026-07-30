package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── PAT Pool tests ───────────────────────────────────────────────────────

func TestPATPoolRoundRobin(t *testing.T) {
	p := NewPATPool([]string{"pt-a", "pt-b", "pt-c"}, "round-robin")
	got := []string{
		p.Next(),
		p.Next(),
		p.Next(),
		p.Next(), // wraps around
	}
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

	// pt-a should be skipped; pt-b should be returned
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

	// Not pricing: plain 403
	err2 := &UpstreamError{StatusCode: 403, Body: `{"message":"forbidden"}`}
	if isPricingError(err2) {
		t.Error("expected isPricingError=false for plain 403")
	}

	// Not pricing: 401
	err3 := &UpstreamError{StatusCode: 401, Body: `{}`}
	if isPricingError(err3) {
		t.Error("expected isPricingError=false for 401")
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
	input := `data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}"}

data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\" world\"}}]}"}

data: [DONE]
`
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("got %q, want %q", text, "Hello world")
	}
}

func TestUnwrapQoderSSENoDoneWithContent(t *testing.T) {
	// Stream ends without [DONE] but has content — should succeed
	input := `data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"Partial\"}}]}"}

`
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Partial" {
		t.Errorf("got %q, want %q", text, "Partial")
	}
}

func TestUnwrapQoderSSENoDoneEmpty(t *testing.T) {
	// Stream ends without [DONE] and has NO content — should fail
	input := `data: {"statusCodeValue":200,"body":""}

`
	text, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for empty stream without [DONE]")
	}
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
}

func TestUnwrapQoderSSEEnvelopeError(t *testing.T) {
	input := `data: {"statusCodeValue":500,"body":"internal error"}
`
	_, err := unwrapQoderSSE(strings.NewReader(input), nil)
	if err == nil {
		t.Error("expected error for 500 envelope")
	}
}

func TestUnwrapQoderSSEWithCallback(t *testing.T) {
	input := `data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"chunk1\"}}]}"}

data: {"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"chunk2\"}}]}"}

data: [DONE]
`
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
	combos = map[string][]string{
		"fast": {"lite", "efficient"},
	}
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

// ── Thinking effort tests ────────────────────────────────────────────────

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

// ── Context window tests ─────────────────────────────────────────────────

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
	// Override: user specifies explicit value
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
	// String
	if got := extractText("hello"); got != "hello" {
		t.Errorf("string: got %q", got)
	}
	// Nil
	if got := extractText(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	// Multipart
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

	// Should cycle through proxies
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

// ── Env parsing tests ────────────────────────────────────────────────────

func TestLoadEnvParsing(t *testing.T) {
	// This tests env parsing logic by checking known patterns
	// Cannot easily test file I/O without creating temp files, but
	// validates the parsing function doesn't panic on edge cases
	cfg := loadEnv("/nonexistent/.env")
	if cfg.port != 7100 {
		t.Errorf("default port: got %d, want 7100", cfg.port)
	}
	if cfg.strategy != "round-robin" {
		t.Errorf("default strategy: got %q", cfg.strategy)
	}
}
