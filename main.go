// main.go — OpenAI-compatible HTTP server for Qoder API.
//
// Bypasses qodercli entirely using pure-Go COSY signing + WAF encoding.
// No Node.js, no WASM, no cold start. ~50ms auth + 2-5s LLM response.
//
// Usage:
//
//	qoder-bridge                          # serve on port 7100
//	qoder-bridge -port 8080               # custom port
//	qoder-bridge quota                    # check quota and exit
//	qoder-bridge quota -env /path/.env    # check quota with custom .env
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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// ── Config ──────────────────────────────────────────────────────────────────

var (
	port     int
	pats     []string
	patPool  *PATPool
	combos   map[string][]string // combo name → model list
	apiKey   string              // optional: sk-* API key for auth
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
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(p.pats))))
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

type ChatRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens,omitempty"`
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
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	models := []ModelEntry{}
	for _, t := range tierModels {
		models = append(models, ModelEntry{
			ID: "qd/" + t, Object: "model", Created: 1, OwnedBy: "qoder",
		})
	}
	keys := make([]string, 0, len(frontierModels))
	for k := range frontierModels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		models = append(models, ModelEntry{
			ID: "qd/" + k, Object: "model", Created: 1, OwnedBy: "qoder",
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
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": models})
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
		http.Error(w, fmt.Sprintf(`{"error":{"message":"bad request: %s"}}`, err), 400)
		return
	}

	if patPool.Len() == 0 {
		http.Error(w, `{"error":{"message":"no PATs configured"}}`, 503)
		return
	}

	modelInput := req.Model
	modelKey := resolveModelKey(modelInput)

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
	})

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
	result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s"}}`, err), 502)
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

		result, err := runWithPATRotation(r.Context(), patPool, modelKey, req.Messages, req.MaxTokens, nil)
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
	http.Error(w, fmt.Sprintf(`{"error":{"message":"combo %s: all models failed, last error: %s"}}`, comboName, lastErr), 502)
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
		})

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

func runWithPATRotation(ctx context.Context, pool *PATPool, modelKey string, messages []ChatMessage, maxTokens int, onChunk StreamCallback) (string, error) {
	pat := pool.Next()

	result, err := callQoder(ctx, pat, modelKey, messages, maxTokens, onChunk)
	if err != nil {
		log.Printf("qd %s error: %v (pat: %s)", modelKey, err, maskPAT(pat))
		if isAuthError(err) && pool.Len() > 1 {
			pat2 := pool.Next()
			log.Printf("pat rotation: %s → %s", maskPAT(pat), maskPAT(pat2))
			result, err = callQoder(ctx, pat2, modelKey, messages, maxTokens, onChunk)
		}
	}
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

func isAuthError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "expired") || strings.Contains(s, "unauthorized")
}

// ── Model & combo resolution ────────────────────────────────────────────────

// resolveModelKey strips prefixes and normalizes a model name.
// Accepts: "qd/auto", "QD/Auto", "qoder/auto", "auto", "apore/auto" → "auto"
func resolveModelKey(model string) string {
	// Lowercase everything
	m := strings.ToLower(strings.TrimSpace(model))

	// Strip any prefix up to and including "/"
	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		prefix := m[:idx]
		m = m[idx+1:]
		// Log if prefix wasn't qd or qoder
		if prefix != "qd" && prefix != "qoder" {
			log.Printf("model: prefix %q auto-converted to qd/ (using qd/%s)", prefix, m)
		}
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

	// Strip prefix
	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		m = m[idx+1:]
	}

	// Strip "combo-" prefix
	m = strings.TrimPrefix(m, "combo-")
	m = strings.TrimPrefix(m, "combo_")

	if models, ok := combos[m]; ok {
		return models, true
	}
	return nil, false
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

// ── .env loader ─────────────────────────────────────────────────────────────

type envConfig struct {
	pats     []string
	port     int
	strategy string
	combos   map[string][]string
	apiKey   string // optional: sk-* API key for auth
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

		// KEY=VALUE parsing
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			// Bare pt- line (legacy format)
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

		case key == "QODER_PROXY":
			if val != "" {
				os.Setenv("QODER_PROXY", val)
			}

		case strings.HasPrefix(line, "pt-"):
			cfg.pats = append(cfg.pats, line)
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

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	// Check for subcommand
	if len(os.Args) > 1 && os.Args[1] == "quota" {
		// Parse flags after "quota"
		qfs := flag.NewFlagSet("quota", flag.ExitOnError)
		envFlag := qfs.String("env", "", "Path to .env file")
		qfs.Parse(os.Args[2:])
		runQuotaCLI(*envFlag)
		return
	}

	// Serve mode
	envFlag := flag.String("env", "", "Path to .env file")
	portFlag := flag.Int("port", 0, "Listen port (overrides QODER_PORT in .env)")
	patsFlag := flag.String("pats", "", "Comma-separated PAT list (overrides .env)")
	flag.Parse()

	cfg := loadEnv(*envFlag)

	// Initialize proxy-aware HTTP client after .env is loaded
	initProxyClient()

	// Override port from flag
	if *portFlag > 0 {
		cfg.port = *portFlag
	}

	// Override PATs from flag
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
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  qoder-bridge -pats \"pt-xxx,pt-yyy\"")
		fmt.Fprintln(os.Stderr, "  qoder-bridge -env /path/to/.env")
		fmt.Fprintln(os.Stderr, "  QODER_PATS=pt-xxx,pt-yyy qoder-bridge")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, ".env format:")
		fmt.Fprintln(os.Stderr, "  pt-your-first-pat-here")
		fmt.Fprintln(os.Stderr, "  pt-your-second-pat-here")
		fmt.Fprintln(os.Stderr, "  QODER_PORT=7100")
		fmt.Fprintln(os.Stderr, "  PAT_STRATEGY=round-robin")
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
			log.Printf("  ok: %s → user %s", maskPAT(pat), uid)
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
			log.Printf("  qd/combo-%s: %s", name, strings.Join(combos[name], " → "))
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
	log.Printf("  ✓ ready on %s", addr)
	log.Printf("    chat:    http://%s/v1/chat/completions", addr)
	log.Printf("    models:  http://%s/v1/models", addr)
	log.Printf("    quota:   http://%s/v1/quota", addr)
	log.Printf("    combos:  http://%s/v1/combos", addr)
	log.Printf("    health:  http://%s/health", addr)
	log.Printf("  ✓ engine:  pure Go COSY (no qodercli)")
	log.Printf("  ✓ proxy:   %s", getProxyInfo())
	if apiKey != "" {
		log.Printf("  ✓ apikey:  enabled (sk-*****)")
	} else {
		log.Printf("  ⚠ apikey:  disabled (no QODER_API_KEY in .env)")
	}
	log.Printf("")
	log.Printf("ready to accept connections.")

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
