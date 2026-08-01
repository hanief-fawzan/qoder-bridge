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
	// If we're back at main, clear sub pages
	name2, _ := pages.GetFrontPage()
	if name2 == "main" {
		pages.RemovePage("sub")
	}
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

// ── Helpers ───────────────────────────────────────────────────────────────

// hintBar creates a footer hint bar for any page.
func hintBar(hint string) tview.Primitive {
	return tview.NewTextView().SetDynamicColors(true).
		SetText("  " + hint)
}

// wrapWithHint wraps a primitive with a footer hint bar and wires Esc.
func wrapWithHint(p tview.Primitive, hint string) tview.Primitive {
	hintView := hintBar(hint)
	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p, 0, 1, true).
		AddItem(hintView, 1, 0, false)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			goBack()
			return nil
		}
		return event
	})
	return flex
}

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
		if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyEnter {
			pages.RemovePage("modal")
			return nil
		}
		return event
	})
	pages.AddAndSwitchToPage("modal", modal, true)
}

// showInput shows an input form with Enter to submit, Esc to cancel.
// Empty input is allowed (onSubmit receives "").
func showInput(label, placeholder string, onSubmit func(string)) {
	inp := tview.NewInputField().SetLabel(label + ": ").SetPlaceholder(placeholder).SetFieldWidth(50)
	inp.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			pages.RemovePage("form")
			onSubmit(strings.TrimSpace(inp.GetText()))
		} else if key == tcell.KeyEscape {
			pages.RemovePage("form")
		}
	})
	hint := tview.NewTextView().SetDynamicColors(true).
		SetText(fmt.Sprintf("  %sEnter%s submit   %sEsc%s cancel", colorKey, colorReset, colorKey, colorReset))
	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(inp, 1, 0, true).
		AddItem(hint, 1, 0, false)
	flex.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", label)).SetTitleAlign(tview.AlignCenter)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.RemovePage("form")
			return nil
		}
		return event
	})
	pages.AddAndSwitchToPage("form", flex, true)
	app.SetFocus(inp)
}

// showDateInput shows a date range input with Enter to submit, Esc to cancel.
func showDateInput(onSubmit func(from, to string)) {
	fromF := tview.NewInputField().SetLabel("From (DD-MM-YYYY): ").SetFieldWidth(20)
	toF := tview.NewInputField().SetLabel("  To (DD-MM-YYYY): ").SetFieldWidth(20)

	submit := func() {
		from := strings.TrimSpace(fromF.GetText())
		to := strings.TrimSpace(toF.GetText())
		if from != "" && to != "" {
			pages.RemovePage("form")
			onSubmit(from, to)
		}
	}
	fromF.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			app.SetFocus(toF)
		} else if key == tcell.KeyEscape {
			pages.RemovePage("form")
		}
	})
	toF.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			submit()
		} else if key == tcell.KeyEscape {
			pages.RemovePage("form")
		}
	})
	hint := tview.NewTextView().SetDynamicColors(true).
		SetText(fmt.Sprintf("  %sEnter%s submit   %sEsc%s cancel", colorKey, colorReset, colorKey, colorReset))
	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(fromF, 1, 0, true).
		AddItem(toF, 1, 0, false).
		AddItem(hint, 1, 0, false)
	flex.SetBorder(true).SetTitle(" Date Range ").SetTitleAlign(tview.AlignCenter)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			pages.RemovePage("form")
			return nil
		}
		return event
	})
	pages.AddAndSwitchToPage("form", flex, true)
	app.SetFocus(fromF)
}

// ── Main Menu ─────────────────────────────────────────────────────────────

func mainMenu() tview.Primitive {
	list := tview.NewList().
		AddItem("  🔑  API Keys", "Generate, enable/disable, view", 'a', func() {
			pushPage("sub", apiKeyMenu())
		}).
		AddItem("  🌐  Proxy", "Set, view, or remove proxy URL", 'p', func() {
			pushPage("sub", proxyMenu())
		}).
		AddItem("  🌍  Domain", "Set domain for public endpoint URLs", 'n', func() {
			pushPage("sub", domainMenu())
		}).
		AddItem("  📋  Endpoints", "Show API endpoint URLs (copy-paste)", 'e', func() {
			pushPage("sub", endpointsView())
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

	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			app.Stop()
			return nil
		}
		return event
	})

	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s exit", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

// ── API Keys ──────────────────────────────────────────────────────────────

func apiKeyMenu() tview.Primitive {
	keys, err := listAPIKeys()
	if err != nil {
		keys = []APIKeyEntry{}
	}

	// Master auth toggle: is API key authentication required?
	authRequired := cfgBool("api_key_enabled", false) || len(keys) > 0
	authStatus := colorRed + "disabled — open access"
	if authRequired {
		authStatus = colorGreen + "enabled — key required"
	}

	list := tview.NewList()
	list.AddItem("  🔒  Require API Key", fmt.Sprintf("Auth: %s%s", authStatus, colorReset), 'r', func() {
		current := cfgBool("api_key_enabled", false)
		cfgSet("api_key_enabled", map[bool]string{true: "0", false: "1"}[current])
		pages.RemovePage("sub")
		pushPage("sub", apiKeyMenu())
	})
	list.AddItem("  Generate New Key", "Create named sk-* key", 'g', func() {
		showInput("Key name", "hermes-desktop", func(name string) {
			if name == "" {
				name = "generated"
			}
			b := make([]byte, 24)
			rand.Read(b)
			newKey := "sk-" + hex.EncodeToString(b)
			if err := addAPIKey(name, newKey); err != nil {
				showMsg(fmt.Sprintf("%s❌ Failed:%s\n%v", colorRed, colorReset, err), "apikey", apiKeyMenu)
				return
			}
			showMsg(fmt.Sprintf("%s✅ Generated key [%s]:%s\n%s\n\nCopy this key now — it won't be shown again in full.", colorGreen, name, colorReset, newKey), "apikey", apiKeyMenu)
			pages.RemovePage("sub")
			pushPage("sub", apiKeyMenu())
		})
	})
	list.AddItem("  View & Manage Keys", fmt.Sprintf("%d key(s) — Enter to view, toggle, delete", len(keys)), 'v', func() {
		pushPage("sub", apiKeyTableView())
	})

	// Legacy single key (backward compat)
	legacyKey := cfgGet("api_key")
	legacyMasked := "(not set)"
	if legacyKey != "" {
		legacyMasked = legacyKey[:min(8, len(legacyKey))] + "..." + legacyKey[max(0, len(legacyKey)-4):]
	}
	list.AddItem("  Set Legacy Key", fmt.Sprintf("Current: %s", legacyMasked), 'k', func() {
		showInput("Legacy API Key", "sk-...", func(val string) {
			if val != "" {
				cfgSet("api_key", val)
			}
			pages.RemovePage("sub")
			pushPage("sub", apiKeyMenu())
		})
	})
	list.AddItem("  Clear Legacy Key", "Remove legacy key entirely", 'x', func() {
		cfgSet("api_key", "")
		pages.RemovePage("sub")
		pushPage("sub", apiKeyMenu())
	})
	list.AddItem("  ← Back", "Esc to go back", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " API Keys ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

func apiKeyTableView() tview.Primitive {
	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	t.SetBorder(true).SetTitle(colorTitle + " API Keys — Enter to select " + colorReset).SetTitleAlign(tview.AlignCenter)

	headers := []string{"ID", "Name", "Key", "Status", "Created"}
	for i, h := range headers {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}

	keys, err := listAPIKeys()
	if err != nil {
		t.SetCell(1, 0, tview.NewTableCell("DB error: "+err.Error()).SetSelectable(false))
		return wrapTable(t)
	}
	if len(keys) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No API keys. Generate one from the menu.").SetSelectable(false))
		return wrapTable(t)
	}

	for i, k := range keys {
		status := colorRed + "disabled" + colorReset
		if k.Enabled {
			status = colorGreen + "enabled" + colorReset
		}
		masked := k.APIKey[:min(8, len(k.APIKey))] + "..." + k.APIKey[max(0, len(k.APIKey)-4):]
		t.SetCell(i+1, 0, tview.NewTableCell(fmt.Sprintf("%d", k.ID)).SetExpansion(1))
		t.SetCell(i+1, 1, tview.NewTableCell(k.Name).SetExpansion(2))
		t.SetCell(i+1, 2, tview.NewTableCell(masked).SetExpansion(3))
		t.SetCell(i+1, 3, tview.NewTableCell(status).SetExpansion(1))
		t.SetCell(i+1, 4, tview.NewTableCell(formatTS(k.CreatedAt)).SetExpansion(2))
	}

	// Enter to select a row → show key detail with toggle/copy/delete
	t.SetSelectedFunc(func(row, col int) {
		if row < 1 || row > len(keys) {
			return
		}
		k := keys[row-1]
		status := colorRed + "disabled" + colorReset
		if k.Enabled {
			status = colorGreen + "enabled" + colorReset
		}
		toggleLabel := "  ✓ Enable"
		if k.Enabled {
			toggleLabel = "  ✗ Disable"
		}
		detail := tview.NewList()
		detail.AddItem(fmt.Sprintf("  Name: %s", k.Name), "", 0, nil)
		detail.AddItem(fmt.Sprintf("  Key:  %s", k.APIKey), colorDim+"(full key visible)"+colorReset, 0, nil)
		detail.AddItem(fmt.Sprintf("  Status: %s%s", status, colorReset), "", 0, nil)
		detail.AddItem(toggleLabel, "Toggle enable/disable", 't', func() {
			if err := toggleAPIKey(k.ID, !k.Enabled); err != nil {
				showMsg(fmt.Sprintf("%s❌ Error:%s\n%v", colorRed, colorReset, err), "apikey", func() tview.Primitive { return apiKeyTableView() })
				return
			}
			// Rebuild both views
			pages.RemovePage("detail")
			pages.RemovePage("sub")
			pushPage("sub", apiKeyTableView())
		})
		detail.AddItem("  🗑  Delete", "Remove this key permanently", 'd', func() {
			if err := removeAPIKey(k.ID); err != nil {
				showMsg(fmt.Sprintf("%s❌ Error:%s\n%v", colorRed, colorReset, err), "apikey", func() tview.Primitive { return apiKeyTableView() })
				return
			}
			pages.RemovePage("detail")
			pages.RemovePage("sub")
			pushPage("sub", apiKeyTableView())
		})
		detail.AddItem("  ← Back", "Esc", 'b', func() { pages.RemovePage("detail") })
		detail.SetBorder(true).SetTitle(fmt.Sprintf(colorTitle+" Key: %s ", k.Name)).SetTitleAlign(tview.AlignCenter)
		wireEsc(detail)
		pages.AddAndSwitchToPage("detail", wrapWithHint(detail, fmt.Sprintf("%st%s toggle   %sd%s delete   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset)), true)
	})

	return wrapTable(t)
}

// ── Proxy ─────────────────────────────────────────────────────────────────

func proxyMenu() tview.Primitive {
	cur := cfgGet("proxy")
	curDisplay := cur
	if curDisplay == "" {
		curDisplay = colorDim + "(direct connection)" + colorReset
	}

	list := tview.NewList()
	list.AddItem("  Set Proxy", "Enter proxy URL", 's', func() {
		showInput("Proxy URL", "socks5://user:pass@host:port", func(val string) {
			if val != "" {
				cfgSet("proxy", val)
			}
			// Rebuild menu so current proxy updates immediately
			pages.RemovePage("sub")
			pushPage("sub", proxyMenu())
		})
	})
	list.AddItem("  View Current", curDisplay, 'v', func() {
		p := cfgGet("proxy")
		if p == "" {
			showMsg("Proxy: (direct connection)", "proxy", proxyMenu)
		} else {
			showMsg("Proxy: "+p, "proxy", proxyMenu)
		}
	})
	list.AddItem("  Clear", "Remove proxy", 'x', func() {
		cfgSet("proxy", "")
		// Rebuild menu so current proxy updates immediately (real-time sync)
		pages.RemovePage("sub")
		pushPage("sub", proxyMenu())
	})
	list.AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Proxy ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

// ── Domain ────────────────────────────────────────────────────────────────

func domainMenu() tview.Primitive {
	cur := cfgGet("domain")
	curDisplay := cur
	if curDisplay == "" {
		curDisplay = colorDim + "(not set)" + colorReset
	}

	list := tview.NewList()
	list.AddItem("  Set Domain", "Enter domain (e.g. bridge.example.com)", 's', func() {
		showInput("Domain", "bridge.example.com", func(val string) {
			if val != "" {
				cfgSet("domain", val)
			}
			pages.RemovePage("sub")
			pushPage("sub", domainMenu())
		})
	})
	list.AddItem("  View Current", curDisplay, 'v', func() {
		d := cfgGet("domain")
		if d == "" {
			showMsg("Domain: (not set)", "domain", domainMenu)
		} else {
			showMsg("Domain: "+d, "domain", domainMenu)
		}
	})
	list.AddItem("  Clear", "Remove domain", 'x', func() {
		cfgSet("domain", "")
		pages.RemovePage("sub")
		pushPage("sub", domainMenu())
	})
	list.AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Domain ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

// ── Endpoints ─────────────────────────────────────────────────────────────

func endpointsView() tview.Primitive {
	domain := cfgGet("domain")
	var base string
	if domain != "" {
		base = "https://" + domain
	} else {
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	endpoints := []struct {
		name string
		path string
	}{
		{"Chat Completions", "/v1/chat/completions"},
		{"Models", "/v1/models"},
		{"Status", "/v1/status"},
		{"Quota", "/v1/quota"},
		{"Combos", "/v1/combos"},
		{"Health", "/health"},
	}

	text := tview.NewTextView().SetDynamicColors(true)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n  %s─── API Endpoints ───%s\n\n", colorTitle, colorReset))
	for _, e := range endpoints {
		sb.WriteString(fmt.Sprintf("  %s%-20s%s %s%s%s\n", colorKey, e.name, colorReset, colorAccent, base+e.path, colorReset))
	}
	sb.WriteString(fmt.Sprintf("\n  %sBase URL:%s %s%s%s\n", colorKey, colorReset, colorAccent, base, colorReset))
	if domain == "" {
		sb.WriteString(fmt.Sprintf("\n  %sTip: Set a domain in config for a public URL.%s\n", colorDim, colorReset))
	}
	text.SetText(sb.String())
	text.SetBorder(true).SetTitle(colorTitle + " Endpoints ").SetTitleAlign(tview.AlignCenter)

	return wrapWithHint(text, fmt.Sprintf("%sEsc%s back to menu", colorKey, colorReset))
}

// ── Delay ─────────────────────────────────────────────────────────────────

func delayMenu() tview.Primitive {
	cur := cfgGet("request_delay_ms")
	display := cur + " ms"
	if cur == "" {
		display = colorDim + "disabled" + colorReset
	}

	list := tview.NewList()
	list.AddItem("  Set Delay", "Enter ms value", 's', func() {
		showInput("Delay (ms)", "1000", func(val string) {
			if val != "" {
				cfgSet("request_delay_ms", val)
			}
			pages.RemovePage("sub")
			pushPage("sub", delayMenu())
		})
	})
	list.AddItem("  View Current", display, 'v', func() {
		showMsg("Delay: "+cfgGet("request_delay_ms")+" ms", "delay", delayMenu)
	})
	list.AddItem("  Disable", "No delay", 'x', func() {
		cfgSet("request_delay_ms", "")
		pages.RemovePage("sub")
		pushPage("sub", delayMenu())
	})
	list.AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Request Delay ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
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

	list := tview.NewList()
	list.AddItem(rr, "Cycle PATs in order", 'r', func() {
		cfgSet("pat_strategy", "round-robin")
		pages.RemovePage("sub")
		pushPage("sub", strategyMenu())
	})
	list.AddItem(rn, "Random PAT each request", 'd', func() {
		cfgSet("pat_strategy", "random")
		pages.RemovePage("sub")
		pushPage("sub", strategyMenu())
	})
	list.AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " PAT Strategy ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

// ── Import from .env ──────────────────────────────────────────────────────

func importMenu() tview.Primitive {
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

	list := tview.NewList()
	list.AddItem("  Import ALL from .env", "Migrate all detected values to DB", 'a', func() {
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
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

// ── Usage ─────────────────────────────────────────────────────────────────

func usageMenu() tview.Primitive {
	list := tview.NewList().
		AddItem("  Today", "Since midnight", 't', func() { showUsage("today", "pat") }).
		AddItem("  This Week", "Past 7 days", 'w', func() { showUsage("week", "pat") }).
		AddItem("  This Month", "Past 30 days", 'm', func() { showUsage("month", "pat") }).
		AddItem("  This Year", "Past 365 days", 'y', func() { showUsage("year", "pat") }).
		AddItem("  Custom Range", "DD-MM-YYYY to DD-MM-YYYY", 'c', func() {
			showDateInput(func(from, to string) { showUsage("custom", "pat", from, to) })
		}).
		AddItem(colorAccent+"  ─── By API Key ───"+colorReset, "", 0, nil).
		AddItem("  Today (by API Key)", "Since midnight, grouped by API key", 'T', func() { showUsage("today", "apikey") }).
		AddItem("  This Week (by API Key)", "Past 7 days, grouped by API key", 'W', func() { showUsage("week", "apikey") }).
		AddItem("  This Month (by API Key)", "Past 30 days, grouped by API key", 'M', func() { showUsage("month", "apikey") }).
		AddItem("  ← Back", "Esc", 'b', func() { goBack() })

	list.SetBorder(true).SetTitle(colorTitle + " Usage ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}

func showUsage(period string, groupBy string, dates ...string) {
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

	var rows []UsageRow
	if groupBy == "apikey" {
		rows, err = queryUsageByAPIKey(fromTS, toTS)
	} else {
		rows, err = queryUsageByPAT(fromTS, toTS)
	}
	if err != nil {
		showMsg(fmt.Sprintf("Error: %v", err), "usage", usageMenu)
		return
	}

	summary, _ := queryUsageSummary(fromTS, toTS)

	t := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	titleGroup := "PAT"
	if groupBy == "apikey" {
		titleGroup = "API Key"
	}
	t.SetBorder(true).SetTitle(fmt.Sprintf(" %sUsage: %s (by %s)%s ", colorTitle, label, titleGroup, colorReset)).SetTitleAlign(tview.AlignCenter)

	headers := []string{titleGroup, "Model", "Req", "Tokens", "Credits", "Avg Lat"}
	for i, h := range headers {
		t.SetCell(0, i, tview.NewTableCell(h).SetTextColor(tcell.ColorYellow).SetSelectable(false).SetExpansion(1))
	}

	rowIdx := 1
	if len(rows) == 0 {
		t.SetCell(1, 0, tview.NewTableCell("No data for this period.").SetSelectable(false))
		rowIdx = 2
	} else {
		for _, r := range rows {
			avgLat := 0
			if r.Requests > 0 {
				avgLat = int(r.LastTS-r.FirstTS) / r.Requests
			}
			t.SetCell(rowIdx, 0, tview.NewTableCell(r.Group).SetExpansion(2))
			t.SetCell(rowIdx, 1, tview.NewTableCell(r.Model).SetExpansion(1))
			t.SetCell(rowIdx, 2, tview.NewTableCell(fmt.Sprintf("%d", r.Requests)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(rowIdx, 3, tview.NewTableCell(fmt.Sprintf("%d", r.Tokens)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(rowIdx, 4, tview.NewTableCell(fmt.Sprintf("%.2f", r.Credits)).SetAlign(tview.AlignRight).SetExpansion(1))
			t.SetCell(rowIdx, 5, tview.NewTableCell(fmt.Sprintf("%ds", avgLat)).SetAlign(tview.AlignRight).SetExpansion(1))
			rowIdx++
		}
	}

	// Summary row
	if summary != nil {
		t.SetCell(rowIdx, 0, tview.NewTableCell("TOTAL").SetTextColor(tcell.ColorAqua).SetSelectable(false).SetExpansion(2))
		t.SetCell(rowIdx, 1, tview.NewTableCell("").SetSelectable(false))
		t.SetCell(rowIdx, 2, tview.NewTableCell(fmt.Sprintf("%d", summary.TotalRequests)).SetTextColor(tcell.ColorAqua).SetAlign(tview.AlignRight).SetSelectable(false))
		t.SetCell(rowIdx, 3, tview.NewTableCell(fmt.Sprintf("%d", summary.TotalTokens)).SetTextColor(tcell.ColorAqua).SetAlign(tview.AlignRight).SetSelectable(false))
		t.SetCell(rowIdx, 4, tview.NewTableCell(fmt.Sprintf("%.2f", summary.TotalCredits)).SetTextColor(tcell.ColorAqua).SetAlign(tview.AlignRight).SetSelectable(false))
		t.SetCell(rowIdx, 5, tview.NewTableCell(fmt.Sprintf("%dms", summary.AvgLatencyMs)).SetTextColor(tcell.ColorAqua).SetAlign(tview.AlignRight).SetSelectable(false))
		if summary.ErrorCount > 0 {
			rowIdx++
			t.SetCell(rowIdx, 0, tview.NewTableCell(fmt.Sprintf("%s%d errors%s", colorRed, summary.ErrorCount, colorReset)).SetSelectable(false))
		}
	}

	// Toggle hint
	nextGroup := "API Key"
	if groupBy == "apikey" {
		nextGroup = "PAT"
	}
	hint := tview.NewTextView().SetDynamicColors(true).
		SetText(fmt.Sprintf("  %s↑↓%s navigate   %sEsc%s back   %s't'%s toggle to by %s", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset, nextGroup))

	wireEsc(t)
	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(t, 0, 1, true).
		AddItem(hint, 1, 0, false)
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			goBack()
			return nil
		}
		if event.Rune() == 't' || event.Rune() == 'T' {
			pages.RemovePage("sub")
			if groupBy == "pat" {
				showUsage(period, "apikey", dates...)
			} else {
				showUsage(period, "pat", dates...)
			}
			return nil
		}
		return event
	})

	pushPage("sub", flex)
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
	return wrapWithHint(list, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
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
	return wrapWithHint(list, fmt.Sprintf("%sEsc%s back to menu", colorKey, colorReset))
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
			result := tuiRunUpdate()
			pages.RemovePage("sub")
			pushPage("sub", updateResult(result))
			return nil
		}
		return event
	})

	return wrapWithHint(text, fmt.Sprintf("%sEnter%s update   %sEsc%s cancel", colorKey, colorReset, colorKey, colorReset))
}

func tuiRunUpdate() string {
	var sb strings.Builder

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

	out, err := exec.Command("git", "pull").Output()
	if err != nil {
		sb.WriteString(fmt.Sprintf("git pull FAILED: %v\n%s\n", err, string(out)))
		return sb.String()
	}
	sb.WriteString("git pull... ok\n")

	out, err = exec.Command("go", "build", "-o", "qoder-bridge", ".").CombinedOutput()
	if err != nil {
		sb.WriteString(fmt.Sprintf("build FAILED: %v\n%s\n", err, string(out)))
		return sb.String()
	}
	sb.WriteString("build... ok\n")

	sb.WriteString("stopping daemon...\n")
	stopOut, _ := exec.Command(bridgeExe(), "stop").CombinedOutput()
	sb.WriteString(fmt.Sprintf("%s\n", strings.TrimSpace(string(stopOut))))
	time.Sleep(2 * time.Second)

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
	tmpDest := dest + ".tmp"
	cpOut, err := exec.Command("cp", exe, tmpDest).CombinedOutput()
	if err != nil {
		os.Remove(tmpDest)
		sb.WriteString(fmt.Sprintf("install FAILED: %v\n%s\n", err, string(cpOut)))
		return sb.String()
	}
	if err := os.Rename(tmpDest, dest); err != nil {
		os.Remove(tmpDest)
		sb.WriteString(fmt.Sprintf("install FAILED (rename): %v\n", err))
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("installed to %s\n", dest))

	sb.WriteString("restarting...\n")
	sb.WriteString(tuiDoRestart())
	return sb.String()
}

func tuiDoRestart() string {
	var sb strings.Builder

	for _, svc := range [][]string{
		{"systemctl", "restart", "qoder-bridge"},
		{"systemctl", "--user", "restart", "qoder-bridge"},
	} {
		_, err := exec.Command(svc[0], svc[1:]...).CombinedOutput()
		if err == nil {
			time.Sleep(2 * time.Second)
			statusOut, _ := exec.Command(svc[0], "is-active", "qoder-bridge").CombinedOutput()
			if strings.TrimSpace(string(statusOut)) == "active" {
				sb.WriteString(fmt.Sprintf("✅ Bridge restarted (systemd: %s)\n", svc[0]))
				return sb.String()
			}
		}
	}

	exe := bridgeExe()
	cmd := exec.Command(exe, "stop")
	out, _ := cmd.CombinedOutput()
	sb.WriteString(fmt.Sprintf("stop: %s\n", strings.TrimSpace(string(out))))

	time.Sleep(500 * time.Millisecond)

	cmd = exec.Command(exe)
	cmd.Dir = bridgeDir()
	out, _ = cmd.CombinedOutput()
	sb.WriteString(fmt.Sprintf("start: %s\n", strings.TrimSpace(string(out))))

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

	return wrapWithHint(text, fmt.Sprintf("%sEnter%s restart   %sEsc%s cancel", colorKey, colorReset, colorKey, colorReset))
}

// updateResult shows the result of an update/restart with navigation options.
func updateResult(result string) tview.Primitive {
	text := tview.NewTextView().SetDynamicColors(true).SetText(result)
	text.SetScrollable(true)

	list := tview.NewList().
		AddItem("  ← Back to Main Menu", "Return to main menu", 'b', func() {
			// Clear all sub pages, go back to main
			pages.RemovePage("sub")
			pages.SwitchToPage("main")
		}).
		AddItem("  🚪 Exit", "Quit config", 'q', func() {
			app.Stop()
		})
	list.SetBorder(true).SetTitle(colorTitle + " Navigation ").SetTitleAlign(tview.AlignCenter)
	wireEsc(list)

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(text, 0, 3, true).
		AddItem(list, 0, 2, false)
	flex.SetBorder(true).SetTitle(colorTitle + " Result ").SetTitleAlign(tview.AlignCenter)

	return wrapWithHint(flex, fmt.Sprintf("%s↑↓%s navigate   %sEnter%s select   %sEsc%s back", colorKey, colorReset, colorKey, colorReset, colorKey, colorReset))
}
