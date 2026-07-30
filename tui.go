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

// runConfigTUI launches the interactive config TUI.
func runConfigTUI() {
	app := tview.NewApplication()

	pages := tview.NewPages()
	mainMenu := buildMainMenu(app, pages)
	pages.AddPage("main", mainMenu, true, true)

	// Global Esc handler
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			front, _ := pages.GetFrontPage()
			if front != "main" {
				pages.SwitchToPage("main")
				return nil
			}
			app.Stop()
			return nil
		}
		return event
	})

	app.SetRoot(pages, true)
	app.SetFocus(mainMenu)
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
		os.Exit(1)
	}
}

func buildMainMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	list := tview.NewList().
		AddItem("🔑  API Keys", "Generate, enable/disable, view keys", 'a', func() {
			pages.AddAndSwitchToPage("sub", buildAPIKeyMenu(app, pages), true)
		}).
		AddItem("🌐  Proxy", "Set, view, or remove proxy URL", 'p', func() {
			pages.AddAndSwitchToPage("sub", buildProxyMenu(app, pages), true)
		}).
		AddItem("⏱  Request Delay", "Anti-ban delay between requests", 'd', func() {
			pages.AddAndSwitchToPage("sub", buildDelayMenu(app, pages), true)
		}).
		AddItem("🔄  PAT Strategy", "round-robin or random selection", 's', func() {
			pages.AddAndSwitchToPage("sub", buildStrategyMenu(app, pages), true)
		}).
		AddItem("📊  Usage", "Token & credit usage by period", 'u', func() {
			pages.AddAndSwitchToPage("sub", buildUsageMenu(app, pages), true)
		}).
		AddItem("📜  Logs", "Request logs with date filter", 'l', func() {
			pages.AddAndSwitchToPage("sub", buildLogsMenu(app, pages), true)
		}).
		AddItem("📋  Show All Config", "View all current settings", 'v', func() {
			pages.AddAndSwitchToPage("sub", buildShowAll(app, pages), true)
		}).
		AddItem("🚪  Exit", "Quit config", 'q', func() {
			app.Stop()
		})

	list.SetBorder(true).SetTitle(" qoder-bridge config ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── API Keys ──────────────────────────────────────────────────────────────

func buildAPIKeyMenu(app *tview.Application, pages *tview.Pages) *tview.List {
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

	list := tview.NewList().
		AddItem("Generate New Key", "Create a random sk-* key", 'g', func() {
			b := make([]byte, 24)
			rand.Read(b)
			newKey := "sk-" + hex.EncodeToString(b)
			cfgSet("api_key", newKey)
			cfgSet("api_key_enabled", "1")
			showModal(app, pages, fmt.Sprintf("✅ Generated & enabled:\n%s", newKey))
		}).
		AddItem("Toggle Enable/Disable", fmt.Sprintf("Currently: %s", status), 't', func() {
			if enabled {
				cfgSet("api_key_enabled", "0")
			} else {
				cfgSet("api_key_enabled", "1")
			}
			showModal(app, pages, "Toggled! Re-open to see new status.")
		}).
		AddItem("View Current Key", masked, 'v', func() {
			if key == "" {
				showModal(app, pages, "No API key configured.")
			} else {
				showModal(app, pages, fmt.Sprintf("Key: %s\nStatus: %s", key, status))
			}
		}).
		AddItem("🗑  Clear Key", "Remove API key entirely", 'x', func() {
			cfgSet("api_key", "")
			cfgSet("api_key_enabled", "0")
			showModal(app, pages, "API key removed.")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" API Keys ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── Proxy ─────────────────────────────────────────────────────────────────

func buildProxyMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	current := cfgGet("proxy")
	if current == "" {
		current = "(direct connection)"
	}

	list := tview.NewList().
		AddItem("Set Proxy", "Enter proxy URL (socks5://, http://)", 's', func() {
			showInput(app, pages, "Proxy URL", "socks5://user:pass@127.0.0.1:1080", func(val string) {
				cfgSet("proxy", val)
				showModal(app, pages, fmt.Sprintf("✅ Proxy set: %s\nRestart bridge to apply.", val))
			})
		}).
		AddItem("View Current", current, 'v', func() {
			showModal(app, pages, fmt.Sprintf("Proxy: %s", current))
		}).
		AddItem("🗑  Clear Proxy", "Remove proxy (direct connection)", 'x', func() {
			cfgSet("proxy", "")
			showModal(app, pages, "Proxy removed. Restart bridge to apply.")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" Proxy ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── Request Delay ─────────────────────────────────────────────────────────

func buildDelayMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	current := cfgGet("request_delay_ms")
	display := current + " ms"
	if current == "" {
		display = "disabled"
	}

	list := tview.NewList().
		AddItem("Set Delay", "Enter delay in milliseconds", 's', func() {
			showInput(app, pages, "Delay (ms)", "1000", func(val string) {
				cfgSet("request_delay_ms", val)
				showModal(app, pages, fmt.Sprintf("✅ Delay set: %s ms", val))
			})
		}).
		AddItem("View Current", display, 'v', func() {
			showModal(app, pages, fmt.Sprintf("Delay: %s", display))
		}).
		AddItem("Disable Delay", "Set to 0 (no delay)", 'x', func() {
			cfgSet("request_delay_ms", "")
			showModal(app, pages, "Delay disabled.")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" Request Delay ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── PAT Strategy ──────────────────────────────────────────────────────────

func buildStrategyMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	current := cfgGet("pat_strategy")
	if current == "" {
		current = "round-robin"
	}

	list := tview.NewList().
		AddItem(fmt.Sprintf("▸ Round-Robin %s", checkMark(current, "round-robin")), "Cycle through PATs in order", 'r', func() {
			cfgSet("pat_strategy", "round-robin")
			showModal(app, pages, "✅ Strategy: round-robin")
		}).
		AddItem(fmt.Sprintf("▸ Random %s", checkMark(current, "random")), "Pick random PAT each request", 'd', func() {
			cfgSet("pat_strategy", "random")
			showModal(app, pages, "✅ Strategy: random")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" PAT Strategy ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── Usage ─────────────────────────────────────────────────────────────────

func buildUsageMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	list := tview.NewList().
		AddItem("Today", "Usage since midnight", 't', func() {
			showUsageTable(app, pages, "today")
		}).
		AddItem("This Week", "Past 7 days", 'w', func() {
			showUsageTable(app, pages, "week")
		}).
		AddItem("This Month", "Past 30 days", 'm', func() {
			showUsageTable(app, pages, "month")
		}).
		AddItem("This Year", "Past 365 days", 'y', func() {
			showUsageTable(app, pages, "year")
		}).
		AddItem("Custom Date Range", "Enter DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			showDateRangeInput(app, pages, "usage")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" Token & Credit Usage ").SetTitleAlign(tview.AlignCenter)
	return list
}

func showUsageTable(app *tview.Application, pages *tview.Pages, period string, customDates ...string) {
	var args []string
	if period == "custom" && len(customDates) >= 2 {
		args = []string{"custom", customDates[0], customDates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		showModal(app, pages, fmt.Sprintf("Error: %v", err))
		return
	}
	rows, err := queryUsage(fromTS, toTS)
	if err != nil {
		showModal(app, pages, fmt.Sprintf("Error: %v", err))
		return
	}

	table := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	table.SetBorder(true).SetTitle(fmt.Sprintf(" Usage: %s ", label)).SetTitleAlign(tview.AlignCenter)

	// Header
	headers := []string{"PAT", "Model", "Requests", "Tokens", "Credits"}
	for i, h := range headers {
		cell := tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1)
		if i == 0 {
			cell.SetAlign(tview.AlignLeft)
		}
		table.SetCell(0, i, cell)
	}

	if len(rows) == 0 {
		table.SetCell(1, 0, tview.NewTableCell("No data for this period").SetSelectable(false))
	} else {
		for i, r := range rows {
			table.SetCell(i+1, 0, tview.NewTableCell(r.PAT).SetExpansion(1))
			table.SetCell(i+1, 1, tview.NewTableCell(r.Model).SetExpansion(1))
			table.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d", r.Requests)).SetAlign(tview.AlignRight).SetExpansion(1))
			table.SetCell(i+1, 3, tview.NewTableCell(fmt.Sprintf("%d", r.Tokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			table.SetCell(i+1, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
		}
	}

	// Esc goes back
	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.SwitchToPage("main")
			return nil
		}
		return event
	})

	pages.AddAndSwitchToPage("sub", table, true)
}

// ── Logs ──────────────────────────────────────────────────────────────────

func buildLogsMenu(app *tview.Application, pages *tview.Pages) *tview.List {
	list := tview.NewList().
		AddItem("Today", "Logs since midnight", 't', func() {
			showLogsTable(app, pages, "today")
		}).
		AddItem("This Week", "Past 7 days", 'w', func() {
			showLogsTable(app, pages, "week")
		}).
		AddItem("This Month", "Past 30 days", 'm', func() {
			showLogsTable(app, pages, "month")
		}).
		AddItem("Custom Date Range", "Enter DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			showDateRangeInput(app, pages, "logs")
		}).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" Request Logs ").SetTitleAlign(tview.AlignCenter)
	return list
}

func showLogsTable(app *tview.Application, pages *tview.Pages, period string, customDates ...string) {
	var args []string
	if period == "custom" && len(customDates) >= 2 {
		args = []string{"custom", customDates[0], customDates[1]}
	} else {
		args = []string{period}
	}
	fromTS, toTS, label, err := parseDateRange(args)
	if err != nil {
		showModal(app, pages, fmt.Sprintf("Error: %v", err))
		return
	}

	logRows, err := queryLogs(fromTS, toTS)
	if err != nil {
		showModal(app, pages, fmt.Sprintf("Error: %v", err))
		return
	}

	table := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	table.SetBorder(true).SetTitle(fmt.Sprintf(" Logs: %s (last 200) ", label)).SetTitleAlign(tview.AlignCenter)

	headers := []string{"Time (WIB)", "PAT", "Model", "Tokens", "Credits", "Status", "Latency"}
	for i, h := range headers {
		table.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}

	if len(logRows) == 0 {
		table.SetCell(1, 0, tview.NewTableCell("No logs for this period").SetSelectable(false))
	} else {
		for i, r := range logRows {
			statusColor := tcell.ColorGreen
			if r.Status != 200 {
				statusColor = tcell.ColorRed
			}
			table.SetCell(i+1, 0, tview.NewTableCell(formatTS(r.TS)).SetExpansion(1))
			table.SetCell(i+1, 1, tview.NewTableCell(r.PAT).SetExpansion(1))
			table.SetCell(i+1, 2, tview.NewTableCell(r.Model).SetExpansion(1))
			table.SetCell(i+1, 3, tview.NewTableCell(fmt.Sprintf("%d", r.TotalTokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			table.SetCell(i+1, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
			table.SetCell(i+1, 5, tview.NewTableCell(fmt.Sprintf("%d", r.Status)).SetTextColor(statusColor).SetAlign(tview.AlignRight).SetExpansion(1))
			table.SetCell(i+1, 6, tview.NewTableCell(fmt.Sprintf("%dms", r.LatencyMs)).SetAlign(tview.AlignRight).SetExpansion(1))
		}
	}

	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.SwitchToPage("main")
			return nil
		}
		return event
	})

	pages.AddAndSwitchToPage("sub", table, true)
}

func showDateRangeInput(app *tview.Application, pages *tview.Pages, target string) {
	fromField := tview.NewInputField().SetLabel("From (DD-MM-YYYY)").SetFieldWidth(20)
	toField := tview.NewInputField().SetLabel("To (DD-MM-YYYY)").SetFieldWidth(20)
	form := tview.NewForm().
		AddFormItem(fromField).
		AddFormItem(toField).
		AddButton("Submit", func() {
			from := strings.TrimSpace(fromField.GetText())
			to := strings.TrimSpace(toField.GetText())
			if from == "" || to == "" {
				return
			}
			pages.RemovePage("input")
			switch target {
			case "usage":
				showUsageTable(app, pages, "custom", from, to)
			case "logs":
				showLogsTable(app, pages, "custom", from, to)
			}
		}).
		AddButton("Cancel", func() {
			pages.RemovePage("input")
		})
	form.SetBorder(true).SetTitle(" Date Range ").SetTitleAlign(tview.AlignCenter)
	form.SetCancelFunc(func() { pages.RemovePage("input") })
	pages.AddAndSwitchToPage("input", form, true)
}

// ── Show All Config ───────────────────────────────────────────────────────

func buildShowAll(app *tview.Application, pages *tview.Pages) *tview.List {
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

	list := tview.NewList().
		AddItem(fmt.Sprintf("API Key:      %s", masked), fmt.Sprintf("Status: %s", status), 'k', nil).
		AddItem(fmt.Sprintf("Delay:        %s ms", delay), "", 'd', nil).
		AddItem(fmt.Sprintf("Domain:       %s", domain), "", 'm', nil).
		AddItem(fmt.Sprintf("Proxy:        %s", proxy), "", 'p', nil).
		AddItem(fmt.Sprintf("PAT Strategy: %s", strategy), "", 's', nil).
		AddItem("← Back", "Return to main menu", 'b', func() {
			pages.SwitchToPage("main")
		})

	list.SetBorder(true).SetTitle(" All Config ").SetTitleAlign(tview.AlignCenter)
	return list
}

// ── Helpers ───────────────────────────────────────────────────────────────

func checkMark(current, this string) string {
	if current == this {
		return " ✓"
	}
	return ""
}

func showModal(app *tview.Application, pages *tview.Pages, msg string) {
	modal := tview.NewModal().
		SetText(msg).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			pages.RemovePage("modal")
		})
	pages.AddAndSwitchToPage("modal", modal, true)
}

func showInput(app *tview.Application, pages *tview.Pages, label, placeholder string, onSubmit func(string)) {
	inputField := tview.NewInputField().SetLabel(label).SetPlaceholder(placeholder).SetFieldWidth(40)
	form := tview.NewForm().
		AddFormItem(inputField).
		AddButton("Submit", func() {
			val := strings.TrimSpace(inputField.GetText())
			if val != "" {
				pages.RemovePage("input")
				onSubmit(val)
			}
		}).
		AddButton("Cancel", func() {
			pages.RemovePage("input")
		})
	form.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", label)).SetTitleAlign(tview.AlignCenter)
	form.SetCancelFunc(func() {
		pages.RemovePage("input")
	})
	pages.AddAndSwitchToPage("input", form, true)
}
