// db.go — SQLite database layer for qoder-bridge.
//
// Uses modernc.org/sqlite (pure Go, no CGO) via database/sql.
// Stores: request logs, token/credit usage per PAT, runtime config.
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	db     *sql.DB
	dbOnce sync.Once
	dbPath string
)

// dbLocation returns the DB file path, creating parent dirs.
// Uses the bridge's working directory (where .env lives), not $HOME.
// This ensures the DB stays with the bridge installation regardless
// of which user runs the service.
func dbLocation() string {
	if dbPath != "" {
		return dbPath
	}
	// Try working directory first (where .env and install.sh live)
	wd, err := os.Getwd()
	if err == nil && wd != "/" && wd != "/root" {
		dir := filepath.Join(wd, ".qoder-bridge")
		os.MkdirAll(dir, 0755)
		dbPath = filepath.Join(dir, "data.db")
		return dbPath
	}
	// Fallback: $HOME/.qoder-bridge
	dir := filepath.Join(os.Getenv("HOME"), ".qoder-bridge")
	os.MkdirAll(dir, 0755)
	dbPath = filepath.Join(dir, "data.db")
	return dbPath
}

// initDB opens (or creates) the SQLite database and migrates schema.
func initDB() error {
	var err error
	dbOnce.Do(func() {
		db, err = sql.Open("sqlite", dbLocation()+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return
		}
		// SQLite via modernc.org/sqlite is internally serialized, but
		// capping pool prevents runaway goroutines from holding the
		// driver mutex. One writer at a time is sufficient for our
		// log/usage throughput.
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
		db.SetConnMaxIdleTime(5 * time.Minute)
		err = migrate(db)
	})
	return err
}

func migrate(d *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			pat TEXT NOT NULL,
			model TEXT NOT NULL,
			stream INTEGER NOT NULL,
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			credits REAL DEFAULT 0,
			status INTEGER DEFAULT 0,
			error TEXT DEFAULT '',
			latency_ms INTEGER DEFAULT 0,
			client_ip TEXT DEFAULT '',
			api_key TEXT DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_pat ON request_logs(pat)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model)`,

		`CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		// API keys table: support multiple named sk-* keys.
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			api_key TEXT NOT NULL UNIQUE,
			enabled INTEGER DEFAULT 1,
			created_at INTEGER NOT NULL
		)`,

		// DB size guard: we aim for <100MB
		`PRAGMA auto_vacuum=INCREMENTAL`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Idempotent column add: ALTER TABLE ADD COLUMN fails if column exists,
	// so we catch and ignore the "duplicate column" error.
	if _, err := d.Exec(`ALTER TABLE request_logs ADD COLUMN api_key TEXT DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate: add api_key column: %w", err)
		}
	}
	// Add permissions column to api_keys (empty = all permissions).
	if _, err := d.Exec(`ALTER TABLE api_keys ADD COLUMN permissions TEXT DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate: add permissions column: %w", err)
		}
	}
	return nil
}

// ── Config helpers ──────────────────────────────────────────────────────────

func cfgGet(key string) string {
	if db == nil {
		return ""
	}
	var v string
	db.QueryRow(`SELECT value FROM config WHERE key=?`, key).Scan(&v)
	return v
}

func cfgSet(key, value string) {
	if db == nil {
		return
	}
	db.Exec(`INSERT INTO config(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

// importEnvFromConfig migrates .env values to DB (only when DB key is empty).
// Called once on startup — after first run, DB is the source of truth.
func importEnvFromConfig(cfg *envConfig) {
	imports := map[string]string{}
	if cfg.requestDelay > 0 && cfgGet("request_delay_ms") == "" {
		imports["request_delay_ms"] = fmt.Sprintf("%d", cfg.requestDelay)
	}
	if cfg.strategy != "" && cfgGet("pat_strategy") == "" {
		imports["pat_strategy"] = cfg.strategy
	}
	// Proxy comes from env var
	if p := os.Getenv("QODER_PROXY"); p != "" && cfgGet("proxy") == "" {
		imports["proxy"] = p
	}
	// Domain from .env
	if cfg.domain != "" && cfgGet("domain") == "" {
		imports["domain"] = cfg.domain
	}
	if len(imports) == 0 {
		return
	}
	for k, v := range imports {
		cfgSet(k, v)
		log.Printf("  imported from .env: %s", k)
	}
}

func cfgBool(key string, def bool) bool {
	v := cfgGet(key)
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "on"
}

// ── Logging ─────────────────────────────────────────────────────────────────

type LogEntry struct {
	PAT              string
	Model            string
	Stream           bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Credits          float64
	Status           int
	Error            string
	LatencyMs        int64
	ClientIP         string
	APIKey           string
}

// logRequest inserts a request log entry into the DB.
func logRequest(e LogEntry) {
	if db == nil {
		return
	}
	stream := 0
	if e.Stream {
		stream = 1
	}
	_, err := db.Exec(`INSERT INTO request_logs(ts,pat,model,stream,prompt_tokens,completion_tokens,total_tokens,credits,status,error,latency_ms,client_ip,api_key)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().Unix(), e.PAT, e.Model, stream,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.Credits,
		e.Status, e.Error, e.LatencyMs, e.ClientIP, e.APIKey)
	if err != nil {
		log.Printf("db: failed to log request: %v", err)
	}
}

// ── Usage queries ───────────────────────────────────────────────────────────

type UsageRow struct {
	Group       string // PAT or API key, depending on query type
	Model       string
	Requests    int
	Tokens      int
	Credits     float64
	FirstTS     int64
	LastTS      int64
	AvgLatencyMs int64
}

// queryUsageByPAT groups usage by PAT + model.
func queryUsageByPAT(fromTS, toTS int64) ([]UsageRow, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	rows, err := db.Query(`
		SELECT pat, model, COUNT(*),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(credits),0.0),
		       MIN(ts), MAX(ts),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER),0)
		FROM request_logs
		WHERE ts >= ? AND ts <= ?
		GROUP BY pat, model
		ORDER BY pat, model`, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Group, &r.Model, &r.Requests, &r.Tokens, &r.Credits, &r.FirstTS, &r.LastTS, &r.AvgLatencyMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// queryUsageByAPIKey groups usage by API key + model.
func queryUsageByAPIKey(fromTS, toTS int64) ([]UsageRow, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	rows, err := db.Query(`
		SELECT COALESCE(api_key,'(no key)'), model, COUNT(*),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(credits),0.0),
		       MIN(ts), MAX(ts),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER),0)
		FROM request_logs
		WHERE ts >= ? AND ts <= ?
		GROUP BY api_key, model
		ORDER BY api_key, model`, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Group, &r.Model, &r.Requests, &r.Tokens, &r.Credits, &r.FirstTS, &r.LastTS, &r.AvgLatencyMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// queryUsageSummary returns overall totals for a period.
type UsageSummary struct {
	TotalRequests  int
	TotalTokens   int
	TotalCredits  float64
	AvgLatencyMs  int64
	ErrorCount    int
}

func queryUsageSummary(fromTS, toTS int64) (*UsageSummary, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	s := &UsageSummary{}
	var errCount int64 // modernc.org/sqlite returns SUM() as int64
	err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(credits),0),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER),0),
		       COALESCE(SUM(CASE WHEN status != 200 THEN 1 ELSE 0 END),0)
		FROM request_logs
		WHERE ts >= ? AND ts <= ?`, fromTS, toTS).Scan(&s.TotalRequests, &s.TotalTokens, &s.TotalCredits, &s.AvgLatencyMs, &errCount)
	if err != nil {
		return nil, err
	}
	s.ErrorCount = int(errCount)
	return s, nil
}

// ── API Key management ─────────────────────────────────────────────────────

type APIKeyEntry struct {
	ID          int
	Name        string
	APIKey      string
	Enabled     bool
	Permissions string // comma-separated, empty = all
	CreatedAt   int64
}

func listAPIKeys() ([]APIKeyEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	rows, err := db.Query(`SELECT id, name, api_key, enabled, permissions, created_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyEntry
	for rows.Next() {
		var k APIKeyEntry
		var en int
		if err := rows.Scan(&k.ID, &k.Name, &k.APIKey, &en, &k.Permissions, &k.CreatedAt); err != nil {
			continue
		}
		k.Enabled = en == 1
		out = append(out, k)
	}
	return out, nil
}

// listEnabledAPIKeys returns only rows where enabled=1. Used by the auth
// path so disabled rows don't count as valid bearers even if their name
// matches.
func listEnabledAPIKeys() ([]APIKeyEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	rows, err := db.Query(`SELECT id, name, api_key, enabled, permissions, created_at FROM api_keys WHERE enabled = 1 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyEntry
	for rows.Next() {
		var k APIKeyEntry
		var en int
		if err := rows.Scan(&k.ID, &k.Name, &k.APIKey, &en, &k.Permissions, &k.CreatedAt); err != nil {
			continue
		}
		k.Enabled = true
		out = append(out, k)
	}
	return out, nil
}

func addAPIKey(name, key, permissions string) error {
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	_, err := db.Exec(`INSERT INTO api_keys(name, api_key, enabled, permissions, created_at) VALUES(?,?,1,?,?)`,
		name, key, permissions, time.Now().Unix())
	return err
}

func removeAPIKey(id int) error {
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	_, err := db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

func toggleAPIKey(id int, enabled bool) error {
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	en := 0
	if enabled {
		en = 1
	}
	_, err := db.Exec(`UPDATE api_keys SET enabled = ? WHERE id = ?`, en, id)
	return err
}

func updatePermissions(id int, perms string) error {
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	_, err := db.Exec(`UPDATE api_keys SET permissions = ? WHERE id = ?`, perms, id)
	return err
}

// validateAPIKey checks if a key is valid and enabled. Returns key name and permissions.
// Disabled rows (enabled=0) are treated as nonexistent — invisible to
// auth — matching the user's "disable = doesn't exist" mental model.
func validateAPIKey(key string) (string, string, bool) {
	if db == nil || key == "" {
		return "", "", false
	}
	var name, perms string
	var en int
	err := db.QueryRow(`SELECT name, enabled, permissions FROM api_keys WHERE api_key = ?`, key).Scan(&name, &en, &perms)
	if err != nil {
		return "", "", false
	}
	if en != 1 {
		return "", "", false
	}
	return name, perms, true
}

// hasPermission checks if a key's permissions allow access to the given path.
// Empty permissions = all access. Permission values: chat, models, status, quota, logs, combos.
func hasPermission(perms, path string) bool {
	if perms == "" {
		return true // no restrictions
	}
	// Map path to permission name
	perm := ""
	switch {
	case strings.HasPrefix(path, "/v1/chat"):
		perm = "chat"
	case path == "/v1/models":
		perm = "models"
	case path == "/v1/status":
		perm = "status"
	case path == "/v1/quota":
		perm = "quota"
	case path == "/v1/logs":
		perm = "logs"
	case path == "/v1/combos":
		perm = "combos"
	default:
		return true // unknown paths pass through
	}
	for _, p := range strings.Split(perms, ",") {
		if strings.TrimSpace(p) == perm {
			return true
		}
	}
	return false
}

// ── Log queries ───────────────────────────────────────────────────────────

type LogRow struct {
	TS              int64
	PAT             string
	Model           string
	Stream          int
	TotalTokens     int
	Credits         float64
	Status          int
	Error           string
	LatencyMs       int
}

func queryLogs(fromTS, toTS int64) ([]LogRow, error) {
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	rows, err := db.Query(`
		SELECT ts, pat, model, stream, total_tokens, credits, status, error, latency_ms
		FROM request_logs
		WHERE ts >= ? AND ts <= ?
		ORDER BY ts DESC
		LIMIT 200`, fromTS, toTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.TS, &r.PAT, &r.Model, &r.Stream, &r.TotalTokens, &r.Credits, &r.Status, &r.Error, &r.LatencyMs); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ── DB size guard ───────────────────────────────────────────────────────────

func dbSizeBytes() int64 {
	fi, err := os.Stat(dbLocation())
	if err != nil {
		return 0
	}
	return fi.Size()
}

func enforceDBLimit(maxMB int) {
	if db == nil {
		return
	}

	// 1) Delete logs older than 365 days
	cutoff365 := time.Now().Add(-365 * 24 * time.Hour).Unix()
	res, _ := db.Exec(`DELETE FROM request_logs WHERE ts < ?`, cutoff365)
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("db cleanup: removed %d logs older than 365 days", n)
	}

	// 2) If DB file still too big, delete oldest 20% of remaining logs
	maxBytes := int64(maxMB) * 1024 * 1024
	if dbSizeBytes() >= maxBytes {
		var oldest int64
		db.QueryRow(`SELECT ts FROM request_logs ORDER BY ts ASC LIMIT 1`).Scan(&oldest)
		if oldest > 0 {
			cutoff := oldest + (time.Now().Unix()-oldest)/5
			res2, _ := db.Exec(`DELETE FROM request_logs WHERE ts < ?`, cutoff)
			if n, _ := res2.RowsAffected(); n > 0 {
				log.Printf("db cleanup: removed %d oldest logs (size limit)", n)
			}
		}
	}

	// 3) Reclaim space
	db.Exec(`PRAGMA incremental_vacuum`)
}

// ── Timezone helpers ────────────────────────────────────────────────────────

var wib = time.FixedZone("WIB", 7*3600)

func formatTS(ts int64) string {
	t := time.Unix(ts, 0).In(wib)
	return t.Format("02-01-2006 15:04:05 WIB")
}

func formatUTC(ts int64) string {
	t := time.Unix(ts, 0).UTC()
	return t.Format("02-01-2006 15:04:05 UTC")
}

// parseDateRange parses "today|week|month|year|custom" + optional "DD-MM-YYYY,DD-MM-YYYY"
// Returns (fromTS, toTS, label).
func parseDateRange(args []string) (int64, int64, string, error) {
	now := time.Now().In(wib)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wib)

	if len(args) == 0 {
		return today.Unix(), now.Unix(), "today", nil
	}

	mode := strings.ToLower(args[0])
	switch mode {
	case "today":
		return today.Unix(), now.Unix(), "today", nil
	case "week", "thisweek":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		monday := today.AddDate(0, 0, -(weekday - 1))
		return monday.Unix(), now.Unix(), "this week", nil
	case "month", "thismonth":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, wib)
		return first.Unix(), now.Unix(), "this month", nil
	case "year", "thisyear":
		first := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, wib)
		return first.Unix(), now.Unix(), "this year", nil
	case "custom":
		if len(args) < 3 {
			return 0, 0, "", fmt.Errorf("usage: custom DD-MM-YYYY DD-MM-YYYY")
		}
		from, err := time.ParseInLocation("02-01-2006", args[1], wib)
		if err != nil {
			return 0, 0, "", fmt.Errorf("bad from date: %v", err)
		}
		to, err := time.ParseInLocation("02-01-2006", args[2], wib)
		if err != nil {
			return 0, 0, "", fmt.Errorf("bad to date: %v", err)
		}
		to = to.Add(24*time.Hour - time.Second)
		return from.Unix(), to.Unix(), fmt.Sprintf("%s to %s", args[1], args[2]), nil
	default:
		return 0, 0, "", fmt.Errorf("unknown range: %s (use today|week|month|year|custom)", mode)
	}
}

// ── Credit multipliers (from Qoder docs) ────────────────────────────────────
// Source: https://docs.qoder.com/user-guide/chat/model-tier-selector
//
// Tier multipliers: Auto=1.0x, Ultimate=1.6x, Performance=1.1x, Efficient=0.3x, Lite=Free
// Frontier: Qwen3.7-Max=0.5x, Qwen3.7-Plus=0.1x, DeepSeek-V4-Pro=0.5x,
//   DeepSeek-V4-Flash=0.1x, GLM-5.2=0.6x, Kimi-K2.7-Code=0.3x, MiniMax-M3=0.2x
//
// Time-based discounts (Off-Peak 14:00–00:00 UTC) not applied in real-time;
// we use standard rates as the conservative baseline.
var modelMultiplier = map[string]float64{
	"auto":           1.0,  // Auto: smart routing
	"ultimate":       1.6,  // Ultimate: deep reasoning
	"performance":    1.1,  // Performance: advanced reasoning
	"efficient":      0.3,  // Efficient: standard reasoning
	"lite":           0.0,  // Lite: free
	"qmodel_preview": 0.5,  // Qwen3.8-Max-Preview (not in tier selector, uses 0.5x standard)
	"qmodel_latest":  0.5,  // Qwen3.7-Max
	"qmodel":         0.1,  // Qwen3.7-Plus
	"kmodel_latest":  1.0,  // Kimi-K3 (not in docs, estimated)
	"kmodel":         0.3,  // Kimi-K2.7-Code
	"gm51model":      0.6,  // GLM-5.2
	"dmodel":         0.5,  // DeepSeek-V4-Pro
	"dfmodel":        0.1,  // DeepSeek-V4-Flash
	"mmodel":         0.2,  // MiniMax-M3
}

// Context window options from Qoder docs.
// Source: https://docs.qoder.com/user-guide/chat/model-tier-selector
// 200K=standard, 400K=extended, 1M=extreme
var contextWindowOptions = []int{200000, 400000, 1000000}

// Thinking effort levels from Qoder docs.
// low: minimal reasoning, fastest
// medium: balanced
// high: thorough
// xhigh: deep analysis
var thinkingEffortLevels = []string{"low", "medium", "high", "xhigh"}

// estimateCredits returns estimated credits for a request.
// Heuristic: 1 credit ≈ 1,000 tokens at 1x multiplier.
func estimateCredits(modelKey string, totalTokens int) float64 {
	mult, ok := modelMultiplier[modelKey]
	if !ok {
		mult = 1.0
	}
	if totalTokens <= 0 {
		totalTokens = 1
	}
	return float64(totalTokens) / 1000.0 * mult
}

// estimateTokens is a rough tokenizer (chars/4).
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// Rough heuristic: 1 token ≈ 4 characters for English/code
	n := len(text) / 4
	if n == 0 {
		n = 1
	}
	return n
}
