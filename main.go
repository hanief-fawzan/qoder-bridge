// main.go — OpenAI-compatible HTTP server for Qoder API.
//
// Bypasses qodercli entirely using pure-Go COSY signing + WAF encoding.
// No Node.js, no WASM, no cold start. ~50ms auth + 2-5s LLM response.
//
// Usage:
//
//	qoder-bridge                          # start daemon (background)
//	qoder-bridge run                      # foreground mode (for systemd)
//	qoder-bridge stop                     # stop daemon
//	qoder-bridge status                   # check if running
//	qoder-bridge update                   # pull, rebuild, restart
//	qoder-bridge quota                    # check quota and exit
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

// ── Config ──────────────────────────────────────────────────────────────────

var (
	port         int
	pats         []string
	patPool      *PATPool
	combos       map[string][]string // combo name -> model list
	requestDelay int                 // max random delay in ms (0 = disabled)
	startTime    = time.Now()        // server start time for uptime tracking
)

// ── Log Ring Buffer ─────────────────────────────────────────────────────────

type logRing struct {
	mu   sync.Mutex
	buf  []string
	pos  int
	size int
	full bool
}

var ringLog = &logRing{buf: make([]string, 500), size: 500}

func (l *logRing) Write(p []byte) (int, error) {
	s := strings.TrimSpace(string(p))
	if s == "" {
		return len(p), nil
	}
	l.mu.Lock()
	l.buf[l.pos] = s
	l.pos = (l.pos + 1) % l.size
	if l.pos == 0 {
		l.full = true
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *logRing) Lines(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := l.pos
	if l.full {
		total = l.size
	}
	if n <= 0 || n > total {
		n = total
	}
	out := make([]string, n)
	start := (l.pos - n + l.size) % l.size
	for i := 0; i < n; i++ {
		out[i] = l.buf[(start+i)%l.size]
	}
	return out
}

// ringBufWriter sends to both stderr and ring buffer.
type ringBufWriter struct {
	ring *logRing
	orig *os.File
}

func (t *ringBufWriter) Write(p []byte) (int, error) {
	t.orig.Write(p)
	t.ring.Write(p)
	return len(p), nil
}

// ── Models ──────────────────────────────────────────────────────────────────

// Known Qoder tier models (auto-routed by Qoder infrastructure).
var tierModels = []string{"auto", "ultimate", "performance", "efficient", "lite"}

// Known frontier models (mapped to real model names).
var frontierModels = map[string]string{
	"qmodel_38max":  "Qwen3.8-Max",
	"qmodel_latest":  "Qwen3.7-Max",
	"qmodel":         "Qwen3.7-Plus",
	"kmodel_latest":  "Kimi-K3",
	"kmodel":         "Kimi-K2.7-Code",
	"gm51model":      "GLM-5.2",
	"dmodel":         "DeepSeek-V4-Pro",
	"dfmodel":        "DeepSeek-V4-Flash",
	"mmodel":         "MiniMax-M3",
}

// Reverse map: display name -> internal key (built at init).
var displayNameToKey = func() map[string]string {
	m := make(map[string]string)
	for k, v := range frontierModels {
		m[strings.ToLower(v)] = k
	}
	return m
}()

// All known model keys for validation.
var knownModelKeys = func() map[string]bool {
	m := make(map[string]bool)
	for _, t := range tierModels {
		m[t] = true
	}
	for k := range frontierModels {
		m[k] = true
	}
	return m
}()

// ── PAT Pool (round-robin or random) ────────────────────────────────────────

type PATPool struct {
	mu       sync.RWMutex
	pats     []string
	idx      int
	strategy string // "round-robin" or "random"
	// cooldown tracks PATs that should be skipped until a deadline.
	// Key: PAT string, Value: time.Time when cooldown expires.
	cooldown map[string]time.Time
}

func NewPATPool(pats []string, strategy string) *PATPool {
	if strategy != "random" {
		strategy = "round-robin"
	}
	return &PATPool{pats: pats, strategy: strategy, cooldown: make(map[string]time.Time)}
}

// Cooldown marks a PAT as unusable for the given duration.
func (p *PATPool) Cooldown(pat string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldown[pat] = time.Now().Add(d)
}

// isAvailablePeek reports cooldown status without mutating the map.
// Used by /v1/status (read-only path) so a concurrent writer doesn't
// race with our read lock.
func (p *PATPool) isAvailablePeek(pat string) bool {
	if deadline, ok := p.cooldown[pat]; ok {
		return !time.Now().Before(deadline)
	}
	return true
}

// isAvailable returns true if the PAT is not in cooldown. Caller MUST
// hold p.mu (write — it may delete expired entries).
func (p *PATPool) isAvailable(pat string) bool {
	if deadline, ok := p.cooldown[pat]; ok {
		if time.Now().Before(deadline) {
			return false
		}
		delete(p.cooldown, pat) // expired, clean up
	}
	return true
}

func (p *PATPool) Next() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pats) == 0 {
		return ""
	}
	if p.strategy == "random" {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(p.pats))))
		if err != nil {
			return p.pats[0]
		}
		return p.pats[n.Int64()]
	}
	pat := p.pats[p.idx%len(p.pats)]
	p.idx++
	return pat
}

func (p *PATPool) All() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.pats))
	copy(out, p.pats)
	return out
}

// NextAvoid selects according to strategy without repeating a PAT in one retry cycle.
// Skips PATs in cooldown.
func (p *PATPool) NextAvoid(used map[string]bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pats) == 0 {
		return ""
	}
	if p.strategy == "random" {
		candidates := make([]int, 0, len(p.pats))
		for i, pat := range p.pats {
			if !used[pat] && p.isAvailable(pat) {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 {
			return ""
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(candidates))))
		if err != nil {
			return p.pats[candidates[0]]
		}
		return p.pats[candidates[n.Int64()]]
	}
	for i := 0; i < len(p.pats); i++ {
		pat := p.pats[p.idx%len(p.pats)]
		p.idx++
		if !used[pat] && p.isAvailable(pat) {
			return pat
		}
	}
	return ""
}

func (p *PATPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pats)
}

// ── OpenAI types ────────────────────────────────────────────────────────────

type ChatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
	// Extra stores tool_calls and tool_call_id from deserialized OpenAI messages.
	// These fields aren't in the standard JSON but are injected by the proxy.
	Extra map[string]interface{} `json:"-"`
}

type ToolDef struct {
	Type     string          `json:"type"`
	Function ToolFunctionDef `json:"function"`
}

type ToolFunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type ChatRequest struct {
	Model          string        `json:"model"`
	Messages       []ChatMessage `json:"messages"`
	Stream         bool          `json:"stream"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int        `json:"max_completion_tokens,omitempty"`
	Tools          []ToolDef     `json:"tools,omitempty"`
	ToolChoice     interface{}   `json:"tool_choice,omitempty"`
	ThinkingEffort string        `json:"thinking_effort,omitempty"`     // low, medium, high, xhigh
	ContextWindow  int           `json:"context_window,omitempty"`      // 200000, 400000, 1000000
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`    // Hermes sends this (OpenAI standard)
	StreamOptions  *StreamOptions `json:"stream_options,omitempty"`     // {include_usage: true}
	Temperature    *float64      `json:"temperature,omitempty"`
	TopP           *float64      `json:"top_p,omitempty"`
	Stop           interface{}   `json:"stop,omitempty"`
	User           string        `json:"user,omitempty"`
	Seed           *int          `json:"seed,omitempty"`
	N              *int          `json:"n,omitempty"`
	ResponseFormat interface{}   `json:"response_format,omitempty"`
}

// StreamOptions mirrors OpenAI's stream_options field. When set with
// include_usage=true, the bridge emits a final SSE chunk carrying Usage
// data even if upstream doesn't natively supply it.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
}

type Message struct {
	Role      string      `json:"role"`
	Content   interface{} `json:"content"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type SSEChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []SSEChoice `json:"choices"`
}

type SSEChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason,omitempty"`
}

// ── Handlers ────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": "cosy-pure-go"})
}

// handleStatus returns diagnostics: proxy egress IP, DB info, PAT pool status, uptime.
// This helps debug whether MicroWARP/proxy is working and DB is logging.

type patInfo struct {
	PAT       string `json:"pat"`
	Available bool   `json:"available"`
	Cooldown  string `json:"cooldown,omitempty"`
}

type StatusResponse struct {
	Uptime      string    `json:"uptime"`
	Engine      string    `json:"engine"`
	Proxy       string    `json:"proxy"`
	ProxyCount  int       `json:"proxy_count"`
	EgressIP    string    `json:"egress_ip"`
	DBPath      string    `json:"db_path"`
	DBWorking   bool      `json:"db_working"`
	DBSize      string    `json:"db_size"`
	LogCount    int64     `json:"log_count"`
	PATStrategy string    `json:"pat_strategy"`
	PATCount    int       `json:"pat_count"`
	PATs        []patInfo `json:"pats"`
	Combos      []string  `json:"combos"`
}

// Egress IP cached for 5 minutes — checking api.ipify.org on every
// /v1/status request adds 200-500ms latency for no fresh data. The
// proxy pool itself is the only thing that can change the egress IP
// and that's user-driven (config save), so we explicitly invalidate.
var (
	egressIPMu  sync.RWMutex
	egressIPVal string
	egressIPExp time.Time
)

func fetchEgressIP(client *http.Client) string {
	egressIPMu.RLock()
	if time.Now().Before(egressIPExp) && egressIPVal != "" {
		v := egressIPVal
		egressIPMu.RUnlock()
		return v
	}
	egressIPMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=text", nil)
	if resp, err := client.Do(req); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ip := strings.TrimSpace(string(body))
		egressIPMu.Lock()
		egressIPVal = ip
		egressIPExp = time.Now().Add(5 * time.Minute)
		egressIPMu.Unlock()
		return ip
	} else {
		// Stale-on-error: if we have a previous value, keep it but tag
		// the failure so the user can see something is wrong.
		egressIPMu.RLock()
		stale := egressIPVal
		egressIPMu.RUnlock()
		if stale != "" {
			return stale + " (stale: " + err.Error() + ")"
		}
		return "check failed: " + err.Error()
	}
}

func invalidateEgressCache() {
	egressIPMu.Lock()
	egressIPExp = time.Time{}
	egressIPMu.Unlock()
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	pats := patPool.All()
	resp := StatusResponse{
		Uptime:      time.Since(startTime).Round(time.Second).String(),
		Engine:      "pure Go COSY",
		DBPath:      dbLocation(),
		DBWorking:   db != nil,
		PATStrategy: patPool.strategy,
		PATCount:    len(pats),
		Proxy:       getProxyInfo(),
		ProxyCount:  proxyCount(),
	}

	// Egress IP — cached for 5 minutes (see fetchEgressIP).
	if client := proxyClientFn(); client != nil {
		resp.EgressIP = fetchEgressIP(client)
	}

	// DB stats
	if db != nil {
		if info, err := os.Stat(dbLocation()); err == nil {
			resp.DBSize = fmt.Sprintf("%d KB", info.Size()/1024)
		}
		db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&resp.LogCount)
	}

	// PAT pool status — read-only snapshot via RLock + isAvailablePeek so
	// we don't race with concurrent Cooldown() writers (delete on a
	// read-locked map is a Go race detector fire).
	patPool.mu.RLock()
	for _, pat := range pats {
		info := patInfo{PAT: maskPAT(pat), Available: patPool.isAvailablePeek(pat)}
		if deadline, ok := patPool.cooldown[pat]; ok && time.Now().Before(deadline) {
			info.Cooldown = fmt.Sprintf("%dm remaining", int(time.Until(deadline).Minutes())+1)
		}
		resp.PATs = append(resp.PATs, info)
	}
	patPool.mu.RUnlock()

	// Combo names
	if combos != nil {
		for name := range combos {
			resp.Combos = append(resp.Combos, name)
		}
		sort.Strings(resp.Combos)
	}

	json.NewEncoder(w).Encode(resp)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 50
	if s := r.URL.Query().Get("n"); s != "" {
		if v, err := fmt.Sscanf(s, "%d", &n); err == nil && v == 1 {
			// ok
		}
	}
	lines := ringLog.Lines(n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total": len(lines),
		"lines": lines,
	})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	type ModelEntry struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		Created       int64  `json:"created"`
		OwnedBy       string `json:"owned_by"`
		ContextLength int    `json:"context_length,omitempty"`
		Name          string `json:"name,omitempty"`
	}

	models := []ModelEntry{}
	for _, t := range tierModels {
		models = append(models, ModelEntry{
			ID: "qd/" + t, Object: "model", Created: 1, OwnedBy: "qoder",
			ContextLength: modelContextSize(t),
		})
	}
	keys := make([]string, 0, len(frontierModels))
	for k := range frontierModels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		models = append(models, ModelEntry{
			ID: frontierModels[k], Object: "model", Created: 1, OwnedBy: "qoder",
			ContextLength: modelContextSize(k),
			Name: k,
		})
	}

	// Add combos
	if combos != nil {
		comboNames := make([]string, 0, len(combos))
		for name := range combos {
			comboNames = append(comboNames, name)
		}
		sort.Strings(comboNames)
		for _, name := range comboNames {
			models = append(models, ModelEntry{
				ID: "qd/combo-" + name, Object: "model", Created: 1, OwnedBy: "qoder-combo",
				ContextLength: 200000,
				Name: "combo-" + name,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": models})
}

// modelContextSize returns the max context window for a model key.
// Used in /v1/models response so Hermes auto-detects the right context length.
func modelContextSize(modelKey string) int {
	switch modelKey {
	case "kmodel_latest", "kmodel": // Kimi
		return 1000000
	case "gm51model": // GLM-5.2
		return 1000000
	case "mmodel": // MiniMax-M3
		return 1000000
	case "qmodel_latest", "qmodel": // Qwen3.7-Max/Plus
		return 400000
	case "qmodel_preview": // Qwen3.8-Max-Preview
		return 400000
	case "dmodel", "dfmodel": // DeepSeek
		return 400000
	default: // tier models (auto, ultimate, etc.)
		return 200000
	}
}

func handleCombos(w http.ResponseWriter, r *http.Request) {
	if combos == nil || len(combos) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"combos": map[string]interface{}{}})
		return
	}
	result := make(map[string]interface{})
	for name, models := range combos {
		result["qd/combo-"+name] = models
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"combos": result})
}

// ── API key auth model ──────────────────────────────────────────────────
//
// Single source of truth: the api_keys DB table + globalEnabled flag.
//
//   • globalEnabled = true → Bearer required, must match an enabled row.
//   • globalEnabled = false → open access, no Bearer required.
//
// Disabled rows in the table do NOT count as valid bearers (treat as
// nonexistent). A row whose enabled column is 0 is invisible to auth.
//
// generateKey() automatically flips globalEnabled ON so the freshly
// issued credential is actually demanded.
var (
	globalEnabled bool   // master switch (api_key_enabled in config)
	authMu        sync.RWMutex
)

// authRequired reports whether the next request must carry a valid Bearer.
// Read-mostly path: RWMutex so /v1/status doesn't block the hot path.
func authRequired() bool {
	authMu.RLock()
	defer authMu.RUnlock()
	return globalEnabled
}

// setGlobalAuth flips the master switch and persists to config table.
// Called from the TUI ("Require API Key" toggle) and from generateKey.
func setGlobalAuth(on bool) {
	authMu.Lock()
	globalEnabled = on
	authMu.Unlock()
	cfgSet("api_key_enabled", boolToStr(on))
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// authMiddleware wraps an http.Handler and enforces Bearer auth when
// api_key_enabled is ON. /health is excluded at the mux level.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /health is always public (load balancer probes, uptime checks).
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if authRequired() {
			auth := r.Header.Get("Authorization")
			key := strings.TrimPrefix(auth, "Bearer ")
			_, perms, ok := validateAPIKey(key)
			if !ok {
				hint := " Bearer required — provide a valid sk-* key."
				all, _ := listAPIKeys()
				if len(all) == 0 {
					hint = " No API keys configured — generate one via TUI (API Keys → Generate) or set the toggle to OFF."
				} else {
					enabled, _ := listEnabledAPIKeys()
					if len(enabled) == 0 {
						hint = " All API keys are disabled — enable one in TUI or set the toggle to OFF."
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				fmt.Fprintf(w, `{"error":{"message":"invalid API key.%s"}}`, hint)
				return
			}
			if !hasPermission(perms, r.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				fmt.Fprintf(w, `{"error":{"message":"key does not have permission for %s"}}`, r.URL.Path)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC in handleChat: %v\n%s", r, debug.Stack())
			// Best-effort error response — header may already be sent.
			if w.Header().Get("Content-Type") == "" {
				sendJSONError(w, 500, "internal server error")
			}
		}
	}()
	if r.Method != "POST" {
		http.Error(w, `{"error":{"message":"POST required"}}`, 405)
		return
	}

	// Auth: middleware already enforced. This block just attributes the key.
	usedAPIKey := "(no key)"
	if auth := r.Header.Get("Authorization"); auth != "" {
		key := strings.TrimPrefix(auth, "Bearer ")
		if name, _, ok := validateAPIKey(key); ok {
			usedAPIKey = name
		}
	}

	// Buffer body so we can decode twice (ChatRequest + raw extra fields).
	// 10MB cap matches typical OpenAI proxies; beyond that we surface a
	// 413 instead of silently truncating (previous behavior).
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		sendJSONError(w, 400, "failed to read body")
		return
	}
	if int64(len(bodyBytes)) == 10<<20 {
		sendJSONError(w, 413, "request body exceeds 10MB limit")
		return
	}

	var req ChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		sendJSONError(w, 400, fmt.Sprintf("bad request: %s", err))
		return
	}

	// Extract tool_calls and tool_call_id into Extra map
	// ponytail: single-pass raw decode, only for messages that need it
	var rawReq struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(bodyBytes, &rawReq) == nil && len(rawReq.Messages) == len(req.Messages) {
		for i, raw := range rawReq.Messages {
			var extra struct {
				ToolCalls  json.RawMessage `json:"tool_calls"`
				ToolCallID string          `json:"tool_call_id"`
			}
			if json.Unmarshal(raw, &extra) == nil {
				if len(extra.ToolCalls) > 0 || extra.ToolCallID != "" {
					req.Messages[i].Extra = make(map[string]interface{})
					if len(extra.ToolCalls) > 0 {
						var tcParsed interface{}
						json.Unmarshal(extra.ToolCalls, &tcParsed)
						req.Messages[i].Extra["tool_calls"] = tcParsed
					}
					if extra.ToolCallID != "" {
						req.Messages[i].Extra["tool_call_id"] = extra.ToolCallID
					}
				}
			}
		}
	}

	if patPool.Len() == 0 {
		http.Error(w, `{"error":{"message":"no PATs configured"}}`, 503)
		return
	}

	modelInput := req.Model
	if modelInput == "" {
		sendJSONError(w, 400, "model field is required")
		return
	}
	if len(req.Messages) == 0 {
		sendJSONError(w, 400, "messages field is required and cannot be empty")
		return
	}
	modelKey := resolveModelKey(modelInput)

	// Resolve thinking effort (Hermes sends reasoning_effort, Qoder expects thinking_effort)
	req.ThinkingEffort = resolveThinkingEffort(req)
	// Resolve context window (auto-set based on model tier if not specified)
	req.ContextWindow = resolveContextWindow(req, modelKey)
	// Resolve max_tokens from max_completion_tokens (OpenAI standard field)
	if req.MaxTokens <= 0 && req.MaxCompletionTokens > 0 {
		req.MaxTokens = req.MaxCompletionTokens
	} else if req.MaxCompletionTokens > 0 && req.MaxCompletionTokens < req.MaxTokens {
		req.MaxTokens = req.MaxCompletionTokens
	}

	// Check if this is a combo request
	if comboModels, isCombo := resolveCombo(modelInput); isCombo {
		if req.Stream && len(req.Tools) == 0 {
			handleComboStream(w, r, req, modelInput, comboModels, usedAPIKey)
		} else if req.Stream {
			handleBufferedComboStream(w, r, req, modelInput, comboModels, usedAPIKey)
		} else {
			handleComboNonStream(w, r, req, modelInput, comboModels, usedAPIKey)
		}
		return
	}

	// When tools are present, use buffered stream: buffer the full response,
	// parse tool_calls from text, then emit clean SSE chunks.
	// Streaming raw ```json tool-call blocks to the client would leak them.
	// Matches qoder-proxy: "with tools → buffered path below."
	if req.Stream && len(req.Tools) == 0 {
		handleStream(w, r, req, modelKey, usedAPIKey)
	} else if req.Stream {
		handleBufferedStream(w, r, req, modelKey, usedAPIKey)
	} else {
		handleNonStream(w, r, req, modelKey, usedAPIKey)
	}
}

// sendFinalSSEChunk emits the final finish_reason chunk and optional
// usage chunk (when include_usage is true). Closes the SSE stream with
// [DONE]. This is the canonical end-of-stream emission for chat
// completions; matches OpenAI's wire format so clients (Hermes/Claude/
// Codex) can parse the last chunk reliably.
//
// Usage stats are computed from the local estimator — the upstream
// Qoder API does not currently emit usage in its stream envelopes, so
// we synthesize it. The numbers are token *estimates*; clients that
// rely on exact usage should not use this path.
func sendFinalSSEChunk(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, includeUsage bool, finishReason string, messages []ChatMessage, result *ChatResult) {
	if flusher == nil {
		return
	}
	// 1. finish_reason chunk
	sendSSE(w, flusher, SSEChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &finishReason}},
	})
	// 2. usage chunk (OpenAI extension)
	if includeUsage {
		promptTokens := 0
		for _, m := range messages {
			promptTokens += estimateTokens(extractText(m.Content))
		}
		completionTokens := 0
		if result != nil {
			completionTokens = estimateTokens(result.Text)
			for _, tc := range result.ToolCalls {
				completionTokens += estimateTokens(tc.ID + tc.Function.Name + tc.Function.Arguments)
			}
		}
		usageJSON, _ := json.Marshal(map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		})
		// emit usage-only chunk with choices: []
		fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[],\"usage\":%s}\n\n",
			id, created, model, string(usageJSON))
	}
	// 3. terminator
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// emitStreamUsageOnlyChunk sends just the usage chunk (no finish_reason
// already sent). Used after an error chunk that itself carried
// finish_reason="error" but the client still expects usage accounting.
func emitStreamUsageOnlyChunk(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, messages []ChatMessage, result *ChatResult) {
	if flusher == nil {
		return
	}
	promptTokens := 0
	for _, m := range messages {
		promptTokens += estimateTokens(extractText(m.Content))
	}
	completionTokens := 0
	if result != nil {
		completionTokens = estimateTokens(result.Text)
	}
	usageJSON, _ := json.Marshal(map[string]interface{}{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	})
	fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[],\"usage\":%s}\n\n",
		id, created, model, string(usageJSON))
	flusher.Flush()
}

// wantsStreamUsage returns whether the client requested include_usage
// via stream_options. nil stream_options or include_usage=false both
// return false.
func wantsStreamUsage(req ChatRequest) bool {
	if req.StreamOptions == nil {
		return false
	}
	return req.StreamOptions.IncludeUsage
}

func handleStream(w http.ResponseWriter, r *http.Request, req ChatRequest, modelKey string, apikey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()
	defer func() {
		if recoverPanic(w, flusher, id, created, req.Model) {
			return
		}
	}()
	first := true

	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, func(sc StreamChunk) {
		if first {
			if flusher != nil {
				sendSSE(w, flusher, SSEChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
				})
			}
			first = false
		}
		if flusher != nil {
			field := "content"
			if sc.Kind == "reasoning" {
				field = "reasoning_content"
			}
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"` + field + `":` + mustQuote(sc.Text) + `}`)}},
			})
		}
	}, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)

	if err != nil {
		log.Printf("stream error: %v", err)
		// Emit error as a proper finish_reason chunk (not a content chunk
		// — putting errors in `content` causes Hermes/Claude to confuse
		// them with model output).
		if flusher != nil {
			errReason := "error"
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &errReason}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
		return
	} else if result != nil && len(result.ToolCalls) > 0 {
		// Emit tool_calls as OpenAI-compatible chunk
		if flusher != nil {
			for i, tc := range result.ToolCalls {
				tcDelta, _ := json.Marshal(map[string]interface{}{
					"index": i,
					"id":    tc.ID,
					"type":  "function",
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
				sendSSE(w, flusher, SSEChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"tool_calls":[` + string(tcDelta) + `]}`)}},
				})
			}
		}
	}

	if flusher != nil {
		finishReason := "stop"
		if result != nil && len(result.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		sendFinalSSEChunk(w, flusher, id, created, req.Model, wantsStreamUsage(req), finishReason, req.Messages, result)
	}
}

func handleNonStream(w http.ResponseWriter, r *http.Request, req ChatRequest, modelKey string, apikey string) {
	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)
	if err != nil {
		forwardUpstreamError(w, err)
		return
	}

	text := ""
	if result != nil {
		text = result.Text
	}

	// Defense-in-depth: catch empty responses at handler level too.
	if result == nil || (len(result.ToolCalls) == 0 && strings.TrimSpace(text) == "") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   map[string]interface{}{"message": fmt.Sprintf("Qoder returned empty response for model '%s'. Possible causes: model unavailable, quota exhausted, or upstream timeout.", modelKey)},
		})
		return
	}

	choices := []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: text}, FinishReason: "stop"}}
	if len(result.ToolCalls) > 0 {
		// Return tool_calls in OpenAI format
		choices = []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: text, ToolCalls: result.ToolCalls}, FinishReason: "tool_calls"}}
	}

	promptTokens := 0
	for _, m := range req.Messages {
		promptTokens += estimateTokens(extractText(m.Content))
	}
	completionTokens := 0
	if result != nil {
		completionTokens = estimateTokens(result.Text)
		for _, tc := range result.ToolCalls {
			completionTokens += estimateTokens(tc.ID + tc.Function.Name + tc.Function.Arguments)
		}
	}

	resp := ChatResponse{
		ID:      "chatcmpl-" + uuidString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: choices,
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleBufferedStream buffers the full Qoder response, parses tool_calls from text,
// then emits clean SSE chunks. Used when stream=true but tools are present —
// the model may emit tool_calls as ```json blocks that must not leak to the client.
// Matches qoder-proxy's buffered path for tool-call requests.
func handleBufferedStream(w http.ResponseWriter, r *http.Request, req ChatRequest, modelKey string, apikey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()
	defer func() {
		if recoverPanic(w, flusher, id, created, req.Model) {
			return
		}
	}()

	// Buffer: no streaming callback
	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)

	if err != nil {
		log.Printf("buffered stream error: %v", err)
		// Emit error as a single finish_reason="error" chunk — putting
		// the error into `content` makes Hermes/Claude interpret it as
		// model output rather than a transport failure.
		if flusher != nil {
			errReason := "error"
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &errReason}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
		return
	}

	if flusher == nil {
		return
	}

	// Send role chunk
	sendSSE(w, flusher, SSEChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
	})

	text := ""
	if result != nil {
		text = result.Text
	}

	// Defense-in-depth: catch empty responses in buffered stream too.
	if result == nil || (len(result.ToolCalls) == 0 && strings.TrimSpace(text) == "") {
		log.Printf("buffered stream: empty response for %s", modelKey)
		errReason := "error"
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &errReason}},
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	hasToolCalls := result != nil && len(result.ToolCalls) > 0
	if hasToolCalls {
		// Send prefix text if any
		if strings.TrimSpace(text) != "" {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
			})
		}
		// Send each tool_call as a chunk
		for i, tc := range result.ToolCalls {
			tcDelta, _ := json.Marshal(map[string]interface{}{
				"index": i,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"tool_calls":[` + string(tcDelta) + `]}`)}},
			})
		}
		reason := "tool_calls"
		sendFinalSSEChunk(w, flusher, id, created, req.Model, wantsStreamUsage(req), reason, req.Messages, result)
	} else {
		// Text-only response: emit content + final stop chunk
		if text != "" {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
			})
		}
		stop := "stop"
		sendFinalSSEChunk(w, flusher, id, created, req.Model, wantsStreamUsage(req), stop, req.Messages, result)
	}
}

// ── Combo handlers ──────────────────────────────────────────────────────────

func handleComboNonStream(w http.ResponseWriter, r *http.Request, req ChatRequest, comboName string, modelList []string, apikey string) {
	const maxRounds = 3
	var lastErr error
	for round := 0; round < maxRounds; round++ {
		for _, model := range modelList {
			modelKey := resolveModelKey(model)
			if !knownModelKeys[modelKey] {
				log.Printf("combo %q: model %q not in known list, trying anyway", comboName, model)
			}
			log.Printf("combo %q: round %d, trying qd/%s", comboName, round+1, modelKey)

			result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)
			if err == nil {
				text := ""
				if result != nil {
					text = result.Text
				}

				// Treat empty response as error — try next model.
				if result == nil || (len(result.ToolCalls) == 0 && strings.TrimSpace(text) == "") {
					log.Printf("combo %q: qd/%s returned empty response, trying next", comboName, modelKey)
					lastErr = fmt.Errorf("empty response from %s", modelKey)
					continue
				}

				choices := []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: text}, FinishReason: "stop"}}
				if len(result.ToolCalls) > 0 {
					choices = []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: text, ToolCalls: result.ToolCalls}, FinishReason: "tool_calls"}}
				}

				promptTokens := 0
				for _, m := range req.Messages {
					promptTokens += estimateTokens(extractText(m.Content))
				}
				completionTokens := 0
				if result != nil {
					completionTokens = estimateTokens(result.Text)
					for _, tc := range result.ToolCalls {
						completionTokens += estimateTokens(tc.ID + tc.Function.Name + tc.Function.Arguments)
					}
				}

				resp := ChatResponse{
					ID:      "chatcmpl-" + uuidString(),
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   comboName,
					Choices: choices,
					Usage: Usage{
						PromptTokens:     promptTokens,
						CompletionTokens: completionTokens,
						TotalTokens:      promptTokens + completionTokens,
					},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
			lastErr = err
			log.Printf("combo %q: qd/%s failed: %v", comboName, modelKey, err)
			if isPricingError(err) {
				log.Printf("combo %q: qd/%s PAT quota exhausted (code 112), PAT rotated", comboName, modelKey)
			}
		}
		if round+1 < maxRounds {
			log.Printf("combo %q: round %d complete, all models failed, starting round %d", comboName, round+1, round+2)
		}
	}
	forwardUpstreamError(w, fmt.Errorf("combo %s: all %d rounds exhausted, last: %w", comboName, maxRounds, lastErr))
}

func handleComboStream(w http.ResponseWriter, r *http.Request, req ChatRequest, comboName string, modelList []string, apikey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()
	defer func() {
		if recoverPanic(w, flusher, id, created, comboName) {
			return
		}
	}()

	const maxRounds = 3
	var lastErr error
	roleSent := false
comboRounds:
	for round := 0; round < maxRounds; round++ {
		for _, model := range modelList {
			modelKey := resolveModelKey(model)
			if !knownModelKeys[modelKey] {
				log.Printf("combo %q: model %q not in known list, trying anyway", comboName, model)
			}
			log.Printf("combo %q: round %d, trying qd/%s", comboName, round+1, modelKey)

			first := true
			result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, func(sc StreamChunk) {
				if first {
					if flusher != nil && !roleSent {
						sendSSE(w, flusher, SSEChunk{
							ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
							Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
						})
						roleSent = true
					}
					first = false
				}
				if flusher != nil {
					field := "content"
					if sc.Kind == "reasoning" {
						field = "reasoning_content"
					}
					sendSSE(w, flusher, SSEChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
						Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"` + field + `":` + mustQuote(sc.Text) + `}`)}},
					})
				}
			}, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)

			if err == nil {
				_ = result
				// Treat empty as failure — try next model.
				text := ""
				if result != nil {
					text = result.Text
				}
				if result == nil || (len(result.ToolCalls) == 0 && strings.TrimSpace(text) == "") {
					log.Printf("combo stream %q: qd/%s returned empty, trying next", comboName, modelKey)
					lastErr = fmt.Errorf("empty response from %s", modelKey)
					continue
				}
				if flusher != nil {
					// Emit tool_calls if present
					if result != nil && len(result.ToolCalls) > 0 {
						if !roleSent {
							sendSSE(w, flusher, SSEChunk{
								ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
								Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant"}`)}},
							})
							roleSent = true
						}
						for i, tc := range result.ToolCalls {
							tcDelta, _ := json.Marshal(map[string]interface{}{
								"index": i, "id": tc.ID, "type": "function",
								"function": map[string]interface{}{
									"name": tc.Function.Name, "arguments": tc.Function.Arguments,
								},
							})
							sendSSE(w, flusher, SSEChunk{
								ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
								Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"tool_calls":[` + string(tcDelta) + `]}`)}},
							})
						}
						reason := "tool_calls"
						sendSSE(w, flusher, SSEChunk{
							ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
							Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &reason}},
						})
					} else {
						stop := "stop"
						sendSSE(w, flusher, SSEChunk{
							ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
							Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &stop}},
						})
					}
					fmt.Fprintf(w, "data: [DONE]\n\n")
					flusher.Flush()
				}
				return
			}
			lastErr = err
			log.Printf("combo %q: qd/%s failed: %v", comboName, modelKey, err)
			if isPricingError(err) {
				log.Printf("combo %q: qd/%s PAT quota exhausted (code 112), PAT rotated", comboName, modelKey)
			}
			// Do not retry another model after partial output reached client.
			if roleSent {
				break comboRounds
			}
		}
		if round+1 < maxRounds {
			log.Printf("combo %q: round %d complete, all models failed, starting round %d", comboName, round+1, round+2)
		}
	}

	// All models failed across all rounds
	if flusher != nil {
		errMsg := fmt.Sprintf("\n\n[Error: combo %s: all %d rounds exhausted, last: %s]", comboName, maxRounds, lastErr)
		if !roleSent {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
			})
		}
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(errMsg) + `}`)}},
		})
		stop := "stop"
		sendFinalSSEChunk(w, flusher, id, created, comboName, wantsStreamUsage(req), stop, req.Messages, nil)
	}
}

// handleBufferedComboStream buffers the full response for each combo model,
// parses tool_calls from text, then emits clean SSE chunks. Used when
// stream=true and tools are present, so raw ```json tool-call blocks don't leak.
func handleBufferedComboStream(w http.ResponseWriter, r *http.Request, req ChatRequest, comboName string, modelList []string, apikey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()
	defer func() {
		if recoverPanic(w, flusher, id, created, comboName) {
			return
		}
	}()

	const maxRounds = 3
	var lastErr error
	for round := 0; round < maxRounds; round++ {
		for _, model := range modelList {
			modelKey := resolveModelKey(model)
			if !knownModelKeys[modelKey] {
				log.Printf("combo %q: model %q not in known list, trying anyway", comboName, model)
			}
			log.Printf("combo %q: round %d, trying qd/%s", comboName, round+1, modelKey)

			// Buffer: no streaming callback
			result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow, r.RemoteAddr, apikey)
			if err == nil {
				emitBufferedComboSSE(w, flusher, id, created, comboName, result, req.Messages, wantsStreamUsage(req))
				return
			}
			lastErr = err
			log.Printf("combo %q: qd/%s failed: %v", comboName, modelKey, err)
			if isPricingError(err) {
				log.Printf("combo %q: qd/%s PAT quota exhausted (code 112), PAT rotated", comboName, modelKey)
			}
		}
		if round+1 < maxRounds {
			log.Printf("combo %q: round %d complete, all models failed, starting round %d", comboName, round+1, round+2)
		}
	}

	// All models failed across all rounds
	if flusher != nil {
		errMsg := fmt.Sprintf("[Error: combo %s: all %d rounds exhausted, last: %s]", comboName, maxRounds, lastErr)
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
		})
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(errMsg) + `}`)}},
		})
		stop := "stop"
		sendFinalSSEChunk(w, flusher, id, created, comboName, wantsStreamUsage(req), stop, req.Messages, nil)
	}
}

// emitBufferedComboSSE emits a buffered combo response as OpenAI-compatible SSE.
func emitBufferedComboSSE(w http.ResponseWriter, flusher http.Flusher, id string, created int64, comboName string, result *ChatResult, messages []ChatMessage, includeUsage bool) {
	if flusher == nil {
		return
	}

	sendSSE(w, flusher, SSEChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
		Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
	})

	text := ""
	if result != nil {
		text = result.Text
	}

	hasToolCalls := result != nil && len(result.ToolCalls) > 0

	if hasToolCalls {
		if strings.TrimSpace(text) != "" {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
			})
		}
		for i, tc := range result.ToolCalls {
			tcDelta, _ := json.Marshal(map[string]interface{}{
				"index": i, "id": tc.ID, "type": "function",
				"function": map[string]interface{}{
					"name": tc.Function.Name, "arguments": tc.Function.Arguments,
				},
			})
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"tool_calls":[` + string(tcDelta) + `]}`)}},
			})
		}
		reason := "tool_calls"
		sendFinalSSEChunk(w, flusher, id, created, comboName, includeUsage, reason, messages, result)
	} else {
		if text != "" {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
			})
		}
		stop := "stop"
		sendFinalSSEChunk(w, flusher, id, created, comboName, includeUsage, stop, messages, result)
	}
}

// startQuotaMonitor checks PAT quota every 5 minutes and cooldowns exhausted ones.
// This prevents pricing 112 errors by skipping PATs that are already at limit.
func startQuotaMonitor(pool *PATPool) {
	// Run once at startup, then every 5 minutes.
	run := func() {
		pats := pool.All()
		var wg sync.WaitGroup
		for _, pat := range pats {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				q := fetchQuota(p)
				if q.Error != "" {
					return // can't check, don't cooldown
				}
				if q.IsQuotaExceeded || (q.Limit > 0 && q.Used >= q.Limit) {
					// Cooldown until reset date if available, otherwise 5 min.
					cooldown := 5 * time.Minute
					if q.ResetDate != "" {
						if t, err := time.Parse(time.RFC3339, q.ResetDate); err == nil {
							if d := time.Until(t); d > 0 {
								cooldown = d
							}
						}
					}
					pool.Cooldown(p, cooldown)
					log.Printf("quota: PAT %s exhausted (%d/%d), cooldown %v", maskPAT(p), q.Used, q.Limit, cooldown.Round(time.Minute))
				}
			}(pat)
		}
		wg.Wait()
	}

	run() // initial check
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		run()
	}
}

func handleQuota(w http.ResponseWriter, r *http.Request) {
	pats := patPool.All()
	results := make([]QuotaInfo, len(pats))

	var wg sync.WaitGroup
	for i, pat := range pats {
		wg.Add(1)
		go func(idx int, p string) {
			defer wg.Done()
			results[idx] = fetchQuota(p)
		}(i, pat)
	}
	wg.Wait()

	// ?detailed=true returns per-PAT breakdown. Default returns aggregate.
	if r.URL.Query().Get("detailed") == "true" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
		return
	}

	var totalUsed, totalRemaining, totalLimit int64
	var exhaustedCount int
	for _, q := range results {
		totalUsed += q.Used
		totalRemaining += q.Remaining
		totalLimit += q.Limit
		if q.IsQuotaExceeded || (q.Limit > 0 && q.Used >= q.Limit) {
			exhaustedCount++
		}
	}
	aggregate := map[string]interface{}{
		"total_used":      totalUsed,
		"total_remaining": totalRemaining,
		"total_limit":     totalLimit,
		"pat_count":       len(results),
		"pat_active":      len(results) - exhaustedCount,
		"pat_exhausted":   exhaustedCount,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(aggregate)
}

// handleUpstreamModels dumps the raw model list from the Qoder API so we can
// see which models exist upstream (including ones not yet mapped in frontierModels).
func handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	pats := patPool.All()
	if len(pats) == 0 {
		http.Error(w, `{"error":"no PATs configured"}`, 500)
		return
	}
	cred, err := getCredential(pats[0])
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"credential: %s"}`, err.Error()), 500)
		return
	}
	// Force a fresh fetch (retry=true bypasses cache).
	mc, err := fetchModelConfig(cred, "__list__", true)
	_ = mc
	if err != nil {
		// fetchModelConfig returns error when key not found, but cache is populated.
		// Fall through to read cache.
	}
	configs := allCachedModelConfigs()
	if configs == nil {
		http.Error(w, `{"error":"model list cache empty"}`, 500)
		return
	}
	type entry struct {
		Key         string `json:"key"`
		DisplayName string `json:"display_name"`
		IsReasoning bool   `json:"is_reasoning"`
		Source      string `json:"source"`
		Mapped      bool   `json:"mapped"`
	}
	out := make([]entry, 0, len(configs))
	for _, c := range configs {
		_, mapped := frontierModels[c.Key]
		out = append(out, entry{
			Key:         c.Key,
			DisplayName: c.DisplayName,
			IsReasoning: c.IsReasoning,
			Source:      c.Source,
			Mapped:      mapped,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":  len(out),
		"models": out,
	})
}

// ── PAT rotation ────────────────────────────────────────────────────────────

func runWithPATRotation(ctx context.Context, pool *PATPool, modelKey string, messages []ChatMessage, maxTokens int, onChunk StreamCallback, tools []ToolDef, thinkingEffort string, contextWindow int, clientIP string, apikey string) (*ChatResult, error) {
	start := time.Now()

	// Smart anti-ban delay: random jitter between 0 and requestDelay ms
	if requestDelay > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(requestDelay)))
		if err == nil {
			time.Sleep(time.Duration(n.Int64()) * time.Millisecond)
		}
	}

	// Try at most three distinct PATs while respecting random/round-robin selection.
	maxAttempts := pool.Len()
	if maxAttempts > 3 {
		maxAttempts = 3
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var result *ChatResult
	var lastErr error
	var pat string
	used := make(map[string]bool, maxAttempts)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		pat = pool.NextAvoid(used)
		if pat == "" {
			break
		}
		used[pat] = true
		log.Printf("qd %s: selected PAT %s (attempt %d/%d)", modelKey, maskPAT(pat), attempt+1, maxAttempts)
		emitted := false
		callback := onChunk
		if onChunk != nil {
			callback = func(sc StreamChunk) {
				emitted = true
				onChunk(sc)
			}
		}
		result, lastErr = callQoder(ctx, pat, modelKey, messages, maxTokens, callback, tools, thinkingEffort, contextWindow)
		if lastErr == nil && result != nil && (strings.TrimSpace(result.Text) != "" || len(result.ToolCalls) > 0) {
			// Auto-retry: tools requested but model responded text-only.
			// Retry up to 2 times with progressively stronger injection.
			if len(tools) > 0 && len(result.ToolCalls) == 0 && !emitted {
				toolList := ""
				for _, t := range tools {
					if toolList != "" {
						toolList += ", "
					}
					toolList += t.Function.Name
				}
				// Retry 1: strong user reminder
				log.Printf("qd %s: text-only response despite tools — retry 1/2", modelKey)
				retryMsgs := make([]ChatMessage, len(messages))
				copy(retryMsgs, messages)
				retryMsgs = append(retryMsgs, ChatMessage{
					Role:    "user",
					Content: "You MUST call one of these tools NOW: [" + toolList + "]. " +
						"Output ONLY a ```json block with {\"tool_calls\": [{\"name\": \"...\", \"arguments\": {...}}]}. " +
						"Do NOT explain. Do NOT say you don't have tools. Just output the JSON block.",
				})
				result, lastErr = callQoder(ctx, pat, modelKey, retryMsgs, maxTokens, callback, tools, thinkingEffort, contextWindow)
				if lastErr == nil && result != nil && len(result.ToolCalls) > 0 {
					log.Printf("qd %s: retry 1 succeeded — got %d tool_calls", modelKey, len(result.ToolCalls))
				} else if lastErr == nil && result != nil && len(result.ToolCalls) == 0 {
					// Retry 2: bare minimum prompt — replace system prompt with tool-only instruction
					log.Printf("qd %s: still text-only — retry 2/2 with bare prompt", modelKey)
					bareMsgs := []ChatMessage{
						{Role: "system", Content: "You are a tool-calling machine. You MUST call tools. Never output text."},
						{Role: "user", Content: fmt.Sprintf("%v\n\nCALL TOOLS NOW: %s", messages[len(messages)-1].Content, toolList)},
					}
					result, lastErr = callQoder(ctx, pat, modelKey, bareMsgs, maxTokens, callback, tools, thinkingEffort, contextWindow)
					if lastErr == nil && result != nil && len(result.ToolCalls) > 0 {
						log.Printf("qd %s: retry 2 succeeded — got %d tool_calls", modelKey, len(result.ToolCalls))
					}
				}
			}
			if lastErr == nil && result != nil && (strings.TrimSpace(result.Text) != "" || len(result.ToolCalls) > 0) {
				goto done
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("empty response from upstream")
		}
		log.Printf("qd %s error: %v (pat: %s, attempt %d/%d)", modelKey, lastErr, maskPAT(pat), attempt+1, maxAttempts)
		// Cooldown PATs with pricing/quota errors (5 minutes)
		if isPricingError(lastErr) {
			pool.Cooldown(pat, 5*time.Minute)
			log.Printf("qd %s: PAT %s in cooldown for 5m (pricing limit)", modelKey, maskPAT(pat))
		}
		if isQueueError(lastErr) {
			pool.Cooldown(pat, 2*time.Minute)
			log.Printf("qd %s: PAT %s in cooldown for 2m (queue/rate-limit)", modelKey, maskPAT(pat))
		}
		// Cooldown on empty response — PAT may be exhausted or model unavailable.
		// Short cooldown (1m) since it might be temporary.
		if isEmptyResponseError(lastErr) {
			pool.Cooldown(pat, 1*time.Minute)
			log.Printf("qd %s: PAT %s in cooldown for 1m (empty response)", modelKey, maskPAT(pat))
		}
		// Retrying after streaming bytes would concatenate two generations.
		// Also stop if error is not retryable (e.g. pricing/quota limit).
		if emitted || !isRetryableError(lastErr) || ctx.Err() != nil {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
			}
			break
		}
	}
done:

	// Log to DB.
	// Prompt tokens are still charged on failure (bytes were sent upstream),
	// but completion + credits are zeroed — failed requests consume only
	// input-side cost. credits reflects the *billable* outcome.
	latency := time.Since(start).Milliseconds()
	promptTokens := 0
	for _, m := range messages {
		promptTokens += estimateTokens(extractText(m.Content))
	}
	completionTokens := 0
	if lastErr == nil && result != nil {
		completionTokens = estimateTokens(result.Text)
		if len(result.ToolCalls) > 0 {
			for _, tc := range result.ToolCalls {
				completionTokens += estimateTokens(tc.ID + tc.Function.Name + tc.Function.Arguments)
			}
		}
	}
	totalTokens := promptTokens + completionTokens
	// Estimate credits only on success — failed requests are logged but
	// not billed (the upstream may have already failed before charging).
	credits := 0.0
	if lastErr == nil {
		credits = estimateCredits(modelKey, totalTokens)
	}

	status := 200
	errMsg := ""
	if lastErr != nil {
		status = upstreamStatusCode(lastErr)
		errMsg = lastErr.Error()
	}

	logRequest(LogEntry{
		PAT:              maskPAT(pat),
		Model:            modelKey,
		Stream:           onChunk != nil,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		Credits:          credits,
		Status:           status,
		Error:            errMsg,
		LatencyMs:        latency,
		ClientIP:         clientIP,
				APIKey:           maskAPIKey(apikey),
			})

	proxyLabel := getProxyInfo()
	if lastErr != nil {
		log.Printf("qd %s: request failed: %v (pat: %s, proxy: %s, %dms)", modelKey, lastErr, maskPAT(pat), proxyLabel, latency)
	} else {
		log.Printf("qd %s: request ok (pat: %s, proxy: %s, %d tokens, %dms)", modelKey, maskPAT(pat), proxyLabel, totalTokens, latency)
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return result, nil
}

// upstreamStatusCode extracts HTTP status from an UpstreamError or guesses from error text.
func upstreamStatusCode(err error) int {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.StatusCode
	}
	return 502
}

func isAuthError(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.StatusCode == 401 {
			return true
		}
		// 403 is auth only if it's NOT a pricing/quota error (code 112)
		if ue.StatusCode == 403 && !isPricingError(err) {
			return true
		}
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401") || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "expired")
}

// isPricingError returns true for 403 code 112 — this PAT's quota/pricing limit.
// The PAT is exhausted; rotating to another PAT may succeed.
func isPricingError(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.StatusCode == 403 && strings.Contains(ue.Body, "\"112\"")
	}
	return false
}

// isQueueError returns true for a Qoder queue/rate-limit error (403 with isQueued=true).
func isQueueError(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.StatusCode == 403 && strings.Contains(ue.Body, "isQueued")
	}
	return false
}

// isRetryableError returns true if trying a different PAT might succeed.
// Plain 401 is NOT retryable — the credential is bad, not exhausted.
// Other PATs can't recover an expired/invalid token.
func isRetryableError(err error) bool {
	if errors.Is(err, errModelNotInList) {
		return false // unknown model: rotating PATs won't help
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.StatusCode == 401 {
			return false // auth error: rotating won't help
		}
	}
	if isPricingError(err) {
		return true // quota exhausted — next PAT may have remaining
	}
	if isQueueError(err) {
		return true // queue backpressure — different PAT may not be queued
	}
	if errors.As(err, &ue) {
		return ue.StatusCode == 429 || ue.StatusCode >= 500
	}
	// network / parse errors are retryable
	return true
}

// isEmptyResponseError returns true for 502 empty response errors.
func isEmptyResponseError(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.StatusCode == 502 && strings.Contains(ue.Body, "empty_response")
	}
	return false
}

// ── Model & combo resolution ────────────────────────────────────────────────

// resolveModelKey strips prefixes and normalizes a model name.
// Accepts: "qd/auto", "QD/Auto", "qoder/auto", "auto", "apore/auto" -> "auto"
func resolveModelKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))

	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		prefix := m[:idx]
		m = m[idx+1:]
		if prefix != "qd" && prefix != "qoder" {
			log.Printf("model: prefix %q auto-converted to qd/ (using qd/%s)", prefix, m)
		}
	}

	// Check if it's a display name (e.g. "DeepSeek-V4-Pro" → "dmodel")
	if key, ok := displayNameToKey[m]; ok {
		return key
	}

	return m
}

// resolveCombo checks if the model request is a combo and returns the model list.
// Accepts: "qd/combo-fast", "combo-fast", "COMBO_FAST", "combo_fast"
func resolveCombo(model string) ([]string, bool) {
	if combos == nil {
		return nil, false
	}

	m := strings.ToLower(strings.TrimSpace(model))

	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		m = m[idx+1:]
	}

	m = strings.TrimPrefix(m, "combo-")
	m = strings.TrimPrefix(m, "combo_")
	m = strings.ReplaceAll(m, "_", "-")

	if models, ok := combos[m]; ok {
		return models, true
	}
	return nil, false
}

// resolveThinkingEffort resolves thinking effort from multiple sources.
// Priority: thinking_effort > reasoning_effort (Hermes OpenAI standard).
// Maps "ultra" → "xhigh" (Qoder max).
func resolveThinkingEffort(req ChatRequest) string {
	effort := req.ThinkingEffort
	if effort == "" {
		effort = req.ReasoningEffort
	}
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "low"
	case "medium", "mid":
		return "medium"
	case "high":
		return "high"
	case "ultra", "xhigh", "max":
		return "xhigh"
	default:
		return ""
	}
}

// resolveContextWindow auto-sets context window based on model tier.
// Source: https://docs.qoder.com/user-guide/chat/model-tier-selector
// 200K=standard, 400K=extended, 1M=extreme
func resolveContextWindow(req ChatRequest, modelKey string) int {
	if req.ContextWindow > 0 {
		return req.ContextWindow
	}
	// Auto-detect by model tier
	switch modelKey {
	case "kmodel_latest", "kmodel": // Kimi — supports 1M
		return 1000000
	case "qmodel_latest", "qmodel": // Qwen3.7-Max/Plus — supports 400K
		return 400000
	case "dmodel", "dfmodel": // DeepSeek — supports 400K
		return 400000
	case "gm51model": // GLM-5.2 — supports 1M
		return 1000000
	case "mmodel": // MiniMax-M3 — 1M context
		return 1000000
	case "qmodel_preview": // Qwen3.8-Max-Preview — 400K
		return 400000
	default: // tier models (auto, ultimate, etc.) — standard 200K
		return 200000
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

var maskRe = regexp.MustCompile(`^(.{4}).*(.{4})$`)

func maskPAT(pat string) string {
	if len(pat) > 10 {
		return maskRe.ReplaceAllString(pat, "$1...$2")
	}
	return "***"
}

// maskAPIKey returns a safe-to-log representation of a key name.
// The DB stores the human-readable label (e.g. "hermes-desktop"),
// never the raw sk-* secret — so the name IS safe to log verbatim.
func maskAPIKey(name string) string {
	if name == "" || name == "(no key)" {
		return ""
	}
	return name
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sendSSE(w http.ResponseWriter, f http.Flusher, chunk SSEChunk) {
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	f.Flush()
}

// sendJSONError writes a JSON error response with the given status code.
// Safe to call after streaming has started — it only sets a header if no
// header has been written yet, so subsequent streaming chunks still go out
// (the JSON error appears as the final chunk body if SSE headers were set).
func sendJSONError(w http.ResponseWriter, code int, msg string) {
	h, ok := w.(http.ResponseWriter)
	_ = ok
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(h).Encode(map[string]interface{}{"error": map[string]string{"message": msg}})
}

// recoverPanic writes a final SSE error chunk when a streaming handler
// panics, so the client never hangs on a half-dead connection. Returns
// true if recovered; the caller should return immediately after.
func recoverPanic(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string) bool {
	if r := recover(); r != nil {
		log.Printf("PANIC recovered in handler: %v\n%s", r, debug.Stack())
		if flusher != nil {
			errReason := "error"
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &errReason}},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
		return true
	}
	return false
}

// forwardUpstreamError forwards the raw Qoder API error to the client,
// preserving the upstream status code and body for debugging.
func forwardUpstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, errModelNotInList) {
		keys := make([]string, 0, len(frontierModels))
		for k := range frontierModels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("%s — Qoder silently downgrades unknown model keys to the default model, so this request is rejected. Valid keys: qd/%s", err.Error(), strings.Join(keys, ", qd/")),
			},
		})
		return
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ue.StatusCode)
		// Try to forward as structured JSON if possible
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(ue.Body), &parsed) == nil {
			json.NewEncoder(w).Encode(parsed)
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"message": ue.Body}})
		}
		return
	}
	sendJSONError(w, 502, err.Error())
}

// ── .env loader ─────────────────────────────────────────────────────────────

type envConfig struct {
	pats         []string
	port         int
	strategy     string
	combos       map[string][]string
	requestDelay int    // max random delay in ms between requests (0 = disabled)
	domain       string // optional: public domain for endpoint URLs
}

func loadEnv(envPath string) *envConfig {
	cfg := &envConfig{
		port:     7100,
		strategy: "round-robin",
		combos:   make(map[string][]string),
	}

	// Find .env file
	if envPath == "" {
		for _, p := range []string{".env", filepath.Join(os.Getenv("HOME"), ".env")} {
			if _, err := os.Stat(p); err == nil {
				envPath = p
				break
			}
		}
	}
	if envPath == "" {
		return cfg
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		return cfg
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			if strings.HasPrefix(line, "pt-") {
				cfg.pats = append(cfg.pats, line)
			}
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "\"'")

		switch {
		case key == "QODER_PORT":
			var p int
			if _, err := fmt.Sscanf(val, "%d", &p); err == nil && p > 0 {
				cfg.port = p
			}

		case key == "PAT_STRATEGY":
			if val == "random" || val == "round-robin" {
				cfg.strategy = val
			}

		case key == "QODER_PATS":
			for _, p := range strings.Split(val, ",") {
				p = strings.TrimSpace(p)
				if p != "" && strings.HasPrefix(p, "pt-") {
					cfg.pats = append(cfg.pats, p)
				}
			}

		case strings.HasPrefix(key, "COMBO_"):
			comboName := strings.ToLower(strings.TrimPrefix(key, "COMBO_"))
			comboName = strings.ReplaceAll(comboName, "_", "-")
			var models []string
			for _, m := range strings.Split(val, ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					models = append(models, m)
				}
			}
			if len(models) > 0 {
				cfg.combos[comboName] = models
			}

		case key == "REQUEST_DELAY_MS":
			var d int
			if _, err := fmt.Sscanf(val, "%d", &d); err == nil && d > 0 {
				cfg.requestDelay = d
			}

		case key == "QODER_PROXY":
			if val != "" {
				os.Setenv("QODER_PROXY", val)
			}

		case key == "QODER_DOMAIN":
			if val != "" {
				cfg.domain = val
			}

		case strings.HasPrefix(key, "pt-"):
			cfg.pats = append(cfg.pats, key)
		}
	}

	return cfg
}

// ── CLI quota command ───────────────────────────────────────────────────────

func runQuotaCLI(envPath string) {
	cfg := loadEnv(envPath)
	if len(cfg.pats) == 0 {
		fmt.Fprintln(os.Stderr, "error: no PATs configured in .env")
		os.Exit(1)
	}

	patPool = NewPATPool(cfg.pats, cfg.strategy)
	initProxyClient()

	fmt.Println("Fetching quota for", len(cfg.pats), "PAT(s)...")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PAT\tUSED\tREMAINING\tLIMIT\tRESET\tSTATUS")

	for _, pat := range cfg.pats {
		q := fetchQuota(pat)
		status := "ok"
		if q.Error != "" {
			status = q.Error
		}
		reset := q.ResetDate
		if reset == "" {
			reset = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\n", q.PAT, q.Used, q.Remaining, q.Limit, reset, status)
	}
	w.Flush()
}

// ── Daemon helpers ──────────────────────────────────────────────────────────

func pidFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".qoder-bridge.pid")
}

func logFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".qoder-bridge.log")
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid, err
}

func writePID(pid int) error {
	return os.WriteFile(pidFilePath(), []byte(fmt.Sprintf("%d\n", pid)), 0644)
}

func removePID() {
	os.Remove(pidFilePath())
}

func isRunning(pid int) bool {
	return processExists(pid)
}

// ── Subcommands ─────────────────────────────────────────────────────────────

func runServe(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	envFlag := fs.String("env", "", "Path to .env file")
	portFlag := fs.Int("port", 0, "Listen port (overrides QODER_PORT in .env)")
	patsFlag := fs.String("pats", "", "Comma-separated PAT list (overrides .env)")
	fs.Parse(args)

	cfg := loadEnv(*envFlag)

	// Initialize proxy-aware HTTP client after .env is loaded
	initProxyClient()

	if *portFlag > 0 {
		cfg.port = *portFlag
	}

	if *patsFlag != "" {
		cfg.pats = nil
		for _, p := range strings.Split(*patsFlag, ",") {
			p = strings.TrimSpace(p)
			if p != "" && strings.HasPrefix(p, "pt-") {
				cfg.pats = append(cfg.pats, p)
			}
		}
	}

	if len(cfg.pats) == 0 {
		fmt.Fprintln(os.Stderr, "error: no PATs configured.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Add PATs to .env file:")
		fmt.Fprintln(os.Stderr, "  pt-your-first-pat-here")
		fmt.Fprintln(os.Stderr, "  pt-your-second-pat-here")
		fmt.Fprintln(os.Stderr, "  QODER_PORT=7100")
		os.Exit(1)
	}

	pats = cfg.pats
	patPool = NewPATPool(pats, cfg.strategy)
	port = cfg.port
	if len(cfg.combos) > 0 {
		combos = cfg.combos
	}
	if cfg.requestDelay > 0 {
		requestDelay = cfg.requestDelay
	}

	// Initialize DB (optional — bridge works without it)
	if err := initDB(); err != nil {
		log.Printf("warn: db init failed: %v (logging disabled)", err)
	} else {
		// Auto-import .env values to DB on first run
		importEnvFromConfig(cfg)
		// DB config overrides .env
		setGlobalAuth(cfgBool("api_key_enabled", false))
		if v := cfgGet("proxy"); v != "" {
			os.Setenv("QODER_PROXY", v)
			initProxyClient()
		}
		if v := cfgGet("request_delay_ms"); v != "" {
			var d int
			if _, err := fmt.Sscanf(v, "%d", &d); err == nil && d > 0 {
				requestDelay = d
			}
		}
	}

	// Enforce DB size limit (~100MB, 365 days)
	if db != nil {
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				enforceDBLimit(100)
			}
		}()
	}

	// Pre-warm credentials
	log.Printf("warming %d PAT(s) [strategy: %s]...", len(pats), cfg.strategy)
	for _, pat := range pats {
		cred, err := getCredential(pat)
		if err != nil {
			log.Printf("  warn: %s: %v", maskPAT(pat), err)
		} else {
			uid := cred.userID
			if len(uid) > 12 {
				uid = uid[:12] + "..."
			}
			log.Printf("  ok: %s -> user %s", maskPAT(pat), uid)
		}
	}

	// Pre-fetch model config
	log.Printf("fetching model config...")
	for _, pat := range pats {
		cred, _ := getCredential(pat)
		if cred != nil {
			_, err := fetchModelConfig(cred, "auto", false)
			if err != nil {
				log.Printf("  warn: model config: %v", err)
			} else {
				modelConfigCacheMu.RLock()
				log.Printf("  ok: %d models loaded", len(modelConfigCache))
				modelConfigCacheMu.RUnlock()
			}
			break
		}
	}

	// Log combos
	if combos != nil && len(combos) > 0 {
		log.Printf("combos loaded:")
		comboNames := make([]string, 0, len(combos))
		for name := range combos {
			comboNames = append(comboNames, name)
		}
		sort.Strings(comboNames)
		for _, name := range comboNames {
			log.Printf("  qd/combo-%s: %s", name, strings.Join(combos[name], " -> "))
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/quota", handleQuota)
	mux.HandleFunc("/v1/upstream-models", handleUpstreamModels)
	mux.HandleFunc("/v1/combos", handleCombos)
	mux.HandleFunc("/v1/status", handleStatus)
	mux.HandleFunc("/v1/logs", handleLogs)

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Tee log output to ring buffer so /v1/logs works.
	log.SetOutput(&ringBufWriter{ring: ringLog, orig: os.Stderr})

	log.Printf("")
	log.Printf("qoder-bridge (cosy-pure-go)")
	log.Printf("  ready on %s", addr)
	log.Printf("    chat:    http://%s/v1/chat/completions", addr)
	log.Printf("    models:  http://%s/v1/models", addr)
	log.Printf("    quota:   http://%s/v1/quota", addr)
	log.Printf("    combos:  http://%s/v1/combos", addr)
	log.Printf("    status:  http://%s/v1/status", addr)
	log.Printf("    health:  http://%s/health", addr)
	log.Printf("  db:      %s", dbLocation())
	log.Printf("  engine:  pure Go COSY (no qodercli)")
	log.Printf("  proxy:   %s", getProxyInfo())
	if authRequired() {
			log.Printf("  auth:    required (Bearer sk-* key)")
		} else {
			log.Printf("  auth:    open access (no key required)")
		}
		log.Printf("ready to accept connections.")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	srv := &http.Server{
		Addr:              addr,
		Handler:           authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,  // slowloris protection
		WriteTimeout:      0,                 // SSE streams can run indefinitely
		IdleTimeout:       120 * time.Second, // close idle keep-alive connections
		MaxHeaderBytes:    1 << 20,           // 1MB max header
	}

	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // drain active connections
		removePID()
		os.Exit(0)
	}()

	// Background quota checker: every 5 min, cooldown exhausted PATs
	// so they're skipped before wasting a request attempt.
	go startQuotaMonitor(patPool)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func runDaemonize(args []string) {
	// Check if already running
	if pid, err := readPID(); err == nil && isRunning(pid) {
		fmt.Fprintf(os.Stderr, "qoder-bridge is already running (PID %d)\n", pid)
		fmt.Fprintf(os.Stderr, "Stop with: qoder-bridge stop\n")
		os.Exit(1)
	}

	removePID()

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot find executable: %v", err)
	}

	logFile := logFilePath()
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatalf("cannot open log file %s: %v", logFile, err)
	}

	childArgs := append([]string{"run"}, args...)

	proc, err := os.StartProcess(exe, childArgs, &os.ProcAttr{
		Dir:   ".",
		Files: []*os.File{os.Stdin, f, f},
		Sys:   sysProcAttr(),
	})
	f.Close()
	if err != nil {
		log.Fatalf("cannot start daemon: %v", err)
	}

	writePID(proc.Pid)

	// Wait for startup
	time.Sleep(3 * time.Second)

	if !isRunning(proc.Pid) {
		fmt.Fprintf(os.Stderr, "qoder-bridge failed to start. Check logs:\n")
		fmt.Fprintf(os.Stderr, "  tail -20 %s\n", logFile)
		removePID()
		os.Exit(1)
	}

	fmt.Printf("qoder-bridge started (PID %d)\n", proc.Pid)
	fmt.Printf("  logs:    tail -f %s\n", logFile)
	fmt.Printf("  stop:    qoder-bridge stop\n")
	fmt.Printf("  status:  qoder-bridge status\n")
}

func runStop(args []string) {
	// Try PID file first (daemon mode)
	if pid, err := readPID(); err == nil && isRunning(pid) {
		if err := sendSignal(pid); err != nil {
			fmt.Fprintf(os.Stderr, "cannot stop PID %d: %v\n", pid, err)
			os.Exit(1)
		}
		for i := 0; i < 30; i++ {
			time.Sleep(100 * time.Millisecond)
			if !isRunning(pid) {
				break
			}
		}
		removePID()
		fmt.Printf("qoder-bridge stopped (PID %d)\n", pid)
		return
	}

	// Try systemd (system or user)
	if err := exec.Command("systemctl", "stop", "qoder-bridge").Run(); err == nil {
		fmt.Println("qoder-bridge stopped (systemd)")
		return
	}
	if err := exec.Command("systemctl", "--user", "stop", "qoder-bridge").Run(); err == nil {
		fmt.Println("qoder-bridge stopped (systemd user)")
		return
	}

	// Clean stale PID
	removePID()
	fmt.Println("qoder-bridge is not running")
	os.Exit(1)
}

func runRestart(args []string) {
	// Try systemd restart first (matches install.sh)
	for _, cmd := range [][]string{
		{"systemctl", "restart", "qoder-bridge"},
		{"systemctl", "--user", "restart", "qoder-bridge"},
	} {
		if err := exec.Command(cmd[0], cmd[1:]...).Run(); err == nil {
			time.Sleep(2 * time.Second)
			statusOut, _ := exec.Command(cmd[0], "is-active", "qoder-bridge").CombinedOutput()
			if strings.TrimSpace(string(statusOut)) == "active" {
				fmt.Printf("qoder-bridge restarted (systemd: %s)\n", cmd[0])
				return
			}
		}
	}

	// Fallback: PID-file daemon mode
	runStop(nil)
	time.Sleep(500 * time.Millisecond)
	runDaemonize(args)
}

func runStatus(args []string) {
	// Try PID file first (daemon mode)
	if pid, err := readPID(); err == nil && isRunning(pid) {
		fmt.Printf("qoder-bridge is running (PID %d)\n", pid)
		fmt.Printf("  logs:   tail -f %s\n", logFilePath())
		fmt.Printf("  stop:   qoder-bridge stop\n")
		return
	}

	// Try systemd (system)
	out, err := exec.Command("systemctl", "is-active", "qoder-bridge").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		fmt.Println("qoder-bridge is running (systemd)")
		fmt.Printf("  logs:   journalctl -u qoder-bridge -f\n")
		fmt.Printf("  stop:   qoder-bridge stop\n")
		return
	}

	// Try systemd (user)
	out, err = exec.Command("systemctl", "--user", "is-active", "qoder-bridge").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == "active" {
		fmt.Println("qoder-bridge is running (systemd user)")
		fmt.Printf("  logs:   journalctl --user -u qoder-bridge -f\n")
		fmt.Printf("  stop:   qoder-bridge stop\n")
		return
	}

	// Clean stale PID
	removePID()
	fmt.Println("qoder-bridge is not running")
}

func runUpdate(args []string) {
	// Find project directory
	projDir := ""
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), "projects", "qoder-bridge"),
		".",
	}
	for _, d := range candidates {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			projDir = d
			break
		}
	}
	if projDir == "" {
		fmt.Fprintf(os.Stderr, "error: cannot find qoder-bridge git repo\n")
		fmt.Fprintf(os.Stderr, "run from the project directory or clone to ~/projects/qoder-bridge\n")
		os.Exit(1)
	}

	fmt.Printf("updating from %s...\n", projDir)

	// git pull
	fmt.Print("  git pull... ")
	if err := runCmd(projDir, "git", "pull"); err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	// go build
	fmt.Print("  building... ")
	if err := runCmd(projDir, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", "qoder-bridge", "."); err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	// Copy binary
	exe := filepath.Join(projDir, "qoder-bridge")
	dest := filepath.Join(os.Getenv("HOME"), ".local", "bin", "qoder-bridge")

	// Stop running instance first (binary is busy while running)
	stopped := false
	if pid, err := readPID(); err == nil && isRunning(pid) {
		fmt.Printf("  stopping (PID %d)... ", pid)
		sendSignal(pid)
		time.Sleep(1 * time.Second)
		stopped = true
		fmt.Println("ok")
	} else if err := exec.Command("systemctl", "stop", "qoder-bridge").Run(); err == nil {
		stopped = true
		fmt.Println("  stopped (systemd)... ok")
	} else if err := exec.Command("systemctl", "--user", "stop", "qoder-bridge").Run(); err == nil {
		stopped = true
		fmt.Println("  stopped (systemd user)... ok")
	}

	if stopped {
		time.Sleep(2 * time.Second)
	}

	fmt.Printf("  installing to %s... ", dest)
	tmpDest := dest + ".tmp"
	if err := runCmd(projDir, "cp", exe, tmpDest); err != nil {
		os.Remove(tmpDest)
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmpDest, dest); err != nil {
		os.Remove(tmpDest)
		fmt.Printf("FAILED (rename): %v\n", err)
		os.Exit(1)
	}
	runCmd(projDir, "chmod", "+x", dest)
	fmt.Println("ok")

	// Restart
	if stopped {
		fmt.Print("  restarting... ")
		if err := exec.Command("systemctl", "start", "qoder-bridge").Run(); err != nil {
			if err := exec.Command("systemctl", "--user", "start", "qoder-bridge").Run(); err != nil {
				fmt.Printf("start manually: %s\n", dest)
			}
		}
		fmt.Println("ok")
	} else {
		fmt.Printf("  not running — start with: %s\n", dest)
	}

	fmt.Println("update complete!")
}

func runCmd(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printUsage() {
	fmt.Print(`qoder-bridge — OpenAI-compatible proxy for Qoder API

Usage:
  qoder-bridge                    Start as background daemon
  qoder-bridge run                Run in foreground (for systemd)
  qoder-bridge stop               Stop the daemon
  qoder-bridge restart            Stop and restart the daemon
  qoder-bridge status             Check if running
  qoder-bridge update             Pull, rebuild, restart
  qoder-bridge config             Interactive TUI config menu
  qoder-bridge quota              Check PAT quota
  qoder-bridge help               Show this help

Flags (for 'run' and default mode):
  -env string       Path to .env file (default: ./.env or ~/.env)
  -port int         Listen port (overrides QODER_PORT in .env)
  -pats string      Comma-separated PAT list (overrides .env)

Environment:
  QODER_PROXY       Proxy URL (socks5://, http://, https://)

Examples:
  qoder-bridge                      Start daemon on port 7100
  qoder-bridge run -port 8080       Run foreground on port 8080
  qoder-bridge quota                Check quota for all PATs
  qoder-bridge update               Update from git and restart
`)
}

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			runServe(os.Args[2:])
		case "stop":
			runStop(os.Args[2:])
		case "restart":
			runRestart(os.Args[2:])
		case "status":
			runStatus(os.Args[2:])
		case "update":
			runUpdate(os.Args[2:])
		case "quota":
			qfs := flag.NewFlagSet("quota", flag.ExitOnError)
			envFlag := qfs.String("env", "", "Path to .env file")
			qfs.Parse(os.Args[2:])
			runQuotaCLI(*envFlag)
		case "config":
			runConfigCLI(os.Args[2:])
		case "usage":
			runUsageCLI(os.Args[2:])
		case "logs":
			runLogsCLI(os.Args[2:])
		case "help", "-h", "--help":
			printUsage()
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
		return
	}

	// Default: daemonize
	runDaemonize(os.Args[1:])
}
