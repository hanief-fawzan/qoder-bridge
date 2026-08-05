// qoder.go — Pure-Go Qoder API client with COSY signing.
//
// Handles: PAT exchange → job token, userId resolution, model config fetch,
// chat completions with full Qoder request format + WAF encoding.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	qoderChatURL       = "https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderModelListURL  = "https://api2.qoder.sh/algo/api/v2/model/list"
	qoderJobTokenURL   = "https://openapi.qoder.sh/api/v1/jobToken/exchange"
	qoderUserInfoURL   = "https://openapi.qoder.sh/api/v1/userinfo"
	qoderQuotaUsageURL = "https://openapi.qoder.sh/api/v2/quota/usage"

	patRefreshBuffer = 5 * time.Minute
	jobTokenExpiry   = 24 * time.Hour
)

// ── Credential cache ────────────────────────────────────────────────────────

// UpstreamError carries the raw Qoder API error for forwarding to clients.
type UpstreamError struct {
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("qoder %d: %s", e.StatusCode, e.Body)
}

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
		// Model not in cache — but cache is fresh, so force refresh
	} else {
		modelConfigCacheMu.RUnlock()
	}

	return fetchModelConfig(cred, modelKey, true)
}

// getCachedModelConfig returns the model config from cache without fetching.
// Returns nil if cache is stale or model not found.
func getCachedModelConfig(modelKey string) *modelConfig {
	modelConfigCacheMu.RLock()
	defer modelConfigCacheMu.RUnlock()
	if modelConfigCache == nil || time.Since(modelConfigCacheTime) >= 10*time.Minute {
		return nil
	}
	return modelConfigCache[modelKey]
}

// ensureModelConfig returns cached config or fetches it. If fetch fails,
// returns a minimal fallback config so requests don't hard-fail.
func ensureModelConfig(cred *patCredential, modelKey string) *modelConfig {
	if mc := getCachedModelConfig(modelKey); mc != nil {
		return mc
	}
	mc, err := getModelConfig(cred, modelKey)
	if err != nil {
		log.Printf("warn: model config for %q not available: %v — using fallback", modelKey, err)
		return &modelConfig{
			Key:             modelKey,
			IsReasoning:     false,
			MaxOutputTokens: 65536,
			Source:          "",
			Raw:             nil,
		}
	}
	return mc
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
// Preserves assistant tool_calls and tool results with tool_call_id.
func normalizeMessages(messages []ChatMessage, tools []ToolDef) ([]ChatMessage, string) {
	var systemParts []string
	var out []ChatMessage
	for _, m := range messages {
		text := extractText(m.Content)
		switch m.Role {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "tool":
			// Preserve tool result with ID for context continuity
			callID := ""
			if tcID, ok := m.Extra["tool_call_id"].(string); ok {
				callID = tcID
			}
			if text != "" {
				if callID != "" {
					out = append(out, ChatMessage{Role: "user", Content: fmt.Sprintf("[Tool Result for call %s]\n%s", callID, text)})
				} else {
					out = append(out, ChatMessage{Role: "user", Content: "[Tool Result]\n" + text})
				}
			}
			continue
		case "assistant":
			// Serialize assistant message + any tool_calls for context continuity
			// Format matches the tool protocol prompt: ```json {"tool_calls": [...]}
			parts := []string{}
			if text != "" {
				parts = append(parts, text)
			}
			if tcRaw, ok := m.Extra["tool_calls"]; ok {
				if tcArr, ok := tcRaw.([]interface{}); ok && len(tcArr) > 0 {
					var normalized []map[string]interface{}
					for _, tc := range tcArr {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							name := "unknown"
							args := "{}"
							if fn, ok := tcMap["function"].(map[string]interface{}); ok {
								if n, ok := fn["name"].(string); ok {
									name = n
								}
								switch a := fn["arguments"].(type) {
								case string:
									if a != "" {
										args = a
									}
								case map[string]interface{}:
									if b, err := json.Marshal(a); err == nil {
										args = string(b)
									}
								default:
									if b, err := json.Marshal(a); err == nil {
										args = string(b)
									}
								}
							}
							normalized = append(normalized, map[string]interface{}{
								"name":      name,
								"arguments": json.RawMessage(args),
							})
						}
					}
					if len(normalized) > 0 {
						tcJSON, _ := json.Marshal(map[string]interface{}{"tool_calls": normalized})
						parts = append(parts, "```json\n"+string(tcJSON)+"\n```")
					}
				}
			}
			if len(parts) > 0 {
				out = append(out, ChatMessage{Role: "assistant", Content: strings.Join(parts, "\n")})
			}
			continue
		}
		out = append(out, ChatMessage{Role: m.Role, Content: text})
	}

	// Inject tool definitions into system prompt (matching qoder-proxy format)
	if len(tools) > 0 {
		var toolDescriptions []map[string]interface{}
		for _, t := range tools {
			desc := map[string]interface{}{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			}
			toolDescriptions = append(toolDescriptions, desc)
		}
		toolJSON, _ := json.MarshalIndent(toolDescriptions, "", "  ")
		fence := "```"
		toolPrompt := "[CRITICAL: Tool Calling Protocol]\n\n" +
			"You have access to these tools:\n" + string(toolJSON) + "\n\n" +
			"TOOL CALLING RULES — FOLLOW EXACTLY:\n" +
			"1. When you need to use a tool, you MUST output a JSON code block:\n\n" +
			fence + "json\n" +
			"{\"tool_calls\": [{\"name\": \"tool_name\", \"arguments\": {\"param1\": \"value1\"}}]}\n" +
			fence + "\n\n" +
			"2. You may include reasoning text BEFORE the JSON block, but the JSON block MUST be the LAST thing you output.\n" +
			"3. You MUST use the EXACT tool names listed above. NEVER rename or substitute tool names.\n" +
			"4. You MUST provide ALL required parameters with real values.\n" +
			"5. For multiple tool calls, put them all in one " + fence + "json" + fence + " block.\n" +
			"6. NEVER describe what you would do — just call the tool directly.\n" +
			"7. NEVER say you don't have access to tools — you ALWAYS have access to the tools listed above.\n" +
			"8. If no tool is needed, respond with normal text.\n\n" +
			"EXAMPLE — Reading a file:\n" +
			fence + "json\n" +
			"{\"tool_calls\": [{\"name\": \"read_file\", \"arguments\": {\"path\": \"/etc/hostname\"}}]}\n" +
			fence + "\n\n" +
			"EXAMPLE — Running a command:\n" +
			fence + "json\n" +
			"{\"tool_calls\": [{\"name\": \"terminal\", \"arguments\": {\"command\": \"ls -la\"}}]}\n" +
			fence
		systemParts = append([]string{toolPrompt}, systemParts...)

	// Append compact tool reminder to last user message.
	// Models that skim system prompt but read user messages will see this.
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "user" {
			toolList := ""
			for _, t := range tools {
				if toolList != "" {
					toolList += ", "
				}
				toolList += t.Function.Name
			}
			out[i].Content = extractText(out[i].Content) +
				"\n\n[Available tools: " + toolList + "] " +
				"Respond with a ```json code block containing {\"tool_calls\": [{\"name\": \"...\", \"arguments\": {...}}]}. " +
				"NEVER say you don't have tools."
			break
		}
	}
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

// Regex for ```json ... ``` blocks — compiled once at package level.
// Supports: ```, ````, `````+ backticks. Also handles json, tool_call, function_call tags.
var jsonBlockRe = regexp.MustCompile("(?s)" + "`{3,}" + `(?:json|tool_call|function_call)?\s*\n?([\s\S]*?)\n?\s*` + "`{3,}")

// Regex for <tool_call>...</tool_call> XML-style tool calls.
// Model may emit: <tool_call>\n{"name":"terminal","arguments":{"command":"pwd"}}\n</tool_call>
var xmlToolCallRe = regexp.MustCompile(`(?s)\x3ctool_call\x3e\s*\n?([\s\S]*?)\n?\s*\x3c/tool_call\x3e`)

// Regex for [assistant called tool: NAME with arguments: ...] text format.
// Captures the tool name; arguments are extracted separately via balanced
// bracket counting to handle nested arrays/objects.
var inlineToolCallRe = regexp.MustCompile(`\[assistant called tool: (\S+) with arguments: `)

// Regex for Anthropic/Claude XML-style function calls:
// <function_calls><invoke name="tool"><parameter name="param">val</parameter></invoke></function_calls>
var anthropicXMLRe = regexp.MustCompile(`(?s)\x3cfunction_calls\x3e([\s\S]*?)\x3c/function_calls\x3e`)
var anthropicInvokeRe = regexp.MustCompile(`(?s)\x3cinvoke name="([^"]*)"\x3e([\s\S]*?)\x3c/invoke\x3e`)
var anthropicParamRe = regexp.MustCompile(`(?s)\x3cparameter name="([^"]*)"\x3e([\s\S]*?)\x3c/parameter\x3e`)

// Regex for Kimi's <Tool>name</Tool> format.
// Model may emit: "I'll run that.\n\n<Tool>terminal</Tool>"
var kimiToolRe = regexp.MustCompile("(?s)<Tool>(\\S+?)</Tool>")

// parseToolCallsFromText extracts tool_calls from Qoder text response.
// Handles three formats in priority order:
//  1. ```json\n{"tool_calls": [...]}\n```  (preferred, matching qoder-proxy)
//  2. Balanced JSON extraction with brace counting (bare JSON fallback)
//  3. [assistant called tool: NAME with arguments: ARGS] (inline text format)
// Returns parsed tool_calls and clean text with tool_call content removed.
func parseToolCallsFromText(text string) ([]ToolCall, string) {
	// Format 0: <tool_call> XML-style (Qoder native when native tools disabled)
	xmlMatches := xmlToolCallRe.FindAllStringSubmatchIndex(text, -1)
	if len(xmlMatches) > 0 {
		var calls []ToolCall
		for _, xmlMatch := range xmlMatches {
			xmlString := text[xmlMatch[2]:xmlMatch[3]]
			var obj map[string]interface{}
			if json.Unmarshal([]byte(xmlString), &obj) == nil {
				if name, ok := obj["name"].(string); ok && name != "" {
					var argsJSON string
					switch a := obj["arguments"].(type) {
					case string:
						argsJSON = a
					case nil:
						argsJSON = "{}"
					default:
						b, _ := json.Marshal(a)
						argsJSON = string(b)
					}
					if argsJSON == "" {
						argsJSON = "{}"
					}
					calls = append(calls, ToolCall{
						ID:       generateCallID(),
						Type:     "function",
						Function: ToolCallFn{Name: name, Arguments: argsJSON},
					})
				}
			}
		}
		if len(calls) > 0 {
			// Remove all <tool_call>...</tool_call> blocks from text
			cleanText := xmlToolCallRe.ReplaceAllString(text, "")
			cleanText = strings.TrimSpace(cleanText)
			return calls, cleanText
		}
	}
	// Format 0b: Anthropic/Claude XML-style <function_calls><invoke>...</function_calls>
	// Model sometimes emits this when confused about tool protocol.
	anthroMatches := anthropicXMLRe.FindAllStringSubmatchIndex(text, -1)
	if len(anthroMatches) > 0 {
		var calls []ToolCall
		for _, am := range anthroMatches {
			inner := text[am[2]:am[3]]
			invokes := anthropicInvokeRe.FindAllStringSubmatch(inner, -1)
			for _, inv := range invokes {
				if len(inv) < 3 {
					continue
				}
				name := inv[1]
				paramBlock := inv[2]
				params := anthropicParamRe.FindAllStringSubmatch(paramBlock, -1)
				args := make(map[string]string)
				for _, p := range params {
					if len(p) >= 3 {
						args[p[1]] = p[2]
					}
				}
				argsJSON := "{}"
				if len(args) > 0 {
					if b, err := json.Marshal(args); err == nil {
						argsJSON = string(b)
					}
				}
				calls = append(calls, ToolCall{
					ID:       generateCallID(),
					Type:     "function",
					Function: ToolCallFn{Name: name, Arguments: argsJSON},
				})
			}
		}
		if len(calls) > 0 {
			cleanText := anthropicXMLRe.ReplaceAllString(text, "")
			cleanText = strings.TrimSpace(cleanText)
			return calls, cleanText
		}
	}
	// Format 0c: Kimi's <Tool>name</Tool> format
	// Model may emit: "I'll run that.\n\n<Tool>terminal</Tool>"
	if kimiMatches := kimiToolRe.FindAllStringSubmatch(text, -1); len(kimiMatches) > 0 {
		var calls []ToolCall
		for _, m := range kimiMatches {
			if len(m) < 2 { continue }
			name := m[1]
			calls = append(calls, ToolCall{
				ID: generateCallID(), Type: "function",
				Function: ToolCallFn{Name: name, Arguments: "{}"},
			})
		}
		if len(calls) > 0 {
			cleanText := kimiToolRe.ReplaceAllString(text, "")
			cleanText = strings.TrimSpace(cleanText)
			return calls, cleanText
		}
	}
	// Format 1: ```json fenced block
	blockMatch := jsonBlockRe.FindStringSubmatchIndex(text)

	var jsonString string
	var prefixText string

	if blockMatch != nil {
		jsonString = text[blockMatch[2]:blockMatch[3]]
		prefixText = text[:blockMatch[0]]
	} else {
		// Format 2: balanced brace extraction (fallback for missing fences)
		extracted := extractBalancedJsonWithToolCalls(text)
		if extracted == nil {
			// Format 3: try inline [assistant called tool: NAME with arguments: ARGS]
			return parseInlineToolCalls(text)
		}
		jsonString = extracted.json
		prefixText = extracted.prefix
	}

	// Parse the JSON
	parsed, err := parseToolCallsJSON(jsonString)
	if err != nil || len(parsed) == 0 {
		// Format 3: try inline
		return parseInlineToolCalls(text)
	}

	// Build tool calls
	var calls []ToolCall
	for _, tc := range parsed {
		name, _ := tc["name"].(string)
		if name == "" {
			continue
		}
		args := tc["arguments"]
		var argsJSON string
		switch a := args.(type) {
		case string:
			argsJSON = a
		case nil:
			argsJSON = "{}"
		default:
			b, _ := json.Marshal(a)
			argsJSON = string(b)
		}
		if argsJSON == "" {
			argsJSON = "{}"
		}
		calls = append(calls, ToolCall{
			ID:       generateCallID(),
			Type:     "function",
			Function: ToolCallFn{Name: name, Arguments: argsJSON},
		})
	}

	if len(calls) == 0 {
		return parseInlineToolCalls(text)
	}

	// Remove tool_call blocks from text, preserve prefix
	cleanText := strings.TrimSpace(prefixText)
	return calls, cleanText
}

// parseInlineToolCalls extracts [assistant called tool: NAME with arguments: ARGS]
// patterns from text. Uses balanced bracket counting to handle nested arrays/objects
// in arguments. This format appears when Qoder models mirror the history serialization
// format that normalizeMessages injects.
func parseInlineToolCalls(text string) ([]ToolCall, string) {
	var calls []ToolCall
	remaining := text

	for {
		loc := inlineToolCallRe.FindStringSubmatchIndex(remaining)
		if loc == nil {
			break
		}
		name := remaining[loc[2]:loc[3]]
		// Arguments start after "with arguments: "
		argsStart := loc[1] // end of full match

		// Extract arguments using balanced bracket counting
		args, argsEnd := extractBalancedArgs(remaining, argsStart)
		if argsEnd < 0 {
			// Can't parse args — skip this marker
			remaining = remaining[loc[0]+1:]
			continue
		}

		// Find the closing ]
		closeIdx := strings.Index(remaining[argsEnd:], "]")
		if closeIdx < 0 {
			remaining = remaining[loc[0]+1:]
			continue
		}
		pos := argsEnd + closeIdx // position of ] in remaining

		if args == "" {
			args = "{}"
		}
		calls = append(calls, ToolCall{
			ID:       generateCallID(),
			Type:     "function",
			Function: ToolCallFn{Name: name, Arguments: args},
		})

		// Remove the matched marker from remaining
		remaining = remaining[:loc[0]] + remaining[pos+1:]
	}

	if len(calls) == 0 {
		return nil, text
	}

	return calls, strings.TrimSpace(remaining)
}

// extractBalancedArgs extracts a balanced JSON object/array from text starting at pos.
// Returns the extracted string and the end position. If the text starts with { or [,
// uses brace counting; otherwise reads until ] delimiter.
func extractBalancedArgs(text string, pos int) (string, int) {
	if pos >= len(text) {
		return "", -1
	}

	ch := text[pos]
	if ch != '{' && ch != '[' {
		// Not a JSON structure — read plain text until ]
		end := strings.IndexByte(text[pos:], ']')
		if end < 0 {
			return "", -1
		}
		return strings.TrimSpace(text[pos : pos+end]), pos + end
	}

	depth := 0
	inString := false
	escapeNext := false

	for i := pos; i < len(text); i++ {
		c := text[i]
		if escapeNext {
			escapeNext = false
			continue
		}
		if c == '\\' && inString {
			escapeNext = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == '{' || c == '[' {
			depth++
		}
		if c == '}' || c == ']' {
			depth--
			if depth == 0 {
				return text[pos : i+1], i + 1
			}
		}
	}

	return "", -1
}

// balancedExtract holds a JSON string and the prefix text before it.
type balancedExtract struct {
	json   string
	prefix string
}

// extractBalancedJsonWithToolCalls uses brace counting to find the outermost
// balanced JSON object containing "tool_calls". More robust than regex for
// nested objects. Matches qoder-proxy's extractBalancedJsonWithToolCalls.
func extractBalancedJsonWithToolCalls(text string) *balancedExtract {
	for start := 0; start < len(text); start++ {
		if text[start] != '{' {
			continue
		}

		depth := 0
		inString := false
		escapeNext := false
		end := start

		for i := start; i < len(text); i++ {
			ch := text[i]
			if escapeNext {
				escapeNext = false
				continue
			}
			if ch == '\\' && inString {
				escapeNext = true
				continue
			}
			if ch == '"' {
				inString = !inString
				continue
			}
			if inString {
				continue
			}
			if ch == '{' {
				depth++
			}
			if ch == '}' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}

		if depth != 0 {
			continue // unbalanced
		}

		candidate := text[start : end+1]
		if !strings.Contains(candidate, `"tool_calls"`) &&
			!strings.Contains(candidate, `"name"`) &&
			!strings.Contains(candidate, `"tool"`) {
			continue
		}

		if parsed, err := parseToolCallsJSON(candidate); err == nil && len(parsed) > 0 {
			return &balancedExtract{
				json:   candidate,
				prefix: text[:start],
			}
		}
	}
	return nil
}

// parseToolCallsJSON parses a JSON string that may contain tool_calls in either:
//   - {"tool_calls": [{"name": "...", "arguments": {...}}]}
//   - [{"name": "...", "arguments": {...}}]
func parseToolCallsJSON(jsonStr string) ([]map[string]interface{}, error) {
	// Try {"tool_calls": [...]} first
	var withKey struct {
		ToolCalls []map[string]interface{} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &withKey); err == nil && len(withKey.ToolCalls) > 0 {
		return normalizeToolCallEntries(withKey.ToolCalls)
	}

	// Try bare array [...]
	var arr []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &arr); err == nil && len(arr) > 0 {
		return normalizeToolCallEntries(arr)
	}

	// Try bare object {"name": "tool", "arguments": {...}} or {"tool": "tool", "parameters": {...}}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &obj); err == nil {
		name, _ := obj["name"].(string)
		if name == "" {
			name, _ = obj["tool"].(string)
		}
		if name != "" {
			return normalizeToolCallEntries([]map[string]interface{}{obj})
		}
	}

	return nil, fmt.Errorf("no tool_calls found")
}

// normalizeToolCallEntries fixes field name variations across models.
// Some models use "tool"/"parameters" instead of "name"/"arguments".
func normalizeToolCallEntries(entries []map[string]interface{}) ([]map[string]interface{}, error) {
	var result []map[string]interface{}
	for _, e := range entries {
		// Normalize: OpenAI native {function: {name, arguments}} → flat {name, arguments}
		if fn, ok := e["function"].(map[string]interface{}); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				e["name"] = n
			}
			if a, ok := fn["arguments"]; ok {
				e["arguments"] = a
			}
		}
		// Normalize name: "name" or "tool"
		name, _ := e["name"].(string)
		if name == "" {
			name, _ = e["tool"].(string)
		}
		if name == "" {
			continue
		}
		e["name"] = name

		// Normalize arguments: "arguments" or "parameters"
		if _, ok := e["arguments"]; !ok {
			if params, ok := e["parameters"]; ok {
				e["arguments"] = params
			}
		}
		result = append(result, e)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no valid tool_calls after normalization")
	}
	return result, nil
}

// generateCallID creates an OpenAI-compatible call ID (24 hex chars).
func generateCallID() string {
	b := make([]byte, 12)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("call_%x", b)
}

// buildQoderRequestBody creates the exact JSON payload Qoder expects.
func buildQoderRequestBody(modelKey string, messages []ChatMessage, maxTokens int, creds *patCredential, tools []ToolDef, thinkingEffort string, contextWindow int) ([]byte, error) {
	mc := ensureModelConfig(creds, modelKey)

	normalized, systemText := normalizeMessages(messages, tools)
	lastUser := lastUserText(messages)
	sessionID := stableHash("qoder-session", creds.userID, modelKey)
	recordID := stableChatRecordID(modelKey, messages, maxTokens)

	if maxTokens <= 0 {
		maxTokens = 65536
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
		"tools":           []interface{}{}, // ponytail: don't send native tools to Qoder API.
		// Qoder's native tool_call implementation doesn't populate function
		// arguments — it emits tool_calls with arguments:"{}" empty.
		// Instead, we inject a text-based tool protocol into the system prompt
		// (in normalizeMessages) and parse ```json blocks from the response.
		// This gives reliable tool calling with full arguments.
		"parameters": map[string]interface{}{"max_tokens": maxTokens},
		"chat_context": map[string]interface{}{
			"chatPrompt":  "",
			"imageUrls":   nil,
			"extra": map[string]interface{}{
				"context":         []interface{}{},
				"modelConfig":     map[string]interface{}{"key": modelKey, "is_reasoning": mc.IsReasoning},
				"originalContent": lastUser,
				"thinking_effort": thinkingEffort,
				"context_window":  contextWindow,
			},
			"features": []interface{}{},
			"text":     lastUser,
		},
		"model_config": func() interface{} {
			if mc.Raw != nil {
				return mc.Raw
			}
			return map[string]interface{}{"key": modelKey}
		}(),
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

type ChatResult struct {
	Text      string
	ToolCalls []ToolCall // parsed from text if present
	Error     error
	RequestID string
}

// ToolCall represents a parsed tool call from Qoder's text response.
type ToolCall struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Function  ToolCallFn  `json:"function"`
}

type ToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolsPayload converts OpenAI tool defs to Qoder tool format.
// Returns []interface{}{} when tools empty so JSON stays an array.
func toolsPayload(tools []ToolDef) []interface{} {
	if len(tools) == 0 {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]interface{}{
			"type":     "function",
			"function": t.Function,
		})
	}
	return out
}

// StreamCallback is called for each text chunk during streaming.
type StreamCallback func(text string)

// callQoder sends a chat completion request directly to Qoder's API with
// full COSY signing and WAF encoding. No qodercli required.
func callQoder(ctx context.Context, pat, modelKey string, messages []ChatMessage, maxTokens int, onChunk StreamCallback, tools []ToolDef, thinkingEffort string, contextWindow int) (*ChatResult, error) {
	t0 := time.Now()
	// 1. Resolve credential (PAT → job token + userId)
	cred, err := getCredential(pat)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	t1 := time.Now()

	// 2. Build request body (Qoder format)
	payload, err := buildQoderRequestBody(modelKey, messages, maxTokens, cred, tools, thinkingEffort, contextWindow)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	t2 := time.Now()

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
	t3 := time.Now()

	reqID := uuidString()

	// 5. Send request (retry with different proxy on network error)
	req, err := http.NewRequestWithContext(ctx, "POST", qoderChatURL, bytes.NewReader(encodedBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Model-Key", modelKey)
	if mc := getCachedModelConfig(modelKey); mc != nil {
		req.Header.Set("X-Model-Source", mc.Source)
	}
	req.Header.Set("Authorization", cosy.Authorization)
	for k, v := range cosy.Headers {
		req.Header.Set(k, v)
	}

	var resp *http.Response
	proxyRetries := proxyCount()
	if proxyRetries < 1 {
		proxyRetries = 1
	}
	if proxyRetries > 3 {
		proxyRetries = 3
	}
	for attempt := 0; attempt < proxyRetries; attempt++ {
		// Each attempt needs a fresh body; http.Client.Do consumes req.Body.
		attemptReq := req.Clone(ctx)
		if req.GetBody != nil {
			attemptReq.Body, err = req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("reset request body: %w", err)
			}
		}
		resp, err = proxyClientFn().Do(attemptReq)
		if err == nil {
			break
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if attempt+1 < proxyRetries {
			log.Printf("proxy retry %d/%d: %v", attempt+1, proxyRetries, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("request (after %d proxy attempts): %w", proxyRetries, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		credCache.Delete(pat)
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: string(b)}
	}

	// 6. Parse SSE with Qoder envelope unwrapping
	tSend := time.Now()
	tFirstChunk := time.Time{}
	wrappedChunk := onChunk
	if onChunk != nil {
		wrappedChunk = func(text string) {
			if tFirstChunk.IsZero() {
				tFirstChunk = time.Now()
			}
			onChunk(text)
		}
	}
	text, nativeToolCalls, err := unwrapQoderSSE(resp.Body, wrappedChunk)
	tDone := time.Now()
	logCallQoderTiming(t0, t1, t2, t3, tSend, tFirstChunk, tDone, modelKey)
	if err != nil {
		return nil, fmt.Errorf("parse SSE: %w", err)
	}

	// 7. Parse tool_call blocks from text response (```json format)
	textToolCalls, cleanText := parseToolCallsFromText(text)

	// 8. Merge: prefer text-parsed (more reliable), fall back to native
	var allToolCalls []ToolCall
	if len(textToolCalls) > 0 {
		allToolCalls = textToolCalls
	} else if len(nativeToolCalls) > 0 {
		// Upstream sends incremental deltas for the same tool call (same
		// index) with partial function.arguments in each chunk. We must
		// accumulate by index and concatenate argument fragments before
		// building the final ToolCall list.
		merged := accumulateNativeToolCalls(nativeToolCalls)
		for _, tc := range merged {
			name, _ := tc["name"].(string)
			args := tc["arguments"]
			if name == "" {
				if fn, ok := tc["function"].(map[string]interface{}); ok {
					name, _ = fn["name"].(string)
					args = fn["arguments"]
				}
			}
			if name == "" {
				continue
			}
			var argsJSON string
			switch a := args.(type) {
			case string:
				argsJSON = a
			case nil:
				argsJSON = "{}"
			default:
				b, _ := json.Marshal(a)
				argsJSON = string(b)
			}
			if argsJSON == "" {
				argsJSON = "{}"
			}
			callID, _ := tc["id"].(string)
			if callID == "" {
				callID = generateCallID()
			}
			allToolCalls = append(allToolCalls, ToolCall{
				ID:       callID,
				Type:     "function",
				Function: ToolCallFn{Name: name, Arguments: argsJSON},
			})
		}
	}

	return &ChatResult{Text: cleanText, ToolCalls: allToolCalls, RequestID: reqID}, nil
}

// accumulateNativeToolCalls merges streaming tool_call deltas by their index.
// Upstream sends one delta per SSE chunk with the same `index` field; each
// delta may carry only a partial piece of function.arguments. This function
// concatenates argument fragments and keeps the first non-empty id/name/type.
func accumulateNativeToolCalls(deltas []map[string]interface{}) []map[string]interface{} {
	type acc struct {
		id   string
		name string
		args strings.Builder
		typ  string
	}
	byIndex := make(map[int]*acc)
	maxIdx := -1
	for _, d := range deltas {
		idx := 0
		if f, ok := d["index"].(float64); ok {
			idx = int(f)
		} else if i, ok := d["index"].(int); ok {
			idx = i
		}
		if _, ok := byIndex[idx]; !ok {
			byIndex[idx] = &acc{typ: "function"}
			if idx > maxIdx {
				maxIdx = idx
			}
		}
		a := byIndex[idx]
		if id, ok := d["id"].(string); ok && id != "" && a.id == "" {
			a.id = id
		}
		if t, ok := d["type"].(string); ok && t != "" {
			a.typ = t
		}
		name, _ := d["name"].(string)
		args := d["arguments"]
		if name == "" || args == nil {
			if fn, ok := d["function"].(map[string]interface{}); ok {
				if n, ok := fn["name"].(string); ok && n != "" && a.name == "" {
					a.name = n
				}
				if arg, ok := fn["arguments"]; ok {
					args = arg
				}
				if id, ok := fn["id"].(string); ok && id != "" && a.id == "" {
					a.id = id
				}
			}
		} else if a.name == "" {
			a.name = name
		}
		if args != nil {
			switch v := args.(type) {
			case string:
				a.args.WriteString(v)
			default:
				if b, err := json.Marshal(v); err == nil {
					a.args.WriteString(string(b))
				}
			}
		}
	}
	out := make([]map[string]interface{}, 0, maxIdx+1)
	for i := 0; i <= maxIdx; i++ {
		a, ok := byIndex[i]
		if !ok {
			continue
		}
		argsStr := a.args.String()
		if argsStr == "" {
			argsStr = "{}"
		}
		out = append(out, map[string]interface{}{
			"id":   a.id,
			"type": a.typ,
			"name": a.name,
			"function": map[string]interface{}{
				"name":      a.name,
				"arguments": argsStr,
			},
		})
	}
	return out
}

// logCallQoderTiming logs detailed timing breakdown for debugging latency.
func logCallQoderTiming(t0, t1, t2, t3, tSend, tFirstChunk, tDone time.Time, modelKey string) {
	credMs := t1.Sub(t0).Milliseconds()
	buildMs := t2.Sub(t1).Milliseconds()
	signMs := t3.Sub(t2).Milliseconds()
	sendMs := tSend.Sub(t3).Milliseconds()
	parseMs := tDone.Sub(tSend).Milliseconds()
	totalMs := tDone.Sub(t0).Milliseconds()

	if totalMs > 5000 {
		log.Printf("perf: %s cred=%dms build=%dms sign=%dms send=%dms parse=%dms total=%dms",
			modelKey, credMs, buildMs, signMs, sendMs, parseMs, totalMs)
	} else if parseMs > 3000 {
		log.Printf("perf: %s parse=%dms (slow streaming)", modelKey, parseMs)
	}
}

// unwrapQoderSSE reads Qoder's SSE stream which wraps responses in a
// {statusCodeValue, body} envelope and returns the concatenated text
// plus any native tool_calls from the stream delta/message.
func unwrapQoderSSE(body io.Reader, onChunk StreamCallback) (string, []map[string]interface{}, error) {
	var full strings.Builder
	var toolCalls []map[string]interface{} // accumulated tool_calls from stream
	sawDone := false
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // 4MB max chunk

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue // keep-alive, not end-of-stream
		}
		if data == "[DONE]" {
			sawDone = true
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
			return full.String(), nil, &UpstreamError{StatusCode: envelope.StatusCode, Body: truncate(envelope.Body, 200)}
		}

		inner := envelope.Body
		if inner == "" {
			continue // empty body = no-op chunk (keep-alive/metadata), not end
		}
		if inner == "[DONE]" {
			sawDone = true
			break
		}

		// Parse inner OpenAI-style chunk
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string                   `json:"content"`
					ToolCalls []map[string]interface{} `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content   string                   `json:"content"`
					ToolCalls []map[string]interface{} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(inner), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			text := ch.Delta.Content
			tc := ch.Delta.ToolCalls
			if text == "" {
				text = ch.Message.Content
			}
			if len(tc) == 0 {
				tc = ch.Message.ToolCalls
			}
			if len(tc) > 0 {
				toolCalls = append(toolCalls, tc...)
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
		return full.String(), toolCalls, err
	}
	if !sawDone {
		// Some upstream responses end cleanly without a [DONE] sentinel.
		// If we already collected content, treat it as a successful stream.
		if full.Len() == 0 && len(toolCalls) == 0 {
			return "", nil, fmt.Errorf("qoder stream ended before [DONE]")
		}
	}

	return full.String(), toolCalls, nil
}
