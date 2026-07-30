// tui.go — Interactive TUI config menu using tview.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	app     *tview.Application
	pages   *tview.Pages
	envData *envConfig
)

// runConfigTUI launches the interactive config TUI.
func runConfigTUI(cfg *envConfig) {
	envData = cfg
	app = tview.NewApplication()
	pages = tview.NewPages()

	app.SetRoot(pages, true)

	// Main menu
	pages.AddAndSwitchToPage("main", mainMenu(), true)

	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
		os.Exit(1)
	}
}

// pushPage adds a named sub-page and switches to it.
func pushPage(name string, page tview.Primitive) {
	pages.AddAndSwitchToPage(name, page, true)
}

// goBack removes the current page (reveals previous or main).
func goBack() {
	name, _ := pages.GetFrontPage()
	if name == "main" {
		app.Stop()
		return
	}
	pages.RemovePage(name)
}

// setEscape wires Esc on a primitive to call goBack.
func setEscape(p tview.Primitive) tview.Primitive {
	if f, ok := p.(*tview.Form); ok {
		f.SetCancelFunc(func() { goBack() })
		return p
	}
	return p
}

// ── Styles ────────────────────────────────────────────────────────────────

const (
	colorTitle  = "[#5f87ff]"
	colorKey    = "[#ffaf00]"
	colorGreen  = "[#5faf5f]"
	colorRed    = "[#ff5f5f]"
	colorDim    = "[#808080]"
	colorReset  = "[white]"
	colorAccent = "[#00afdf]"
)

// ── Main Menu ─────────────────────────────────────────────────────────────

func mainMenu() tview.Primitive {
	list := tview.NewList().
		AddItem("  🔑  API Keys", "Generate, enable/disable, view", 'a', func() {
			pushPage("sub", apiKeyMenu())
		}).
		AddItem("  🌐  Proxy", "Set, view, or remove proxy URL", 'p', func() {
			pushPage("sub", proxyMenu())
		}).
		AddItem("  ⏱   Request Delay", "Anti-ban delay (0-N ms jitter)", 'd', func() {
			pushPage("sub", delayMenu())
		}).
		AddItem("  🔄  PAT Strategy", "round-robin or random rotation", 's', func() {
			pushPage("sub", strategyMenu())
		}).
		AddItem("  📦  Import from .env", "Auto-detect & migrate .env → DB", 'i', func() {
			pushPage("sub", importMenu())
		}).
		AddItem("  📊  Usage", "Token & credit usage by period", 'u', func() {
			pushPage("sub", usageMenu())
		}).
		AddItem("  📜  Logs", "Request log viewer", 'l', func() {
			pushPage("sub", logsMenu())
		}).
		AddItem("  📋  Show All Config", "Summary of all settings", 'v', func() {
			pushPage("sub", showAllView())
		}).
		AddItem("  🔄  Update Bridge", "Pull latest, rebuild, restart", 'x', func() {
			pushPage("sub", updateView())
		}).
		AddItem("  🔁  Restart Bridge", "Stop and restart the daemon", 'r', func() {
			pushPage("sub", restartView())
		}).
		AddItem("  🚪  Exit", "Quit (q / Esc)", 'q', func() { app.Stop() })

	list.SetBorder(true)
	list.SetBorderColor(tcell.ColorNavy)
	list.SetTitle(colorTitle + " qoder-bridge config ")
	list.SetTitleAlign(tview.AlignCenter)

	// Esc on list = exit
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			app.Stop()
			return nil
		}
		return event
	})

	return list
}

// ── API Keys ──────────────────────────────────────────────────────────────

func apiKeyMenu() tview.Primitive {
	key := cfgGet("api_key")
	enabled := cfgBool("api_key_enabled", false)

	status := colorRed + "disabled"
	if enabled {
		status = colorGreen + "enabled"
	}
	masked := "(not set)"
	if key != "" {
		masked = key[:min(8, len(key))] + "..." + key[max(0, len(key)-4):]
	}

	list := tview.NewList().
		AddItem("  Generate New Key", "Create random sk-* key + auto-enable", 'g', func() {
			b := make([]byte, 24)
			rand.Read(b)
			newKey := "sk-" + hex.EncodeToString(b)
			cfgSet("api_key", newKey)
			cfgSet("api_key_enabled", "1")
			showMsg(fmt.Sprintf("%s✅ Generated & enabled:%s\n%s", colorGreen, colorReset, newKey), "apikey", apiKeyMenu)
		}).
		AddItem("  Toggle On/Off", fmt.Sprintf("Currently: %s%s", status, colorReset), 't', func() {
			if enabled {
				cfgSet("api_key_enabled", "0")
			} else {
				cfgSet("api_key_enabled", "1")
			}
			// Rebuild so status text updates
			pages.RemovePage("sub")
			pushPage("sub", apiKeyMenu())
		}).
		AddItem("  View Current Key", masked, 'v', func() {
			if key == "" {
				showMsg("No API key configured.", "apikey", apiKeyMenu)
			} else {
				showMsg(fmt.Sprintf("Key: %s\nStatus: %s%s", key, status, colorReset), "apikey", apiKeyMenu)
			}
		}).
		AddItem("  Clear Key", "Remove key entirely", 'x', func() {
			cfgSet("api_key", "")
			cfgSet("api_key_enabled", "0")
			pages.RemovePage("sub")
			pushPage("sub", apiKeyMenu())
		}).
		AddItem("  ← Back", "Esc to go back", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " API Keys ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── Proxy ─────────────────────────────────────────────────────────────────

func proxyMenu() tview.Primitive {
	cur := cfgGet("proxy")
	if cur == "" {
		cur = colorDim + "(direct connection)"
	}

	list := tview.NewList().
		AddItem("  Set Proxy", "Enter proxy URL", 's', func() {
			showInput("Proxy URL", "socks5://user:pass@host:port", func(val string) {
				cfgSet("proxy", val)
				showMsg(fmt.Sprintf("✅ Proxy set: %s\nRestart bridge to apply.", val), "proxy", proxyMenu)
			})
		}).
		AddItem("  View Current", cur, 'v', func() {
			showMsg("Proxy: "+cfgGet("proxy"), "proxy", proxyMenu)
		}).
		AddItem("  Clear", "Remove proxy", 'x', func() {
			cfgSet("proxy", "")
			showMsg("🗑 Proxy removed. Restart bridge to apply.", "proxy", proxyMenu)
		}).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Proxy ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── Delay ─────────────────────────────────────────────────────────────────

func delayMenu() tview.Primitive {
	cur := cfgGet("request_delay_ms")
	display := cur + " ms"
	if cur == "" {
		display = colorDim + "disabled"
	}

	list := tview.NewList().
		AddItem("  Set Delay", "Enter ms value", 's', func() {
			showInput("Delay (ms)", "1000", func(val string) {
				cfgSet("request_delay_ms", val)
				showMsg(fmt.Sprintf("✅ Delay set: %s ms", val), "delay", delayMenu)
			})
		}).
		AddItem("  View Current", display, 'v', func() {
			showMsg("Delay: "+cur, "delay", delayMenu)
		}).
		AddItem("  Disable", "No delay", 'x', func() {
			cfgSet("request_delay_ms", "")
			showMsg("✅ Delay disabled.", "delay", delayMenu)
		}).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Request Delay ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── PAT Strategy ──────────────────────────────────────────────────────────

func strategyMenu() tview.Primitive {
	cur := cfgGet("pat_strategy")
	if cur == "" {
		cur = "round-robin"
	}

	rr := "  Round-Robin"
	rn := "  Random"
	if cur == "round-robin" {
		rr = colorGreen + "  ✓ Round-Robin" + colorReset
	} else {
		rn = colorGreen + "  ✓ Random" + colorReset
	}

	list := tview.NewList().
		AddItem(rr, "Cycle PATs in order", 'r', func() {
			cfgSet("pat_strategy", "round-robin")
			// Rebuild menu so ✓ updates immediately
			pages.RemovePage("sub")
			pushPage("sub", strategyMenu())
		}).
		AddItem(rn, "Random PAT each request", 'd', func() {
			cfgSet("pat_strategy", "random")
			pages.RemovePage("sub")
			pushPage("sub", strategyMenu())
		}).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " PAT Strategy ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── Import from .env ──────────────────────────────────────────────────────

func importMenu() tview.Primitive {
	// Scan .env for values
	apiKey := ""
	delay := ""
	proxy := ""
	strategy := ""

	if envData != nil {
		apiKey = envData.apiKey
		if envData.requestDelay > 0 {
			delay = fmt.Sprintf("%d", envData.requestDelay)
		}
		strategy = envData.strategy
	}
	proxy = os.Getenv("QODER_PROXY")

	list := tview.NewList().
		AddItem("  Import ALL from .env", "Migrate all detected values to DB", 'a', func() {
			count := 0
			if apiKey != "" && cfgGet("api_key") == "" {
				cfgSet("api_key", apiKey)
				cfgSet("api_key_enabled", "1")
				count++
			}
			if delay != "" && cfgGet("request_delay_ms") == "" {
				cfgSet("request_delay_ms", delay)
				count++
			}
			if strategy != "" && cfgGet("pat_strategy") == "" {
				cfgSet("pat_strategy", strategy)
				count++
			}
			if proxy != "" && cfgGet("proxy") == "" {
				cfgSet("proxy", proxy)
				count++
			}
			showMsg(fmt.Sprintf("✅ Imported %d value(s) from .env to DB.\nDB is now source of truth.", count), "import", importMenu)
		})

	// Show detected values
	if apiKey != "" {
		masked := apiKey[:min(8, len(apiKey))] + "..."
		status := colorDim + "empty"
		if cfgGet("api_key") != "" {
			status = colorGreen + "already in DB"
		}
		list.AddItem(fmt.Sprintf("  API Key: %s", masked), status, 'k', nil)
	} else {
		list.AddItem("  API Key: (not in .env)", "", 'k', nil)
	}

	if delay != "" {
		status := colorDim + "empty"
		if cfgGet("request_delay_ms") != "" {
			status = colorGreen + "already in DB"
		}
		list.AddItem(fmt.Sprintf("  Delay: %s ms", delay), status, 'd', nil)
	}

	if strategy != "" {
		status := colorDim + "empty"
		if cfgGet("pat_strategy") != "" {
			status = colorGreen + "already in DB"
		}
		list.AddItem(fmt.Sprintf("  Strategy: %s", strategy), status, 's', nil)
	}

	if proxy != "" {
		masked := proxy
		if len(proxy) > 30 {
			masked = proxy[:30] + "..."
		}
		status := colorDim + "empty"
		if cfgGet("proxy") != "" {
			status = colorGreen + "already in DB"
		}
		list.AddItem(fmt.Sprintf("  Proxy: %s", masked), status, 'p', nil)
	}

	list.AddItem("  ← Back", "Esc", 'b', func() { goBack() })
	list.SetBorder(true).SetTitle(colorTitle + " Import from .env ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── Usage ─────────────────────────────────────────────────────────────────

func usageMenu() tview.Primitive {
	list := tview.NewList().
		AddItem("  Today", "Since midnight", 't', func() { showUsage("today") }).
		AddItem("  This Week", "Past 7 days", 'w', func() { showUsage("week") }).
		AddItem("  This Month", "Past 30 days", 'm', func() { showUsage("month") }).
		AddItem("  This Year", "Past 365 days", 'y', func() { showUsage("year") }).
		AddItem("  Custom Range", "DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			showDateInput(func(from, to string) { showUsage("custom", from, to) })
		}).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Usage ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

func showUsage(period string, dates ...string) {
	var args []string
	if period == "custom" && len(dates) >= 2 {
		args = []string{"custom", dates[0], dates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		showMsg(fmt.Sprintf("Error: %v", err), "usage", usageMenu)
		return
	}
	rows, err := queryUsage(fromTS, toTS)
	if err != nil {
		showMsg(fmt.Sprintf("Error: %v", err), "usage", usageMenu)
		return
	}

	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	t.SetBorder(true).SetTitle(fmt.Sprintf(" %sUsage: %s %s ", colorTitle, label, colorReset)).SetTitleAlign(tview.AlignCenter)

	for i, h := range []string{"PAT", "Model", "Requests", "Tokens", "Credits"} {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}
	if len(rows) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No data for this period.").SetSelectable(false))
	} else {
		for i, r := range rows {
			t.SetCell(i+1, 0, tview.NewTableCell(r.PAT).SetExpansion(1))
			t.SetCell(i+1, 1, tview.NewTableCell(r.Model).SetExpansion(1))
			t.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d", r.Requests)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 3, tview.NewTableCell(fmt.Sprintf("%d", r.Tokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
		}
	}

	pushPage("sub", wrapTable(t))
}

// ── Logs ──────────────────────────────────────────────────────────────────

func logsMenu() tview.Primitive {
	list := tview.NewList().
		AddItem("  Today", "Since midnight", 't', func() { showLogs("today") }).
		AddItem("  This Week", "Past 7 days", 'w', func() { showLogs("week") }).
		AddItem("  This Month", "Past 30 days", 'm', func() { showLogs("month") }).
		AddItem("  Custom Range", "DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			showDateInput(func(from, to string) { showLogs("custom", from, to) })
		}).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Request Logs ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

func showLogs(period string, dates ...string) {
	var args []string
	if period == "custom" && len(dates) >= 2 {
		args = []string{"custom", dates[0], dates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		showMsg(fmt.Sprintf("Error: %v", err), "logs", logsMenu)
		return
	}
	logRows, err := queryLogs(fromTS, toTS)
	if err != nil {
		showMsg(fmt.Sprintf("Error: %v", err), "logs", logsMenu)
		return
	}

	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	t.SetBorder(true).SetTitle(fmt.Sprintf(" %sLogs: %s %s ", colorTitle, label, colorReset)).SetTitleAlign(tview.AlignCenter)

	for i, h := range []string{"Time (WIB)", "PAT", "Model", "Tokens", "Credits", "Status", "Latency"} {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}
	if len(logRows) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No logs for this period.").SetSelectable(false))
	} else {
		for i, r := range logRows {
			sc := tcell.ColorGreen
			if r.Status != 200 {
				sc = tcell.ColorRed
			}
			t.SetCell(i+1, 0, tview.NewTableCell(formatTS(r.TS)).SetExpansion(2))
			t.SetCell(i+1, 1, tview.NewTableCell(r.PAT).SetExpansion(1))
			t.SetCell(i+1, 2, tview.NewTableCell(r.Model).SetExpansion(1))
			t.SetCell(i+1, 3, tview.NewTableCell(fmt.Sprintf("%d", r.TotalTokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 5, tview.NewTableCell(fmt.Sprintf("%d", r.Status)).SetTextColor(sc).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 6, tview.NewTableCell(fmt.Sprintf("%dms", r.LatencyMs)).SetAlign(tview.AlignRight).SetExpansion(1))
		}
	}

	pushPage("sub", wrapTable(t))
}

// ── Show All Config ───────────────────────────────────────────────────────

func showAllView() tview.Primitive {
	key := cfgGet("api_key")
	enabled := cfgBool("api_key_enabled", false)
	delay := cfgGet("request_delay_ms")
	domain := cfgGet("domain")
	proxy := cfgGet("proxy")
	strategy := cfgGet("pat_strategy")
	if strategy == "" {
		strategy = "round-robin"
	}

	masked := "(not set)"
	if key != "" {
		masked = key[:min(8, len(key))] + "..."
	}
	status := colorRed + "disabled"
	if enabled {
		status = colorGreen + "enabled"
	}
	if delay == "" {
		delay = "off"
	}
	if domain == "" {
		domain = "(not set)"
	}
	if proxy == "" {
		proxy = "(direct)"
	}

	list := tview.NewList().
		AddItem(fmt.Sprintf("  API Key:      %s", masked), fmt.Sprintf("  Status: %s%s", status, colorReset), 0, nil).
		AddItem(fmt.Sprintf("  Delay:        %s ms", delay), "", 0, nil).
		AddItem(fmt.Sprintf("  Domain:       %s", domain), "", 0, nil).
		AddItem(fmt.Sprintf("  Proxy:        %s", proxy), "", 0, nil).
		AddItem(fmt.Sprintf("  PAT Strategy: %s", strategy), "", 0, nil).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " All Config ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return list
}

// ── Update ────────────────────────────────────────────────────────────────

func updateView() tview.Primitive {
	text := tview.NewTextView().SetDynamicColors(true)
	text.SetBorder(true).SetTitle(colorTitle + " Update Bridge ").SetTitleAlign(tview.AlignCenter)
	text.SetText(fmt.Sprintf("\n  %sThis will run:%s\n\n  %scd <bridge-dir> && git pull && go build -o qoder-bridge .%s\n\n  %sAfter build, restart the bridge:%s\n  %s./qoder-bridge stop && ./qoder-bridge%s\n\n  %sPress Enter to update, Esc to cancel.%s",
		colorKey, colorReset,
		colorAccent, colorReset,
		colorKey, colorReset,
		colorAccent, colorReset,
		colorDim, colorReset))

	text.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			goBack()
			return nil
		case tcell.KeyEnter:
			// Run update
			result := tuiRunUpdate()
			pages.RemovePage("sub")
			pushPage("sub", updateResult(result))
			return nil
		}
		return event
	})

	return text
}

func tuiRunUpdate() string {
	var sb strings.Builder

	// Find project directory (same logic as runUpdate CLI)
	projDir := ""
	for _, d := range []string{
		filepath.Join(os.Getenv("HOME"), "projects", "qoder-bridge"),
		".",
	} {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			projDir = d
			break
		}
	}
	if projDir == "" {
		return "Error: cannot find qoder-bridge git repo.\nClone to ~/projects/qoder-bridge or run from the project directory."
	}

	sb.WriteString(fmt.Sprintf("Updating from %s...\n", projDir))

	// git pull — use CombinedOutput (not runCmd, which sends to raw terminal)
	out, err := exec.Command("git", "pull").Output()
	if err != nil {
		sb.WriteString(fmt.Sprintf("git pull FAILED: %v\n%s\n", err, string(out)))
		return sb.String()
	}
	sb.WriteString("git pull... ok\n")

	// go build
	out, err = exec.Command("go", "build", "-o", "qoder-bridge", ".").CombinedOutput()
	if err != nil {
		sb.WriteString(fmt.Sprintf("build FAILED: %v\n%s\n", err, string(out)))
		return sb.String()
	}
	sb.WriteString("build... ok\n")

	// Stop running daemon BEFORE copying (binary in use on Linux can be replaced,
	// but stop first so restart picks up new binary)
	sb.WriteString("stopping daemon...\n")
	stopOut, _ := exec.Command(bridgeExe(), "stop").CombinedOutput()
	sb.WriteString(fmt.Sprintf("%s\n", strings.TrimSpace(string(stopOut))))
	time.Sleep(500 * time.Millisecond)

	// Copy binary — find actual install location
	dest := ""
	candidates := []string{
		filepath.Join(os.Getenv("HOME"), ".local", "bin", "qoder-bridge"),
		filepath.Join("/usr", "local", "bin", "qoder-bridge"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			dest = c
			break
		}
	}
	if dest == "" {
		dest = filepath.Join(os.Getenv("HOME"), ".local", "bin", "qoder-bridge")
		os.MkdirAll(filepath.Dir(dest), 0755)
	}

	exe := filepath.Join(projDir, "qoder-bridge")
	cpOut, err := exec.Command("cp", exe, dest).CombinedOutput()
	if err != nil {
		sb.WriteString(fmt.Sprintf("install FAILED: %v\n%s\n", err, string(cpOut)))
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("installed to %s\n", dest))

	// Restart
	sb.WriteString("restarting...\n")
	sb.WriteString(tuiDoRestart())
	return sb.String()
}

func tuiDoRestart() string {
	var sb strings.Builder
	exe := bridgeExe()

	// Stop via subprocess (don't call runStop — it calls os.Exit)
	cmd := exec.Command(exe, "stop")
	out, _ := cmd.CombinedOutput()
	sb.WriteString(fmt.Sprintf("stop: %s\n", strings.TrimSpace(string(out))))

	// Small wait for port release
	time.Sleep(500 * time.Millisecond)

	// Start via subprocess
	cmd = exec.Command(exe)
	cmd.Dir = bridgeDir()
	out, _ = cmd.CombinedOutput()
	sb.WriteString(fmt.Sprintf("start: %s\n", strings.TrimSpace(string(out))))

	// Wait for startup
	time.Sleep(3 * time.Second)
	sb.WriteString("✅ Bridge restarted!\n")
	return sb.String()
}

func bridgeDir() string {
	if ex, err := os.Executable(); err == nil {
		return strings.TrimSuffix(ex, "/qoder-bridge")
	}
	return "."
}

func bridgeExe() string {
	if ex, err := os.Executable(); err == nil {
		return ex
	}
	return "./qoder-bridge"
}

func restartView() tview.Primitive {
	text := tview.NewTextView().SetDynamicColors(true)
	text.SetBorder(true).SetTitle(colorTitle + " Restart Bridge ").SetTitleAlign(tview.AlignCenter)
	text.SetText(fmt.Sprintf("\n  %sThis will:%s\n\n  %s1. Stop the running bridge daemon%s\n  %s2. Start it again with current .env + DB config%s\n\n  %sPress Enter to restart, Esc to cancel.%s",
		colorKey, colorReset,
		colorAccent, colorReset,
		colorAccent, colorReset,
		colorDim, colorReset))

	text.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			goBack()
			return nil
		case tcell.KeyEnter:
			result := tuiDoRestart()
			pages.RemovePage("sub")
			pushPage("sub", updateResult(result))
			return nil
		}
		return event
	})
	return text
}

func updateResult(result string) tview.Primitive {
	text := tview.NewTextView().SetDynamicColors(true).SetText(result)
	text.SetBorder(true).SetTitle(colorTitle + " Update Result ").SetTitleAlign(tview.AlignCenter)
	text.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			goBack()
			return nil
		}
		return event
	})
	return text
}

// ── Helpers ───────────────────────────────────────────────────────────────

// wireEsc adds Esc → goBack on a List or Table.
func wireEsc(p tview.Primitive) {
	if l, ok := p.(*tview.List); ok {
		l.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyEscape {
				goBack()
				return nil
			}
			return event
		})
	}
	if t, ok := p.(*tview.Table); ok {
		t.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyEscape {
				goBack()
				return nil
			}
			return event
		})
	}
}

// wrapTable wraps a table with a footer showing key hints.
func wrapTable(t *tview.Table) tview.Primitive {
	wireEsc(t)
	hint := tview.NewTextView().SetDynamicColors(true).
		SetText(fmt.Sprintf("  %s↑↓%s navigate   %sEsc%s go back", colorKey, colorReset, colorKey, colorReset))
	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t, 0, 1, true).
		AddItem(hint, 1, 0, false)
	// Wire Esc on the flex too (in case table doesn't get focus)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			goBack()
			return nil
		}
		return event
	})
	return flex
}

// showMsg displays a modal message, then returns to the named menu.
func showMsg(msg string, menuName string, menuBuilder func() tview.Primitive) {
	modal := tview.NewModal().
		SetText(msg).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(_ int, _ string) {
			pages.RemovePage("modal")
		})
	modal.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.RemovePage("modal")
			return nil
		}
		return event
	})
	pages.AddAndSwitchToPage("modal", modal, true)
}

// showInput shows an input form.
func showInput(label, placeholder string, onSubmit func(string)) {
	inp := tview.NewInputField().SetLabel(label).SetPlaceholder(placeholder).SetFieldWidth(50)
	form := tview.NewForm().
		AddFormItem(inp).
		AddButton("Submit", func() {
			v := strings.TrimSpace(inp.GetText())
			if v != "" {
				pages.RemovePage("form")
				onSubmit(v)
			}
		}).
		AddButton("Cancel", func() {
			pages.RemovePage("form")
		})
	form.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", label)).SetTitleAlign(tview.AlignCenter)
	form.SetCancelFunc(func() { pages.RemovePage("form") })
	pages.AddAndSwitchToPage("form", form, true)
}

// showDateInput shows a date range input.
func showDateInput(onSubmit func(from, to string)) {
	fromF := tview.NewInputField().SetLabel("From (DD-MM-YYYY) ").SetFieldWidth(20)
	toF := tview.NewInputField().SetLabel("  To (DD-MM-YYYY) ").SetFieldWidth(20)
	form := tview.NewForm().
		AddFormItem(fromF).
		AddFormItem(toF).
		AddButton("Submit", func() {
			from := strings.TrimSpace(fromF.GetText())
			to := strings.TrimSpace(toF.GetText())
			if from != "" && to != "" {
				pages.RemovePage("form")
				onSubmit(from, to)
			}
		}).
		AddButton("Cancel", func() {
			pages.RemovePage("form")
		})
	form.SetBorder(true).SetTitle(" Date Range ").SetTitleAlign(tview.AlignCenter)
	form.SetCancelFunc(func() { pages.RemovePage("form") })
	pages.AddAndSwitchToPage("form", form, true)
}
