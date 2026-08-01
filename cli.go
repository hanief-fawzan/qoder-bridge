// cli.go — CLI subcommands for qoder-bridge.
//
// Commands: config, usage, logs.
package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
)

// ── config ──────────────────────────────────────────────────────────────────

func runConfigCLI(args []string) {
	if err := initDB(); err != nil {
		fmt.Fprintf(os.Stderr, "error: db init: %v\n", err)
		os.Exit(1)
	}

	if len(args) == 0 {
		cfg := loadEnv("")
		runConfigTUI(cfg)
		return
	}

	switch args[0] {
	case "show":
		configShow()
	case "apikey":
		configAPIKey(args[1:])
	case "delay":
		configDelay(args[1:])
	case "domain":
		configDomain(args[1:])
	case "proxy":
		configProxy(args[1:])
	case "help", "-h", "--help":
		configHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown config command: %s\n\n", args[0])
		configHelp()
		os.Exit(1)
	}
}

func configHelp() {
	fmt.Print(`qoder-bridge config — manage runtime configuration

Usage:
  qoder-bridge config              Show all config
  qoder-bridge config show         Same as above

API Key:
  qoder-bridge config apikey show      Show current API key
  qoder-bridge config apikey gen       Generate new API key
  qoder-bridge config apikey on        Enable API key auth
  qoder-bridge config apikey off       Disable API key auth
  qoder-bridge config apikey clear     Remove API key

Anti-ban delay:
  qoder-bridge config delay show       Show current delay
  qoder-bridge config delay set MS     Set delay (e.g. 1000)
  qoder-bridge config delay off        Disable delay

Domain (for reverse proxy):
  qoder-bridge config domain show      Show configured domain
  qoder-bridge config domain set HOST  Set domain (e.g. qoder.example.com)
  qoder-bridge config domain clear     Remove domain

Proxy:
  qoder-bridge config proxy show       Show current proxies
  qoder-bridge config proxy set URL    Set proxy (comma-separated for multi)
  qoder-bridge config proxy clear      Remove proxy
`)
}

func configShow() {
	fmt.Println("qoder-bridge config")
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY	VALUE")

	// Auth: global toggle + active key count
	enabled := cfgBool("api_key_enabled", false)
	status := colorRed + "open access"
	if enabled {
		status = colorGreen + "required"
	}
	keys, _ := listEnabledAPIKeys()
	fmt.Fprintf(w, "auth	%s (%d active key(s))%s\n", status, len(keys), colorReset)

	// Delay
	if v := cfgGet("request_delay_ms"); v != "" {
		fmt.Fprintf(w, "request_delay_ms	%s ms\n", v)
	} else {
		fmt.Fprintln(w, "request_delay_ms	(off)")
	}

	// Domain
	if v := cfgGet("domain"); v != "" {
		fmt.Fprintf(w, "domain	%s\n", v)
	} else {
		fmt.Fprintln(w, "domain	(not set)")
	}

	// Proxy
	if v := cfgGet("proxy"); v != "" {
		fmt.Fprintf(w, "proxy	%s\n", v)
	} else {
		fmt.Fprintln(w, "proxy	(not set)")
	}

	w.Flush()
}

func configAPIKey(args []string) {
	if len(args) == 0 {
		configAPIKeyShow()
		return
	}

	switch args[0] {
	case "show", "ls", "list":
		configAPIKeyShow()
	case "on":
		setGlobalAuth(true)
		fmt.Println("API key auth: enabled")
	case "off":
		setGlobalAuth(false)
		fmt.Println("API key auth: disabled (open access)")
	default:
		fmt.Fprintf(os.Stderr, "unknown apikey command: %s\n", args[0])
		os.Exit(1)
	}
}

func configAPIKeyShow() {
	keys, err := listAPIKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "DB error: %v\n", err)
		os.Exit(1)
	}
	enabled := cfgBool("api_key_enabled", false)
	authState := colorRed + "off (open access)"
	if enabled {
		authState = colorGreen + "on (Bearer required)"
	}
	fmt.Printf("Global auth: %s%s\n", authState, colorReset)
	fmt.Printf("API keys:    %d total\n", len(keys))
	if len(keys) > 0 {
		for _, k := range keys {
			st := colorRed + "disabled"
			if k.Enabled {
				st = colorGreen + "enabled"
			}
			fmt.Printf("  • [%d] %s — %s%s%s\n", k.ID, k.Name, st, colorReset, "")
		}
	}
	fmt.Println("\nGenerate a new key via TUI (API Keys → Generate).")
}

func configDelay(args []string) {
	if len(args) == 0 {
		configDelayShow()
		return
	}

	switch args[0] {
	case "show":
		configDelayShow()
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: config delay set MS")
			os.Exit(1)
		}
		cfgSet("request_delay_ms", args[1])
		fmt.Printf("Request delay set to %s ms\n", args[1])
	case "off":
		cfgSet("request_delay_ms", "")
		fmt.Println("Request delay disabled")
	default:
		fmt.Fprintf(os.Stderr, "unknown delay command: %s\n", args[0])
		os.Exit(1)
	}
}

func configDelayShow() {
	v := cfgGet("request_delay_ms")
	if v == "" {
		fmt.Println("Request delay: disabled")
	} else {
		fmt.Printf("Request delay: %s ms (random 0-%s)\n", v, v)
	}
}

func configDomain(args []string) {
	if len(args) == 0 {
		configDomainShow()
		return
	}

	switch args[0] {
	case "show":
		configDomainShow()
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: config domain set HOST")
			os.Exit(1)
		}
		cfgSet("domain", args[1])
		fmt.Printf("Domain set to: %s\n", args[1])
	case "clear":
		cfgSet("domain", "")
		fmt.Println("Domain removed")
	default:
		fmt.Fprintf(os.Stderr, "unknown domain command: %s\n", args[0])
		os.Exit(1)
	}
}

func configDomainShow() {
	v := cfgGet("domain")
	if v == "" {
		fmt.Println("Domain: not set")
	} else {
		fmt.Printf("Domain: %s\n", v)
	}
}

func configProxy(args []string) {
	if len(args) == 0 {
		configProxyShow()
		return
	}

	switch args[0] {
	case "show":
		configProxyShow()
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: config proxy set URL")
			os.Exit(1)
		}
		cfgSet("proxy", args[1])
		fmt.Printf("Proxy set to: %s\n", args[1])
		fmt.Println("Restart bridge for changes to take effect.")
	case "clear":
		cfgSet("proxy", "")
		fmt.Println("Proxy removed")
		fmt.Println("Restart bridge for changes to take effect.")
	default:
		fmt.Fprintf(os.Stderr, "unknown proxy command: %s\n", args[0])
		os.Exit(1)
	}
}

func configProxyShow() {
	v := cfgGet("proxy")
	if v == "" {
		fmt.Println("Proxy: not set (direct connection)")
	} else {
		fmt.Printf("Proxy: %s\n", v)
	}
}

// ── usage ───────────────────────────────────────────────────────────────────

func runUsageCLI(args []string) {
	if err := initDB(); err != nil {
		fmt.Fprintf(os.Stderr, "error: db init: %v\n", err)
		os.Exit(1)
	}

	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		fmt.Println("Usage:")
		fmt.Println("  qoder-bridge usage today")
		fmt.Println("  qoder-bridge usage week")
		fmt.Println("  qoder-bridge usage month")
		fmt.Println("  qoder-bridge usage year")
		fmt.Println("  qoder-bridge usage custom DD-MM-YYYY DD-MM-YYYY")
		os.Exit(1)
	}

	rows, err := queryUsageByPAT(fromTS, toTS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Usage: %s (%s to %s)\n\n", label, formatTS(fromTS), formatTS(toTS))

	if len(rows) == 0 {
		fmt.Println("No data.")
		return
	}

	// Group by PAT
	byPAT := make(map[string][]UsageRow)
	for _, r := range rows {
		byPAT[r.Group] = append(byPAT[r.Group], r)
	}

	pats := make([]string, 0, len(byPAT))
	for p := range byPAT {
		pats = append(pats, p)
	}
	sort.Strings(pats)

	for _, pat := range pats {
		fmt.Printf("PAT: %s\n", pat)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  MODEL\tREQUESTS\tTOKENS\tCREDITS")
		var totalReq, totalTok int
		var totalCred float64
		for _, r := range byPAT[pat] {
			fmt.Fprintf(w, "  %s\t%d\t%d\t%.2f\n", r.Model, r.Requests, r.Tokens, r.Credits)
			totalReq += r.Requests
			totalTok += r.Tokens
			totalCred += r.Credits
		}
		fmt.Fprintf(w, "  ─────\t───────\t──────\t───────\n")
		fmt.Fprintf(w, "  TOTAL\t%d\t%d\t%.2f\n", totalReq, totalTok, totalCred)
		w.Flush()
		fmt.Println()
	}
}

// ── logs ────────────────────────────────────────────────────────────────────

func runLogsCLI(args []string) {
	if err := initDB(); err != nil {
		fmt.Fprintf(os.Stderr, "error: db init: %v\n", err)
		os.Exit(1)
	}

	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Logs: %s (%s to %s)\n\n", label, formatTS(fromTS), formatTS(toTS))

	rows, err := db.Query(`
		SELECT ts, pat, model, stream, total_tokens, credits, status, error, latency_ms
		FROM request_logs
		WHERE ts >= ? AND ts <= ?
		ORDER BY ts DESC
		LIMIT 100`, fromTS, toTS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME (WIB)\tPAT\tMODEL\tSTREAM\tTOKENS\tCREDITS\tSTATUS\tLATENCY")
	for rows.Next() {
		var ts int64
		var pat, model string
		var stream, totalTokens, status, latency int
		var credits float64
		var errStr string
		if err := rows.Scan(&ts, &pat, &model, &stream, &totalTokens, &credits, &status, &errStr, &latency); err != nil {
			continue
		}
		streamStr := ""
		if stream == 1 {
			streamStr = "✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%.2f\t%d\t%dms\n",
			formatTS(ts), pat, model, streamStr, totalTokens, credits, status, latency)
	}
	w.Flush()
}
