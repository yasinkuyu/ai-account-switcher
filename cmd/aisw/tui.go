package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ----------------------------------------------------------- accounts --
// The simple view: every tool with the accounts saved for it. Profiles stay
// underneath (an account is a tool's entry in a profile), so the
// `aisw <name>` commands keep working.

type account struct {
	tool    Tool
	profile string
	entry   ToolEntry
	active  bool
}

type toolView struct {
	tool      Tool
	accounts  []account
	loggedIn  bool
	installed bool
}

// findBin resolves a tool's command, including well-known install paths.
func findBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	extra := map[string][]string{"agy": {filepath.Join(home(), ".gemini", "bin", "agy")}}
	for _, p := range extra[name] {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// autoName picks a profile name for a new account: the suggested name, or
// name-2, name-3… when that name already holds another account of this tool.
func autoName(m *Meta, t Tool, s Snapshot) string {
	base := suggestName(t, s)
	if base == "" || base == backupName {
		base = t.Name()
	}
	id := t.Identify(s).ID
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		e, ok := m.Profiles[name][t.Name()]
		if !ok || e.ID == id {
			return name
		}
	}
}

// autoSave stores the current login of t if it is not saved yet and returns
// the profile holding it ("" when not logged in).
func autoSave(m *Meta, t Tool, cur Snapshot) (string, error) {
	if cur == nil || unreliable(t, cur) {
		return "", nil
	}
	id := t.Identify(cur)
	if p := matchProfile(m, t.Name(), id.ID); p != "" {
		return p, nil
	}
	if t.Name() == "cursor" && cur["email"] == "" { // fetch the email for a nicer label
		if full, err := t.Capture(true); err == nil && full != nil {
			cur = full
		}
	}
	name := autoName(m, t, cur)
	if err := store(m, t, name, cur); err != nil {
		return "", err
	}
	return name, saveMeta(m)
}

func loadViews(m *Meta) ([]toolView, error) {
	var views []toolView
	for _, t := range tools {
		v := toolView{tool: t, installed: findBin(t.LoginCmd()[0]) != ""}
		cur, err := t.Capture(false)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.Name(), err)
		}
		curID := ""
		if cur != nil {
			v.loggedIn = true
			curID = t.Identify(cur).ID
			if _, err := autoSave(m, t, cur); err != nil {
				return nil, fmt.Errorf("%s: %v", t.Name(), err)
			}
		}
		seen := map[string]bool{}
		for _, n := range sortedNames(m) {
			e, ok := m.Profiles[n][t.Name()]
			if n == backupName || !ok || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			v.accounts = append(v.accounts, account{tool: t, profile: n, entry: e, active: e.ID != "" && e.ID == curID})
		}
		if v.installed || len(v.accounts) > 0 {
			views = append(views, v)
		}
	}
	return views, nil
}

// -------------------------------------------------------------- add --

// addAccount saves the current login, removes it from this machine only
// (never the tool's logout, which revokes it), runs the tool's own login and
// saves the result. If the login does not complete, the old one is restored.
func addAccount(m *Meta, t Tool) error {
	cmdLine := t.LoginCmd()
	bin := findBin(cmdLine[0])
	if bin == "" {
		return fmt.Errorf("%s is not installed (%s not found)", t.Title(), cmdLine[0])
	}
	stop := spinner("Saving your current " + t.Title() + " account…")
	cur, err := t.Capture(true)
	if err != nil {
		stop()
		return err
	}
	prev, err := autoSave(m, t, cur)
	stop()
	if err != nil {
		return err
	}
	if cur != nil {
		if err := t.Clear(); err != nil {
			return err
		}
		fmt.Printf("Your current %s account is saved as [%s].\n", t.Title(), prev)
	}
	fmt.Printf("Log in to the new account with: %s\n", strings.Join(cmdLine, " "))
	if t.Name() == "antigravity" {
		fmt.Println("Sign in when agy asks, then quit it (Ctrl-C) to come back here.")
	}
	fmt.Println()
	c := exec.Command(bin, cmdLine[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = c.Run()
	fmt.Println()

	stop = spinner("Checking the new login…")
	after, err := t.Capture(true)
	stop()
	if err != nil {
		return err
	}
	if after == nil {
		if cur != nil {
			if err := t.Apply(cur); err != nil {
				return fmt.Errorf("login not completed and restoring failed: %v (it is saved as [%s])", err, prev)
			}
			fmt.Printf("Login not completed — back on [%s].\n", prev)
			return nil
		}
		return errors.New("login not completed")
	}
	if cur != nil && t.Identify(after).ID == t.Identify(cur).ID {
		fmt.Printf("That is the same account as before ([%s]).\n", prev)
		return nil
	}
	name, err := autoSave(m, t, after)
	if err != nil {
		return err
	}
	e := m.Profiles[name][t.Name()]
	fmt.Printf("✓ Added %s — %s\n", t.Title(), e.Label)
	if prev != "" {
		fmt.Printf("  Switch back any time:  aisw %s %s\n", t.Name(), prev)
	}
	return nil
}

func cmdAdd(args []string) error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if !isTTY() {
			return errors.New("usage: aisw add <tool>")
		}
		enableVT()
		stop := spinner("Loading…")
		views, err := loadViews(m)
		stop()
		if err != nil {
			return err
		}
		t, err := pickTool(views)
		if err != nil || t == nil {
			return err
		}
		return addAccount(m, t)
	}
	ts, err := parseTools(args)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if err := addAccount(m, t); err != nil {
			return fmt.Errorf("%s: %v", t.Name(), err)
		}
	}
	return nil
}

// ------------------------------------------------------------- screen --

const (
	screenEnter = "\x1b[?1049h\x1b[?25l"
	screenLeave = "\x1b[?25h\x1b[?1049l"
)

var useColor = os.Getenv("NO_COLOR") == ""

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string  { return paint("1", s) }
func dim(s string) string   { return paint("2", s) }
func green(s string) string { return paint("32", s) }
func cyan(s string) string  { return paint("36", s) }

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyEnter
	keyQuit
	keyAdd
	keyDelete
	keyYes
	keyNo
)

func readKey() key {
	buf := make([]byte, 16)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return keyQuit
	}
	b := buf[:n]
	switch {
	case string(b) == "\x1b[A" || string(b) == "\x1bOA" || b[0] == 'k':
		return keyUp
	case string(b) == "\x1b[B" || string(b) == "\x1bOB" || b[0] == 'j':
		return keyDown
	case b[0] == '\r' || b[0] == '\n':
		return keyEnter
	case b[0] == 'q' || b[0] == 3 || string(b) == "\x1b":
		return keyQuit
	case b[0] == 'a' || b[0] == '+':
		return keyAdd
	case b[0] == 'd' || b[0] == 'x':
		return keyDelete
	case b[0] == 'y' || b[0] == 'Y':
		return keyYes
	case b[0] == 'n' || b[0] == 'N':
		return keyNo
	}
	return keyNone
}

// row kinds in the menu
type item struct {
	tool    Tool
	acct    *account // nil: "not logged in" line or the global add line
	addLine bool
}

type menuResult struct {
	switchTo *account
	add      Tool
	addPick  bool
}

func buildItems(views []toolView) []item {
	var items []item
	for _, v := range views {
		if len(v.accounts) == 0 {
			items = append(items, item{tool: v.tool})
			continue
		}
		for i := range v.accounts {
			items = append(items, item{tool: v.tool, acct: &v.accounts[i]})
		}
	}
	return append(items, item{addLine: true})
}

func render(views []toolView, items []item, sel int, status string) string {
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString("\n  " + bold("aisw") + dim("  ·  switch AI accounts") + "\n")
	idx := 0
	for _, v := range views {
		b.WriteString("\n  " + bold(v.tool.Title()) + "\n")
		n := len(v.accounts)
		if n == 0 {
			n = 1
		}
		for k := 0; k < n; k++ {
			it := items[idx]
			cursor := "  "
			if idx == sel {
				cursor = cyan("❯ ")
			}
			if it.acct == nil {
				b.WriteString("  " + cursor + dim("○ not logged in — press a to add") + "\n")
			} else {
				a := it.acct
				dot, label := "○", a.entry.Label
				if a.active {
					dot = green("●")
				}
				if label == "" {
					label = "?"
				}
				pad := 44 - len([]rune(label))
				if pad < 1 {
					pad = 1
				}
				b.WriteString("  " + cursor + dot + " " + label + strings.Repeat(" ", pad) + dim(a.profile) + "\n")
			}
			idx++
		}
	}
	cursor := "  "
	if idx == sel {
		cursor = cyan("❯ ")
	}
	b.WriteString("\n  " + cursor + "+ Add account\n")
	b.WriteString("\n  " + dim("↑↓ move · enter switch · a add · d delete · q quit") + "\n")
	if status != "" {
		b.WriteString("\n  " + status + "\n")
	}
	return b.String()
}

// runMenu shows the interactive menu and returns what the user picked.
func runMenu(m *Meta, views []toolView) (menuResult, error) {
	enableVT()
	restore, err := rawMode()
	if err != nil {
		return menuResult{}, err
	}
	os.Stdout.WriteString(screenEnter)
	defer func() { os.Stdout.WriteString(screenLeave); restore() }()

	items := buildItems(views)
	sel := 0
	for i, it := range items { // start on the first active account
		if it.acct != nil && it.acct.active {
			sel = i
			break
		}
	}
	status := ""
	for {
		os.Stdout.WriteString(render(views, items, sel, status))
		status = ""
		switch readKey() {
		case keyUp:
			if sel > 0 {
				sel--
			}
		case keyDown:
			if sel < len(items)-1 {
				sel++
			}
		case keyQuit:
			return menuResult{}, nil
		case keyAdd:
			if items[sel].addLine {
				return menuResult{addPick: true}, nil
			}
			return menuResult{add: items[sel].tool}, nil
		case keyEnter:
			it := items[sel]
			switch {
			case it.addLine:
				return menuResult{addPick: true}, nil
			case it.acct == nil:
				return menuResult{add: it.tool}, nil
			case it.acct.active:
				status = "Already using this account."
			default:
				return menuResult{switchTo: it.acct}, nil
			}
		case keyDelete:
			it := items[sel]
			if it.acct == nil {
				continue
			}
			if it.acct.active {
				status = "This account is in use — switch to another one before deleting it."
				continue
			}
			os.Stdout.WriteString(render(views, items, sel,
				fmt.Sprintf("Delete %s from aisw? It stays logged in on the server. [y/N]", it.acct.entry.Label)))
			if readKey() != keyYes {
				continue
			}
			a := it.acct
			blobDel(a.tool.Name()+"/"+a.profile, a.entry.Chunks)
			delete(m.Profiles[a.profile], a.tool.Name())
			if len(m.Profiles[a.profile]) == 0 {
				delete(m.Profiles, a.profile)
			}
			if err := saveMeta(m); err != nil {
				return menuResult{}, err
			}
			os.Stdout.WriteString(render(views, items, sel, dim("Updating…")))
			if views, err = loadViews(m); err != nil {
				return menuResult{}, err
			}
			items = buildItems(views)
			if sel >= len(items) {
				sel = len(items) - 1
			}
			status = "Deleted."
		}
	}
}

// pickTool asks which tool to add an account to.
func pickTool(views []toolView) (Tool, error) {
	var ts []Tool
	for _, v := range views {
		if v.installed {
			ts = append(ts, v.tool)
		}
	}
	if len(ts) == 0 {
		return nil, errors.New("none of the supported tools is installed")
	}
	enableVT()
	restore, err := rawMode()
	if err != nil {
		return nil, err
	}
	os.Stdout.WriteString(screenEnter)
	defer func() { os.Stdout.WriteString(screenLeave); restore() }()
	sel := 0
	for {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J\n  " + bold("Add an account to") + "\n\n")
		for i, t := range ts {
			cursor := "  "
			if i == sel {
				cursor = cyan("❯ ")
			}
			b.WriteString("  " + cursor + t.Title() + "\n")
		}
		b.WriteString("\n  " + dim("↑↓ move · enter choose · q back") + "\n")
		os.Stdout.WriteString(b.String())
		switch readKey() {
		case keyUp:
			if sel > 0 {
				sel--
			}
		case keyDown:
			if sel < len(ts)-1 {
				sel++
			}
		case keyEnter:
			return ts[sel], nil
		case keyQuit:
			return nil, nil
		}
	}
}

// cmdMenu is plain `aisw`.
func cmdMenu() error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	enableVT()
	stop := spinner("Loading accounts…")
	views, err := loadViews(m)
	stop()
	if err != nil {
		return err
	}
	if !isTTY() {
		printViews(views)
		return nil
	}
	res, err := runMenu(m, views)
	if err != nil {
		printViews(views) // terminal could not be controlled: show the list
		return nil
	}
	switch {
	case res.switchTo != nil:
		return switchTo(res.switchTo.profile, []string{res.switchTo.tool.Name()})
	case res.add != nil:
		return addAccount(m, res.add)
	case res.addPick:
		t, err := pickTool(views)
		if err != nil || t == nil {
			return err
		}
		return addAccount(m, t)
	}
	return nil
}

func printViews(views []toolView) {
	for _, v := range views {
		fmt.Println(v.tool.Title())
		if len(v.accounts) == 0 {
			fmt.Println("  ○ not logged in")
		}
		for _, a := range v.accounts {
			dot := "○"
			if a.active {
				dot = "●"
			}
			fmt.Printf("  %s %-44s %s\n", dot, a.entry.Label, a.profile)
		}
	}
}

// spinner shows "⠋ msg" while slow work runs (only on a terminal).
func spinner(msg string) func() {
	if !isTTY() {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		for i := 0; ; i++ {
			select {
			case <-done:
				os.Stdout.WriteString("\r\x1b[2K")
				return
			default:
			}
			os.Stdout.WriteString("\r  " + cyan(frames[i%len(frames)]) + " " + dim(msg))
			time.Sleep(80 * time.Millisecond)
		}
	}()
	return func() { close(done); <-finished }
}
