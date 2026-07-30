// tui.go — Interactive TUI config menu using tview.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	tuiApp  *tview.Application
	tuiPages *tview.Pages
)

// runConfigTUI launches the interactive config TUI.
func runConfigTUI() {
	tuiApp = tview.NewApplication()
	tuiPages = tview.NewPages()

	// Global Esc handler — ALWAYS works regardless of focused widget
	tuiApp.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			tuiGoBack()
			return nil
		}
		return event
	})

	mainMenu := tuiMainMenu()
	tuiPages.AddPage("main", mainMenu, true, true)

	tuiApp.SetRoot(tuiPages, true)
	if err := tuiApp.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
		os.Exit(1)
	}
}

// tuiGoBack pops the current page back to main.
func tuiGoBack() {
	name, _ := tuiPages.GetFrontPage()
	if name == "main" {
		tuiApp.Stop()
		return
	}
	tuiPages.RemovePage(name)
}

// tuiPush switches to a named sub-page. Esc will pop it.
func tuiPush(name string, p tview.Primitive) {
	tuiPages.AddAndSwitchToPage(name, p, true)
}

// ── Main Menu ─────────────────────────────────────────────────────────────

func tuiMainMenu() *tview.List {
	l := tview.NewList().
		AddItem("🔑  API Keys", "Generate, enable/disable, view keys", 'a', func() {
			tuiPush("apikey", tuiAPIKeyMenu())
		}).
		AddItem("🌐  Proxy", "Set, view, or remove proxy", 'p', func() {
			tuiPush("proxy", tuiProxyMenu())
		}).
		AddItem("⏱   Request Delay", "Anti-ban delay between requests", 'd', func() {
			tuiPush("delay", tuiDelayMenu())
		}).
		AddItem("🔄  PAT Strategy", "round-robin or random selection", 's', func() {
			tuiPush("strategy", tuiStrategyMenu())
		}).
		AddItem("📊  Usage", "Token & credit usage by period", 'u', func() {
			tuiPush("usage", tuiUsageMenu())
		}).
		AddItem("📜  Logs", "Request logs with date filter", 'l', func() {
			tuiPush("logs", tuiLogsMenu())
		}).
		AddItem("📋  Show All Config", "View all current settings", 'v', func() {
			tuiPush("showall", tuiShowAll())
		}).
		AddItem("🚪  Exit", "Quit config (q/Esc)", 'q', func() {
			tuiApp.Stop()
		})
	l.SetBorder(true).SetTitle(" qoder-bridge config ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── API Keys ──────────────────────────────────────────────────────────────

func tuiAPIKeyMenu() *tview.List {
	key := cfgGet("api_key")
	enabled := cfgBool("api_key_enabled", false)

	status := "[red]disabled"
	if enabled {
		status = "[green]enabled"
	}
	masked := "(not set)"
	if key != "" {
		masked = key[:min(8, len(key))] + "..." + key[max(0, len(key)-4):]
	}

	l := tview.NewList().
		AddItem("Generate New Key", "Create sk-* key + auto-enable", 'g', func() {
			b := make([]byte, 24)
			rand.Read(b)
			newKey := "sk-" + hex.EncodeToString(b)
			cfgSet("api_key", newKey)
			cfgSet("api_key_enabled", "1")
			tuiModal(fmt.Sprintf("✅ Generated & enabled:\n%s", newKey))
		}).
		AddItem("Toggle On/Off", fmt.Sprintf("Currently: %s", status), 't', func() {
			if enabled {
				cfgSet("api_key_enabled", "0")
			} else {
				cfgSet("api_key_enabled", "1")
			}
			tuiModal("✅ Toggled! Re-open menu to see new status.")
		}).
		AddItem("View Current Key", masked, 'v', func() {
			if key == "" {
				tuiModal("No API key configured.")
			} else {
				tuiModal(fmt.Sprintf("Key: %s\nStatus: %s", key, status))
			}
		}).
		AddItem("🗑  Clear Key", "Remove key entirely", 'x', func() {
			cfgSet("api_key", "")
			cfgSet("api_key_enabled", "0")
			tuiModal("API key removed.")
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" API Keys ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── Proxy ─────────────────────────────────────────────────────────────────

func tuiProxyMenu() *tview.List {
	current := cfgGet("proxy")
	if current == "" {
		current = "(direct connection)"
	}

	l := tview.NewList().
		AddItem("Set Proxy", "Enter proxy URL", 's', func() {
			tuiInput("Proxy URL", "socks5://user:pass@host:port", func(val string) {
				cfgSet("proxy", val)
				tuiModal(fmt.Sprintf("✅ Proxy set: %s\nRestart bridge to apply.", val))
			})
		}).
		AddItem("View Current", current, 'v', func() {
			tuiModal(fmt.Sprintf("Proxy: %s", current))
		}).
		AddItem("🗑  Clear", "Remove proxy", 'x', func() {
			cfgSet("proxy", "")
			tuiModal("Proxy removed. Restart bridge to apply.")
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" Proxy ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── Request Delay ─────────────────────────────────────────────────────────

func tuiDelayMenu() *tview.List {
	current := cfgGet("request_delay_ms")
	display := current + " ms"
	if current == "" {
		display = "disabled"
	}

	l := tview.NewList().
		AddItem("Set Delay", "Enter delay in ms", 's', func() {
			tuiInput("Delay (ms)", "1000", func(val string) {
				cfgSet("request_delay_ms", val)
				tuiModal(fmt.Sprintf("✅ Delay set: %s ms", val))
			})
		}).
		AddItem("View Current", display, 'v', func() {
			tuiModal(fmt.Sprintf("Delay: %s", display))
		}).
		AddItem("Disable", "No delay", 'x', func() {
			cfgSet("request_delay_ms", "")
			tuiModal("Delay disabled.")
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" Request Delay ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── PAT Strategy ──────────────────────────────────────────────────────────

func tuiStrategyMenu() *tview.List {
	current := cfgGet("pat_strategy")
	if current == "" {
		current = "round-robin"
	}

	l := tview.NewList().
		AddItem("Round-Robin"+mark(current, "round-robin"), "Cycle PATs in order", 'r', func() {
			cfgSet("pat_strategy", "round-robin")
			tuiModal("✅ Strategy: round-robin")
		}).
		AddItem("Random"+mark(current, "random"), "Random PAT each request", 'd', func() {
			cfgSet("pat_strategy", "random")
			tuiModal("✅ Strategy: random")
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" PAT Strategy ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── Usage ─────────────────────────────────────────────────────────────────

func tuiUsageMenu() *tview.List {
	l := tview.NewList().
		AddItem("Today", "Since midnight", 't', func() { tuiShowUsage("today") }).
		AddItem("This Week", "Past 7 days", 'w', func() { tuiShowUsage("week") }).
		AddItem("This Month", "Past 30 days", 'm', func() { tuiShowUsage("month") }).
		AddItem("This Year", "Past 365 days", 'y', func() { tuiShowUsage("year") }).
		AddItem("Custom Range", "DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			tuiDateRange(func(from, to string) { tuiShowUsage("custom", from, to) })
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" Usage ").SetTitleAlign(tview.AlignCenter)
	return l
}

func tuiShowUsage(period string, dates ...string) {
	var args []string
	if period == "custom" && len(dates) >= 2 {
		args = []string{"custom", dates[0], dates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		tuiModal(fmt.Sprintf("Error: %v", err))
		return
	}
	rows, err := queryUsage(fromTS, toTS)
	if err != nil {
		tuiModal(fmt.Sprintf("Error: %v", err))
		return
	}

	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	t.SetBorder(true).SetTitle(fmt.Sprintf(" Usage: %s ", label)).SetTitleAlign(tview.AlignCenter)

	for i, h := range []string{"PAT", "Model", "Requests", "Tokens", "Credits"} {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}
	if len(rows) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No data").SetSelectable(false))
	} else {
		for i, r := range rows {
			t.SetCell(i+1, 0, tview.NewTableCell(r.PAT).SetExpansion(1))
			t.SetCell(i+1, 1, tview.NewTableCell(r.Model).SetExpansion(1))
			t.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d", r.Requests)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 3, tview.NewTableCell(fmt.Sprintf("%d", r.Tokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(i+1, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
		}
	}

	tuiPush("table", tuiWrapTable(t))
}

// ── Logs ──────────────────────────────────────────────────────────────────

func tuiLogsMenu() *tview.List {
	l := tview.NewList().
		AddItem("Today", "Since midnight", 't', func() { tuiShowLogs("today") }).
		AddItem("This Week", "Past 7 days", 'w', func() { tuiShowLogs("week") }).
		AddItem("This Month", "Past 30 days", 'm', func() { tuiShowLogs("month") }).
		AddItem("Custom Range", "DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			tuiDateRange(func(from, to string) { tuiShowLogs("custom", from, to) })
		}).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" Request Logs ").SetTitleAlign(tview.AlignCenter)
	return l
}

func tuiShowLogs(period string, dates ...string) {
	var args []string
	if period == "custom" && len(dates) >= 2 {
		args = []string{"custom", dates[0], dates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		tuiModal(fmt.Sprintf("Error: %v", err))
		return
	}
	logRows, err := queryLogs(fromTS, toTS)
	if err != nil {
		tuiModal(fmt.Sprintf("Error: %v", err))
		return
	}

	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	t.SetBorder(true).SetTitle(fmt.Sprintf(" Logs: %s ", label)).SetTitleAlign(tview.AlignCenter)

	for i, h := range []string{"Time (WIB)", "PAT", "Model", "Tokens", "Credits", "Status", "Latency"} {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}
	if len(logRows) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No logs").SetSelectable(false))
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

	tuiPush("table", tuiWrapTable(t))
}

// ── Show All ──────────────────────────────────────────────────────────────

func tuiShowAll() *tview.List {
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
	status := "disabled"
	if enabled {
		status = "enabled"
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

	l := tview.NewList().
		AddItem(fmt.Sprintf("API Key:      %s", masked), fmt.Sprintf("Status: %s", status), 'k', nil).
		AddItem(fmt.Sprintf("Delay:        %s ms", delay), "", 'd', nil).
		AddItem(fmt.Sprintf("Domain:       %s", domain), "", 'm', nil).
		AddItem(fmt.Sprintf("Proxy:        %s", proxy), "", 'p', nil).
		AddItem(fmt.Sprintf("PAT Strategy: %s", strategy), "", 's', nil).
		AddItem("← Back", "Esc to go back", 'b', func() { tuiGoBack() })
	l.SetBorder(true).SetTitle(" All Config ").SetTitleAlign(tview.AlignCenter)
	return l
}

// ── Helpers ───────────────────────────────────────────────────────────────

func tuiModal(msg string) {
	m := tview.NewModal().
		SetText(msg).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(_ int, _ string) {
			tuiGoBack()
		})
	tuiPush("modal", m)
}

func tuiInput(label, placeholder string, onSubmit func(string)) {
	inp := tview.NewInputField().SetLabel(label).SetPlaceholder(placeholder).SetFieldWidth(40)
	form := tview.NewForm().
		AddFormItem(inp).
		AddButton("Submit", func() {
			v := strings.TrimSpace(inp.GetText())
			if v != "" {
				tuiGoBack()
				onSubmit(v)
			}
		}).
		AddButton("Cancel", func() { tuiGoBack() })
	form.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", label)).SetTitleAlign(tview.AlignCenter)
	tuiPush("input", form)
}

func tuiDateRange(onSubmit func(from, to string)) {
	fromF := tview.NewInputField().SetLabel("From (DD-MM-YYYY)").SetFieldWidth(20)
	toF := tview.NewInputField().SetLabel("To (DD-MM-YYYY)").SetFieldWidth(20)
	form := tview.NewForm().
		AddFormItem(fromF).
		AddFormItem(toF).
		AddButton("Submit", func() {
			from := strings.TrimSpace(fromF.GetText())
			to := strings.TrimSpace(toF.GetText())
			if from != "" && to != "" {
				tuiGoBack()
				onSubmit(from, to)
			}
		}).
		AddButton("Cancel", func() { tuiGoBack() })
	form.SetBorder(true).SetTitle(" Date Range ").SetTitleAlign(tview.AlignCenter)
	tuiPush("input", form)
}

// tuiWrapTable wraps a table in a Flex with a footer hint.
func tuiWrapTable(t *tview.Table) *tview.Flex {
	hint := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetText("[yellow]↑↓[white] navigate  [yellow]Esc[white] go back")
	return tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t, 0, 1, true).
		AddItem(hint, 1, 0, false)
}

func mark(current, this string) string {
	if current == this {
		return " ✓"
	}
	return ""
}
