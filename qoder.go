// qoder.go — Pure-Go Qoder API client with COSY signing.
//
// Handles: PAT exchange → job token, userId resolution, model config fetch,
// chat completions with full Qoder request format + WAF encoding.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	qoderChatURL       = "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderModelListURL  = "https://api3.qoder.sh/algo/api/v2/model/list"
	qoderJobTokenURL   = "https://openapi.qoder.sh/api/v1/jobToken/exchange"
	qoderUserInfoURL   = "https://openapi.qoder.sh/api/v1/userinfo"
	qoderQuotaUsageURL = "https://openapi.qoder.sh/api/v2/quota/usage"

	patRefreshBuffer = 5 * time.Minute
	jobTokenExpiry   = 24 * time.Hour
)

// ── Credential cache ────────────────────────────────────────────────────────

type patCredential struct {
	pat         string // original PAT (pt-...)
	accessToken string // job token (jt-...)
	userID      string
	machineID   string
	expiresAt   time.Time
}

var (
	credCache sync.Map // pat string -> *patCredential
)

func getCredential(pat string) (*patCredential, error) {
	if v, ok := credCache.Load(pat); ok {
		c := v.(*patCredential)
		if time.Until(c.expiresAt) > patRefreshBuffer {
			return c, nil
		}
	}
	c, err := resolveCredential(pat)
	if err != nil {
		return nil, err
	}
	credCache.Store(pat, c)
	return c, nil
}

func resolveCredential(pat string) (*patCredential, error) {
	// 1. Exchange PAT → job token
	jt, expiresAt, err := exchangeJobToken(pat)
	if err != nil {
		return nil, fmt.Errorf("PAT exchange: %w", err)
	}

	// 2. Fetch userId from userinfo
	userID, err := fetchUserID(jt)
	if err != nil {
		log.Printf("warn: fetch userId failed (%v), using fallback", err)
		userID = "user-" + uuidString()[:8]
	}

	return &patCredential{
		pat:         pat,
		accessToken: jt,
		userID:      userID,
		machineID:   uuidString(),
		expiresAt:   expiresAt,
	}, nil
}

func exchangeJobToken(pat string) (string, time.Time, error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, _ := http.NewRequest("POST", qoderJobTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")
	req.Header.Set("Cosy-Version", cosyVersion)
	req.Header.Set("Cosy-ClientType", clientType)

	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	resp, err := proxyClientFn().Do(req.WithContext(ctx))
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", time.Time{}, fmt.Errorf("%d: %s", resp.StatusCode, string(b))
	}

	var data struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", time.Time{}, err
	}
	if data.Token == "" {
		return "", time.Time{}, fmt.Errorf("no token in response")
	}

	expiresAt := time.Now().Add(jobTokenExpiry)
	if data.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, data.ExpiresAt); err == nil {
			expiresAt = t
		}
	} else if data.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second)
	}

	return data.Token, expiresAt, nil
}

func fetchUserID(jobToken string) (string, error) {
	req, _ := http.NewRequest("GET", qoderUserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+jobToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")

	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	resp, err := proxyClientFn().Do(req.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo %d: %s", resp.StatusCode, string(b))
	}

	var info struct {
		ID     string `json:"id"`
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.ID != "" {
		return info.ID, nil
	}
	return info.UserID, nil
}

// ── Model config cache ──────────────────────────────────────────────────────

type modelConfig struct {
	Key             string `json:"key"`
	IsReasoning     bool   `json:"is_reasoning"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Source          string `json:"source"`
	// Other fields from API — store raw for forwarding
	Raw json.RawMessage `json:"-"`
}

var (
	modelConfigCache     map[string]*modelConfig
	modelConfigCacheMu   sync.RWMutex
	modelConfigCacheTime time.Time
)

func getModelConfig(cred *patCredential, modelKey string) (*modelConfig, error) {
	modelConfigCacheMu.RLock()
	if modelConfigCache != nil && time.Since(modelConfigCacheTime) < 10*time.Minute {
		mc, ok := modelConfigCache[modelKey]
		modelConfigCacheMu.RUnlock()
		if ok {
			return mc, nil
		}
		// Force refresh if model not found
	} else {
		modelConfigCacheMu.RUnlock()
	}

	return fetchModelConfig(cred, modelKey, true)
}

func fetchModelConfig(cred *patCredential, modelKey string, retry bool) (*modelConfig, error) {
	// Model list is a GET request with COSY-signed empty body
	req, _ := http.NewRequest("GET", qoderModelListURL, nil)

	// Sign the request with COSY (empty body)
	cosy, err := BuildCosyHeaders([]byte{}, qoderModelListURL, CosyCreds{
		UserID:    cred.userID,
		AuthToken: cred.accessToken,
		MachineID: cred.machineID,
	})
	if err != nil {
		return nil, fmt.Errorf("cosy sign model list: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", cosy.Authorization)
	for k, v := range cosy.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")

	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	resp, err := proxyClientFn().Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		if retry && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			credCache.Delete(cred.pat)
		}
		return nil, fmt.Errorf("model list %d: %s", resp.StatusCode, string(b))
	}

	// Parse response — format is {"chat": [{"key":"auto","display_name":"Auto",...}, ...]}
	rawResp, _ := io.ReadAll(resp.Body)

	// Try envelope format first
	var envelopeResp struct {
		StatusCode int    `json:"statusCodeValue"`
		Body       string `json:"body"`
	}
	bodyToParse := rawResp
	if json.Unmarshal(rawResp, &envelopeResp) == nil && envelopeResp.Body != "" {
		bodyToParse = []byte(envelopeResp.Body)
	}

	var listResp struct {
		Chat []struct {
			Key             string          `json:"key"`
			DisplayName     string          `json:"display_name"`
			Format          string          `json:"format"`
			Source          string          `json:"source"`
			IsReasoning     bool            `json:"is_reasoning"`
			IsDefault       bool            `json:"is_default"`
			MaxInputTokens  int             `json:"max_input_tokens"`
			PriceFactor     float64         `json:"price_factor"`
			Raw             json.RawMessage `json:"-"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(bodyToParse, &listResp); err != nil {
		return nil, fmt.Errorf("parse model list: %w (body: %s)", err, truncate(string(bodyToParse), 200))
	}

	var rawChatModels []json.RawMessage
	// Extract raw per-model configs from the parsed array
	if err := json.Unmarshal(bodyToParse, &rawChatModels); err != nil {
		// Try nested: {"chat": [...]}
		var tmp struct {
			Chat []json.RawMessage `json:"chat"`
		}
		json.Unmarshal(bodyToParse, &tmp)
		rawChatModels = tmp.Chat
	}

	newCache := make(map[string]*modelConfig)
	for i, m := range listResp.Chat {
		mc := &modelConfig{
			Key:         m.Key,
			IsReasoning: m.IsReasoning,
			Source:      m.Source,
		}
		if i < len(rawChatModels) {
			mc.Raw = rawChatModels[i]
		}
		newCache[m.Key] = mc
	}

	modelConfigCacheMu.Lock()
	modelConfigCache = newCache
	modelConfigCacheTime = time.Now()
	modelConfigCacheMu.Unlock()

	if mc, ok := newCache[modelKey]; ok {
		return mc, nil
	}

	return nil, fmt.Errorf("model %q not found in model list (%d models)", modelKey, len(newCache))
}

// ── Quota ───────────────────────────────────────────────────────────────────

type QuotaInfo struct {
	PAT       string `json:"pat"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	Limit     int64  `json:"limit"`
	ResetDate string `json:"reset_date,omitempty"`
	Error     string `json:"error,omitempty"`
}

func fetchQuota(pat string) QuotaInfo {
	cred, err := getCredential(pat)
	if err != nil {
		return QuotaInfo{PAT: maskPAT(pat), Error: err.Error()}
	}

	req, _ := http.NewRequest("GET", qoderQuotaUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+cred.accessToken)
	req.Header.Set("Accept", "application/json")

	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	resp, err := proxyClientFn().Do(req.WithContext(ctx))
	if err != nil {
		return QuotaInfo{PAT: maskPAT(pat), Error: err.Error()}
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return QuotaInfo{PAT: maskPAT(pat), Error: fmt.Sprintf("%d: %s", resp.StatusCode, string(b))}
	}

	var q struct {
		Used      int64  `json:"used"`
		Remaining int64  `json:"remaining"`
		Limit     int64  `json:"limit"`
		ResetDate string `json:"reset_date"`
	}
	json.Unmarshal(b, &q)
	return QuotaInfo{
		PAT:       maskPAT(pat),
		Used:      q.Used,
		Remaining: q.Remaining,
		Limit:     q.Limit,
		ResetDate: q.ResetDate,
	}
}

// ── Chat completions ────────────────────────────────────────────────────────

// normalizeMessages hoists system messages out of the array and flattens
// multipart content. Qoder rejects system messages inside the messages array.
func normalizeMessages(messages []ChatMessage, tools []ToolDef) ([]ChatMessage, string) {
	var systemParts []string
	var out []ChatMessage
	for _, m := range messages {
		text := extractText(m.Content)
		switch m.Role {
		case "system":
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "tool":
			// Convert tool result to readable user message
			toolName := "unknown"
			if text != "" {
				out = append(out, ChatMessage{
					Role:    "user",
					Content: "[Tool Result]\n" + text,
				})
			}
			_ = toolName
			continue
		case "assistant":
			if text == "" {
				// Empty assistant message (tool_calls-only) — skip
				continue
			}
		}
		out = append(out, ChatMessage{Role: m.Role, Content: text})
	}

	// Inject tool definitions into system prompt
	if len(tools) > 0 {
		var toolParts []string
		toolParts = append(toolParts, "You have access to the following tools. To call a tool, respond with a JSON block:")
		toolParts = append(toolParts, "```tool_call\n{\"name\": \"tool_name\", \"arguments\": {...}}\n```")
		toolParts = append(toolParts, "")
		for _, t := range tools {
			desc := t.Function.Description
			if desc == "" {
				desc = "(no description)"
			}
			toolParts = append(toolParts, fmt.Sprintf("- %s: %s", t.Function.Name, desc))
		}
		systemParts = append([]string{strings.Join(toolParts, "\n")}, systemParts...)
	}

	return out, strings.Join(systemParts, "\n\n")
}

func extractText(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case nil:
		return ""
	case []interface{}:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == "text" {
					if t, _ := m["text"].(string); t != "" {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", c)
	}
}

func lastUserText(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			if s, ok := messages[i].Content.(string); ok {
				return s
			}
		}
	}
	return ""
}

func stableHash(prefix string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func stableChatRecordID(model string, messages []ChatMessage, maxTokens int) string {
	h := sha256.New()
	h.Write([]byte("qoder-record\x00"))
	h.Write([]byte(model))
	for _, m := range messages {
		if m.Role != "" {
			h.Write([]byte{0})
			h.Write([]byte(m.Role))
		}
		if s, ok := m.Content.(string); ok && s != "" {
			h.Write([]byte{0})
			h.Write([]byte(s))
		}
	}
	h.Write([]byte(fmt.Sprintf("\x00mt=%d", maxTokens)))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// buildQoderRequestBody creates the exact JSON payload Qoder expects.
func buildQoderRequestBody(modelKey string, messages []ChatMessage, maxTokens int, creds *patCredential, tools []ToolDef) ([]byte, error) {
	mc, err := getModelConfig(creds, modelKey)
	if err != nil {
		return nil, fmt.Errorf("model config: %w", err)
	}

	normalized, systemText := normalizeMessages(messages, tools)
	lastUser := lastUserText(messages)
	sessionID := stableHash("qoder-session", creds.userID, modelKey)
	recordID := stableChatRecordID(modelKey, messages, maxTokens)

	if maxTokens <= 0 {
		maxTokens = 32768
	}
	if mc.MaxOutputTokens > 0 && mc.MaxOutputTokens < maxTokens {
		maxTokens = mc.MaxOutputTokens
	}

	payload := map[string]interface{}{
		"request_id":      uuidString(),
		"request_set_id":  recordID,
		"chat_record_id":  recordID,
		"session_id":      sessionID,
		"stream":          true,
		"chat_task":       "FREE_INPUT",
		"is_reply":        true,
		"is_retry":        false,
		"source":          1,
		"version":         "3",
		"session_type":    "qodercli",
		"agent_id":        "agent_common",
		"task_id":         "common",
		"code_language":   "",
		"chat_prompt":     "",
		"image_urls":      nil,
		"aliyun_user_type": "",
		"system":          systemText,
		"messages":        normalized,
		"tools":           []interface{}{},
		"parameters":      map[string]interface{}{"max_tokens": maxTokens},
		"chat_context": map[string]interface{}{
			"chatPrompt":  "",
			"imageUrls":   nil,
			"extra": map[string]interface{}{
				"context":      []interface{}{},
				"modelConfig":  map[string]interface{}{"key": modelKey, "is_reasoning": mc.IsReasoning},
				"originalContent": lastUser,
			},
			"features": []interface{}{},
			"text":     lastUser,
		},
		"model_config": mc.Raw,
		"business": map[string]interface{}{
			"product":  "cli",
			"version":  "1.0.0",
			"type":     "agent",
			"stage":    "start",
			"id":       uuidString(),
			"name":     truncate(lastUser, 30),
			"begin_at": time.Now().UnixMilli(),
		},
	}

	return json.Marshal(payload)
}

// ChatResult holds the response from a Qoder chat call.
type ChatResult struct {
	Text      string
	Error     error
	RequestID string
}

// StreamCallback is called for each text chunk during streaming.
type StreamCallback func(text string)

// callQoder sends a chat completion request directly to Qoder's API with
// full COSY signing and WAF encoding. No qodercli required.
func callQoder(ctx context.Context, pat, modelKey string, messages []ChatMessage, maxTokens int, onChunk StreamCallback, tools []ToolDef) (*ChatResult, error) {
	// 1. Resolve credential (PAT → job token + userId)
	cred, err := getCredential(pat)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}

	// 2. Build request body (Qoder format)
	payload, err := buildQoderRequestBody(modelKey, messages, maxTokens, cred, tools)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// 3. WAF-encode the body
	encodedBody := qoderEncodeBody(payload)

	// 4. Sign the ENCODED body with COSY
	cosy, err := BuildCosyHeaders(encodedBody, qoderChatURL, CosyCreds{
		UserID:    cred.userID,
		AuthToken: cred.accessToken,
		MachineID: cred.machineID,
	})
	if err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}

	// 5. Send request
	req, err := http.NewRequestWithContext(ctx, "POST", qoderChatURL, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Model-Key", modelKey)
	if mc, _ := getModelConfig(cred, modelKey); mc != nil {
		req.Header.Set("X-Model-Source", mc.Source)
	}
	req.Header.Set("Authorization", cosy.Authorization)
	for k, v := range cosy.Headers {
		req.Header.Set(k, v)
	}

	resp, err := proxyClientFn().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		credCache.Delete(pat)
		return nil, fmt.Errorf("qoder %d: %s", resp.StatusCode, string(b))
	}

	// 6. Parse SSE with Qoder envelope unwrapping
	text, err := unwrapQoderSSE(resp.Body, onChunk)
	if err != nil {
		return nil, fmt.Errorf("parse SSE: %w", err)
	}

	return &ChatResult{Text: text}, nil
}

// unwrapQoderSSE reads Qoder's SSE stream which wraps responses in a
// {statusCodeValue, body} envelope and returns the concatenated text.
func unwrapQoderSSE(body io.Reader, onChunk StreamCallback) (string, error) {
	var full strings.Builder
	scanner := bufio.NewScanner(body)
	// ponytail: 64KB buffer for large chunks
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" || data == "" {
			break
		}

		// Parse Qoder envelope: {"statusCodeValue":200,"body":"..."}
		var envelope struct {
			StatusCode int    `json:"statusCodeValue"`
			Body       string `json:"body"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			continue
		}

		if envelope.StatusCode != 200 && envelope.StatusCode != 0 {
			return full.String(), fmt.Errorf("qoder stream error %d: %s", envelope.StatusCode, truncate(envelope.Body, 200))
		}

		inner := envelope.Body
		if inner == "" || inner == "[DONE]" {
			break
		}

		// Parse inner OpenAI-style chunk
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(inner), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			text := ch.Delta.Content
			if text == "" {
				text = ch.Message.Content
			}
			if text != "" {
				full.WriteString(text)
				if onChunk != nil {
					onChunk(text)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return full.String(), err
	}

	return full.String(), nil
}
