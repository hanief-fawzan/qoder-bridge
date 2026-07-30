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
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	apiKey       string              // optional: sk-* API key for auth
	requestDelay int                 // max random delay in ms (0 = disabled)
)

// ── Models ──────────────────────────────────────────────────────────────────

// Known Qoder tier models (auto-routed by Qoder infrastructure).
var tierModels = []string{"auto", "ultimate", "performance", "efficient", "lite"}

// Known frontier models (mapped to real model names).
var frontierModels = map[string]string{
	"qmodel_preview": "Qwen3.8-Max-Preview",
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
	mu       sync.Mutex
	pats     []string
	idx      int
	strategy string // "round-robin" or "random"
}

func NewPATPool(pats []string, strategy string) *PATPool {
	if strategy != "random" {
		strategy = "round-robin"
	}
	return &PATPool{pats: pats, strategy: strategy}
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

func (p *PATPool) Len() int { return len(p.pats) }

// ── OpenAI types ────────────────────────────────────────────────────────────

type ChatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
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
	Tools          []ToolDef     `json:"tools,omitempty"`
	ToolChoice     interface{}   `json:"tool_choice,omitempty"`
	ThinkingEffort string        `json:"thinking_effort,omitempty"` // low, medium, high, xhigh
	ContextWindow  int           `json:"context_window,omitempty"`  // 200000, 400000, 1000000
	ReasoningEffort string       `json:"reasoning_effort,omitempty"` // Hermes sends this (OpenAI standard)
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
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
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

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, `{"error":{"message":"POST required"}}`, 405)
		return
	}

	// Auth check — if apiKey is set, require Bearer token
	if apiKey != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+apiKey {
			http.Error(w, `{"error":{"message":"invalid API key"}}`, 401)
			return
		}
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, 400, fmt.Sprintf("bad request: %s", err))
		return
	}

	if patPool.Len() == 0 {
		http.Error(w, `{"error":{"message":"no PATs configured"}}`, 503)
		return
	}

	modelInput := req.Model
	modelKey := resolveModelKey(modelInput)

	// Resolve thinking effort (Hermes sends reasoning_effort, Qoder expects thinking_effort)
	req.ThinkingEffort = resolveThinkingEffort(req)
	// Resolve context window (auto-set based on model tier if not specified)
	req.ContextWindow = resolveContextWindow(req, modelKey)

	// Check if this is a combo request
	if comboModels, isCombo := resolveCombo(modelInput); isCombo {
		if req.Stream {
			handleComboStream(w, r, req, modelInput, comboModels)
		} else {
			handleComboNonStream(w, r, req, modelInput, comboModels)
		}
		return
	}

	if req.Stream {
		handleStream(w, r, req, modelKey)
	} else {
		handleNonStream(w, r, req, modelKey)
	}
}

func handleStream(w http.ResponseWriter, r *http.Request, req ChatRequest, modelKey string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()
	first := true

	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, func(text string) {
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
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
			})
		}
	}, req.Tools, req.ThinkingEffort, req.ContextWindow)

	if err != nil {
		log.Printf("stream error: %v", err)
		errMsg := "\n\n[Error: " + err.Error() + "]"
		if flusher != nil {
			sendSSE(w, flusher, SSEChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(errMsg) + `}`)}},
			})
		}
	}
	_ = result

	if flusher != nil {
		stop := "stop"
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &stop}},
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func handleNonStream(w http.ResponseWriter, r *http.Request, req ChatRequest, modelKey string) {
	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow)
	if err != nil {
		forwardUpstreamError(w, err)
		return
	}

	resp := ChatResponse{
		ID:      "chatcmpl-" + uuidString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: result}, FinishReason: "stop"}},
		Usage:   Usage{},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ── Combo handlers ──────────────────────────────────────────────────────────

func handleComboNonStream(w http.ResponseWriter, r *http.Request, req ChatRequest, comboName string, modelList []string) {
	var lastErr error
	for _, model := range modelList {
		modelKey := resolveModelKey(model)
		if !knownModelKeys[modelKey] {
			log.Printf("combo %q: model %q not in known list, trying anyway", comboName, model)
		}
		log.Printf("combo %q: trying qd/%s", comboName, modelKey)

		result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil, req.Tools, req.ThinkingEffort, req.ContextWindow)
		if err == nil {
			resp := ChatResponse{
				ID:      "chatcmpl-" + uuidString(),
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   comboName,
				Choices: []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: result}, FinishReason: "stop"}},
				Usage:   Usage{},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		lastErr = err
		log.Printf("combo %q: qd/%s failed: %v", comboName, modelKey, err)
	}
	forwardUpstreamError(w, fmt.Errorf("combo %s: all models failed, last: %w", comboName, lastErr))
}

func handleComboStream(w http.ResponseWriter, r *http.Request, req ChatRequest, comboName string, modelList []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + uuidString()
	created := time.Now().Unix()

	var lastErr error
	for _, model := range modelList {
		modelKey := resolveModelKey(model)
		if !knownModelKeys[modelKey] {
			log.Printf("combo %q: model %q not in known list, trying anyway", comboName, model)
		}
		log.Printf("combo %q: trying qd/%s", comboName, modelKey)

		first := true
		result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, func(text string) {
			if first {
				if flusher != nil {
					sendSSE(w, flusher, SSEChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
						Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"role":"assistant","content":""}`)}},
					})
				}
				first = false
			}
			if flusher != nil {
				sendSSE(w, flusher, SSEChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
					Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(text) + `}`)}},
				})
			}
		}, req.Tools, req.ThinkingEffort, req.ContextWindow)

		if err == nil {
			_ = result
			if flusher != nil {
				stop := "stop"
				sendSSE(w, flusher, SSEChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
					Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &stop}},
				})
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
			}
			return
		}
		lastErr = err
		log.Printf("combo %q: qd/%s failed: %v", comboName, modelKey, err)
	}

	// All models failed
	if flusher != nil {
		errMsg := fmt.Sprintf("\n\n[Error: combo %s: all models failed, last: %s]", comboName, lastErr)
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{"content":` + mustQuote(errMsg) + `}`)}},
		})
		stop := "stop"
		sendSSE(w, flusher, SSEChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: comboName,
			Choices: []SSEChoice{{Index: 0, Delta: json.RawMessage(`{}`), FinishReason: &stop}},
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// ── PAT rotation ────────────────────────────────────────────────────────────

func runWithPATRotation(ctx context.Context, pool *PATPool, modelKey string, messages []ChatMessage, maxTokens int, onChunk StreamCallback, tools []ToolDef, thinkingEffort string, contextWindow int) (string, error) {
	start := time.Now()

	// Smart anti-ban delay: random jitter between 0 and requestDelay ms
	if requestDelay > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(requestDelay)))
		if err == nil {
			time.Sleep(time.Duration(n.Int64()) * time.Millisecond)
		}
	}

	// Try up to pool.Len() PATs on auth/queue errors (full rotation)
	maxAttempts := pool.Len()
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var result *ChatResult
	var lastErr error
	var pat string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		pat = pool.Next()
		result, lastErr = callQoder(ctx, pat, modelKey, messages, maxTokens, onChunk, tools, thinkingEffort, contextWindow)
		if lastErr == nil {
			break
		}
		log.Printf("qd %s error: %v (pat: %s, attempt %d/%d)", modelKey, lastErr, maskPAT(pat), attempt+1, maxAttempts)
		if (isAuthError(lastErr) || isQueueError(lastErr)) && attempt+1 < maxAttempts {
			log.Printf("pat rotation: retrying with next PAT...")
			continue
		}
		break // non-retryable error or last attempt
	}

	// Log to DB
	latency := time.Since(start).Milliseconds()
	promptTokens := 0
	for _, m := range messages {
		promptTokens += estimateTokens(extractText(m.Content))
	}
	completionTokens := 0
	if lastErr == nil && result != nil {
		completionTokens = estimateTokens(result.Text)
	}
	totalTokens := promptTokens + completionTokens
	credits := estimateCredits(modelKey, totalTokens)

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
		ClientIP:         "",
	})

	if lastErr != nil {
		return "", lastErr
	}
	return result.Text, nil
}

// upstreamStatusCode extracts HTTP status from an UpstreamError or guesses from error text.
func upstreamStatusCode(err error) int {
	if ue, ok := err.(*UpstreamError); ok {
		return ue.StatusCode
	}
	return 502
}

func isAuthError(err error) bool {
	if ue, ok := err.(*UpstreamError); ok {
		return ue.StatusCode == 401 || ue.StatusCode == 403
	}
	s := err.Error()
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "expired") || strings.Contains(s, "unauthorized")
}

// isQueueError returns true if the error is a Qoder queue/rate-limit error (403 with isQueued=true).
func isQueueError(err error) bool {
	if ue, ok := err.(*UpstreamError); ok {
		return ue.StatusCode == 403 && strings.Contains(ue.Body, "isQueued")
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

var maskRe = regexp.MustCompile(`^(.{6}).*(.{4})$`)

func maskPAT(pat string) string {
	if len(pat) > 14 {
		return maskRe.ReplaceAllString(pat, "$1...$2")
	}
	return "***"
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

func sendJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"message": msg}})
}

// forwardUpstreamError forwards the raw Qoder API error to the client,
// preserving the upstream status code and body for debugging.
func forwardUpstreamError(w http.ResponseWriter, err error) {
	if ue, ok := err.(*UpstreamError); ok {
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
	apiKey       string // optional: sk-* API key for auth
	requestDelay int    // max random delay in ms between requests (0 = disabled)
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

		case key == "QODER_API_KEY":
			if val != "" {
				cfg.apiKey = val
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
	if cfg.apiKey != "" {
		apiKey = cfg.apiKey
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
		if v := cfgGet("api_key"); v != "" {
			apiKey = v
		}
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
	mux.HandleFunc("/v1/combos", handleCombos)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("")
	log.Printf("qoder-bridge (cosy-pure-go)")
	log.Printf("  ready on %s", addr)
	log.Printf("    chat:    http://%s/v1/chat/completions", addr)
	log.Printf("    models:  http://%s/v1/models", addr)
	log.Printf("    quota:   http://%s/v1/quota", addr)
	log.Printf("    combos:  http://%s/v1/combos", addr)
	log.Printf("    health:  http://%s/health", addr)
	log.Printf("  engine:  pure Go COSY (no qodercli)")
	log.Printf("  proxy:   %s", getProxyInfo())
	if apiKey != "" {
		log.Printf("  apikey:  enabled (sk-*****)")
	} else {
		log.Printf("  apikey:  disabled (no QODER_API_KEY in .env)")
	}
	log.Printf("")
	log.Printf("ready to accept connections.")

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		removePID()
		os.Exit(0)
	}()

	if err := http.ListenAndServe(addr, mux); err != nil {
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
	if err := runCmd(projDir, "go", "build", "-o", "qoder-bridge", "."); err != nil {
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
