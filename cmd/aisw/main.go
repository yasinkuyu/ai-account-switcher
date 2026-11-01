// aisw — switch Claude Code, Codex and Cursor CLI accounts.
//
// A single self-contained binary for macOS, Linux and Windows: no runtime,
// no packages. It snapshots each CLI's login into named profiles (secrets kept
// in the OS credential store) and swaps them in place, so every terminal and
// editor integration picks up the switched account.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var version = "dev"

const usage = `aisw — switch accounts in your AI coding CLIs

  aisw                         interactive menu (switch · add · delete)

  aisw add [tool]              add another account to a tool
  aisw <tool> <name>           switch one tool, e.g. aisw claude max
  aisw <name>                  switch every tool saved under that name
  aisw undo [tool...]          go back to the login before the last switch
  aisw status [--json]         which account each tool is using now
  aisw list                    list saved accounts
  aisw save [name] [tool...]   save current logins under a name you choose
  aisw rm <name> [tool...]     delete a saved account
  aisw prompt                  one-line status for shell prompts
  aisw doctor                  where each tool keeps its login
  aisw version

tools: claude codex cursor copilot opencode antigravity kimi

Never use a tool's own logout to change accounts: it revokes the session
on the server and the saved account stops working. Use aisw add.`

const backupName = "_previous"

// ------------------------------------------------------------- metadata --

type ToolEntry struct {
	Chunks int    `json:"chunks"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Saved  int64  `json:"saved"`
}

type Meta struct {
	Version  int                             `json:"version"`
	Profiles map[string]map[string]ToolEntry `json:"profiles"`
}

const metaVersion = 2

func stateDir() string {
	if d := os.Getenv("AISW_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".aisw")
}

func metaPath() string { return filepath.Join(stateDir(), "profiles.json") }

func loadMeta() (*Meta, error) {
	m := &Meta{Version: metaVersion, Profiles: map[string]map[string]ToolEntry{}}
	b, err := os.ReadFile(metaPath())
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var old Meta
	if err := json.Unmarshal(b, &old); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %v", metaPath(), err)
	}
	if old.Version != metaVersion {
		// profiles from the earlier prototype use another format; clear them
		for name, ts := range old.Profiles {
			for t, e := range ts {
				blobDel(t+"/"+name, e.Chunks)
			}
		}
		fmt.Fprintln(os.Stderr, "aisw: removed profiles from an older format — save them again with `aisw save <name>`")
		return m, saveMeta(m)
	}
	if old.Profiles == nil {
		old.Profiles = m.Profiles
	}
	return &old, nil
}

func saveMeta(m *Meta) error {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(metaPath(), b, 0o600)
}

func store(m *Meta, t Tool, name string, snap Snapshot) error {
	n, err := blobPut(t.Name()+"/"+name, snap)
	if err != nil {
		return err
	}
	id := t.Identify(snap)
	if m.Profiles[name] == nil {
		m.Profiles[name] = map[string]ToolEntry{}
	}
	m.Profiles[name][t.Name()] = ToolEntry{Chunks: n, ID: id.ID, Label: id.Label, Saved: time.Now().Unix()}
	return nil
}

func matchProfile(m *Meta, tool, id string) string {
	if id == "" {
		return ""
	}
	names := sortedNames(m)
	for _, n := range names {
		if n != backupName && m.Profiles[n][tool].ID == id {
			return n
		}
	}
	return ""
}

func sortedNames(m *Meta) []string {
	names := make([]string, 0, len(m.Profiles))
	for n := range m.Profiles {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == backupName) != (names[j] == backupName) {
			return names[j] == backupName
		}
		return names[i] < names[j]
	})
	return names
}

// ------------------------------------------------------------- helpers --

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".aisw-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseTools(args []string) ([]Tool, error) {
	if len(args) == 0 {
		return tools, nil
	}
	var out []Tool
	for _, a := range args {
		t := toolByName(a)
		if t == nil {
			return nil, fmt.Errorf("unknown tool %q (use: claude, codex, cursor, copilot, opencode, antigravity, kimi)", a)
		}
		out = append(out, t)
	}
	return out, nil
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func row(tool, label, extra string) {
	if label == "" {
		label = "?"
	}
	fmt.Printf("  %-11s %-42s %s\n", tool, label, extra)
}

// ------------------------------------------------------------ commands --

// statusJSON is the machine-readable form of `aisw status --json`, for
// scripts and AI agents. It never contains tokens.
type statusJSON struct {
	Tool     string        `json:"tool"`
	Title    string        `json:"title"`
	LoggedIn bool          `json:"logged_in"`
	Account  string        `json:"account,omitempty"`
	Profile  string        `json:"profile,omitempty"`
	Usage    *usageJSON    `json:"usage,omitempty"`
	Saved    []accountJSON `json:"saved"`
	Error    string        `json:"error,omitempty"`
}

// usageJSON: percent of the plan limit used in each window (null = unknown).
type usageJSON struct {
	FiveHourPercent *float64 `json:"five_hour_percent"`
	FiveHourResets  string   `json:"five_hour_resets_at,omitempty"`
	WeekPercent     *float64 `json:"week_percent"`
}

func toUsageJSON(u usageInfo) *usageJSON {
	if !u.known() {
		return nil
	}
	j := &usageJSON{}
	if u.FiveHour >= 0 {
		v := u.FiveHour
		j.FiveHourPercent = &v
		if u.FiveReset.After(time.Now()) {
			j.FiveHourResets = u.FiveReset.UTC().Format(time.RFC3339)
		}
	}
	if u.Week >= 0 {
		v := u.Week
		j.WeekPercent = &v
	}
	return j
}

type accountJSON struct {
	Profile string `json:"profile"`
	Account string `json:"account"`
	Active  bool   `json:"active"`
}

func cmdStatusJSON(m *Meta) error {
	var out []statusJSON
	for _, t := range tools {
		st := statusJSON{Tool: t.Name(), Title: t.Title(), Saved: []accountJSON{}}
		cur, err := t.Capture(false)
		curID := ""
		if err != nil {
			st.Error = err.Error()
		} else if cur != nil {
			id := t.Identify(cur)
			st.LoggedIn, st.Account, curID = true, id.Label, id.ID
			st.Profile = matchProfile(m, t.Name(), id.ID)
			st.Usage = toUsageJSON(usageFor(t, cur))
		}
		seen := map[string]bool{}
		for _, n := range sortedNames(m) {
			e, ok := m.Profiles[n][t.Name()]
			if n == backupName || !ok || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			st.Saved = append(st.Saved, accountJSON{Profile: n, Account: e.Label, Active: e.ID != "" && e.ID == curID})
		}
		out = append(out, st)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func cmdStatus(args []string) error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "--json" {
		return cmdStatusJSON(m)
	}
	for _, t := range tools {
		stop := spinner("Checking " + t.Title() + "…")
		snap, err := t.Capture(true)
		stop()
		if err != nil {
			row(t.Name(), "error: "+err.Error(), "")
			continue
		}
		if snap == nil {
			row(t.Name(), "not logged in", "")
			continue
		}
		id := t.Identify(snap)
		p := matchProfile(m, t.Name(), id.ID)
		tag := "(unsaved — aisw save <name>)"
		if p != "" {
			tag = "[" + p + "]"
		}
		if u := usageFor(t, snap); u.known() {
			tag += "  " + u.short()
		}
		row(t.Name(), id.Label, tag)
	}
	return nil
}

func cmdPrompt(_ []string) error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	var out []string
	for _, t := range tools {
		snap, err := t.Capture(false)
		if err != nil || snap == nil {
			continue
		}
		p := matchProfile(m, t.Name(), t.Identify(snap).ID)
		if p == "" {
			p = "?"
		}
		out = append(out, t.Name()+":"+p)
	}
	fmt.Println(strings.Join(out, " "))
	return nil
}

// isTTY: a person is at the keyboard (stdin and stdout are terminals).
func isTTY() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) }

var stdin = bufio.NewReader(os.Stdin)

func ask(q string) string {
	if !isTTY() {
		return ""
	}
	fmt.Print(q)
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

// cmdSave: `aisw save [name] [tool...]`. Without a name it proposes one.
func cmdSave(args []string) error {
	name := ""
	if len(args) > 0 && toolByName(args[0]) == nil {
		name, args = args[0], args[1:]
	}
	ts, err := parseTools(args)
	if err != nil {
		return err
	}
	m, err := loadMeta()
	if err != nil {
		return err
	}
	snaps := map[string]Snapshot{}
	for _, t := range ts {
		snap, err := t.Capture(true)
		if err != nil {
			return fmt.Errorf("%s: %v", t.Name(), err)
		}
		if snap == nil {
			row(t.Name(), "not logged in — skipped", "")
			continue
		}
		if unreliable(t, snap) {
			row(t.Name(), t.Identify(snap).Label, "not saved: login and account info disagree — switch with aisw first")
			continue
		}
		snaps[t.Name()] = snap
	}
	if len(snaps) == 0 {
		return errors.New("nothing to save — log in with one of the tools first")
	}
	if name == "" {
		suggestion := "main"
		for _, t := range ts {
			if s := snaps[t.Name()]; s != nil {
				if v := suggestName(t, s); v != "" {
					suggestion = v
				}
				break
			}
		}
		for _, t := range ts { // same account already saved? reuse that name
			if s := snaps[t.Name()]; s != nil {
				if p := matchProfile(m, t.Name(), t.Identify(s).ID); p != "" {
					suggestion = p
				}
				break
			}
		}
		name = suggestion
		if v := ask(fmt.Sprintf("Profile name [%s]: ", suggestion)); v != "" {
			name = v
		}
	}
	if !nameRe.MatchString(name) || name == backupName {
		return fmt.Errorf("invalid profile name %q (letters, digits, . _ -)", name)
	}
	saved := 0
	for _, t := range ts {
		snap := snaps[t.Name()]
		if snap == nil {
			continue
		}
		id := t.Identify(snap)
		if other := matchProfile(m, t.Name(), id.ID); other != "" && other != name {
			row(t.Name(), id.Label, "note: same account as ["+other+"]")
		}
		if err := store(m, t, name, snap); err != nil {
			return fmt.Errorf("%s: %v", t.Name(), err)
		}
		row(t.Name(), id.Label, "saved as ["+name+"]")
		saved++
	}
	if saved == 0 {
		return errors.New("nothing saved")
	}
	return saveMeta(m)
}

func switchTo(name string, args []string) error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	prof, ok := m.Profiles[name]
	if !ok && name == backupName {
		return errors.New("nothing to undo yet — no switch has been made")
	}
	if !ok {
		return fmt.Errorf("no profile named %q — see `aisw list`", name)
	}
	ts, err := parseTools(args)
	if err != nil {
		return err
	}
	switched := false
	any := false
	for _, t := range ts {
		entry, ok := prof[t.Name()]
		if !ok {
			continue
		}
		any = true
		target, err := blobGet(t.Name()+"/"+name, entry.Chunks)
		if err != nil {
			return fmt.Errorf("%s: %v", t.Name(), err)
		}
		stop := spinner("Switching " + t.Title() + "…")
		cur, err := t.Capture(true)
		stop()
		if err != nil {
			return fmt.Errorf("%s: %v", t.Name(), err)
		}
		if cur != nil && unreliable(t, cur) {
			cur = nil // mixed login: don't save it, don't call it active — just apply the target
		}
		if cur != nil {
			curID := t.Identify(cur).ID
			// Logins refresh their tokens over time: re-save the live login
			// into its own profile before leaving it, and keep an undo copy.
			if active := matchProfile(m, t.Name(), curID); active != "" && active != name {
				if err := store(m, t, active, cur); err != nil {
					return fmt.Errorf("%s: %v", t.Name(), err)
				}
			}
			if curID != "" && curID == entry.ID {
				if name != backupName {
					if err := store(m, t, name, cur); err != nil {
						return fmt.Errorf("%s: %v", t.Name(), err)
					}
				}
				row(t.Name(), entry.Label, "already active")
				continue
			}
			// target is already in memory, so this also makes `undo` a toggle
			if err := store(m, t, backupName, cur); err != nil {
				return fmt.Errorf("%s: %v", t.Name(), err)
			}
		}
		if err := saveMeta(m); err != nil {
			return err
		}
		if err := t.Apply(target); err != nil {
			return fmt.Errorf("%s: switch failed: %v (previous login kept as [%s])", t.Name(), err, backupName)
		}
		shown := name
		if name == backupName { // undo: show which saved account we went back to
			shown = "previous"
			if p := matchProfile(m, t.Name(), entry.ID); p != "" {
				shown = p
			}
		}
		row(t.Name(), entry.Label, "→ ["+shown+"]")
		if u := usageFor(t, target); u.known() {
			fmt.Printf("  %-11s limits: %s\n", "", u.short())
		}
		if n := runningSessions(t); n > 0 {
			var msg string
			if t.Name() == "claude" {
				// measured: Claude Code re-reads its Keychain login at most every 30 s
				msg = "Open Claude Code sessions move to this account within ~30 s (their status may still show the old name)."
			} else {
				msg = fmt.Sprintf("⚠ %d open %s session(s) may keep the old account until restarted.", n, t.Title())
			}
			fmt.Printf("  %-11s %s\n", "", msg)
		}
		switched = true
	}
	if !any {
		return fmt.Errorf("profile [%s] has none of the requested tools", name)
	}
	if err := saveMeta(m); err != nil {
		return err
	}
	if switched {
		fmt.Println("\n  New sessions use the new account.")
	}
	return nil
}

func cmdUse(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aisw use <name> [tool...]")
	}
	return switchTo(args[0], args[1:])
}

func cmdUndo(args []string) error { return switchTo(backupName, args) }

func cmdList(_ []string) error {
	m, err := loadMeta()
	if err != nil {
		return err
	}
	if len(m.Profiles) == 0 {
		fmt.Println("No profiles yet. Log in with each tool as usual, then: aisw save <name>")
		return nil
	}
	for _, n := range sortedNames(m) {
		if n == backupName {
			fmt.Println(n + "  (automatic: login before the last switch, used by `aisw undo`)")
		} else {
			fmt.Println(n)
		}
		ts := make([]string, 0)
		for t := range m.Profiles[n] {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		for _, t := range ts {
			e := m.Profiles[n][t]
			row(t, e.Label, time.Unix(e.Saved, 0).Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func cmdRm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aisw rm <name> [tool...]")
	}
	name := args[0]
	ts, err := parseTools(args[1:])
	if err != nil {
		return err
	}
	m, err := loadMeta()
	if err != nil {
		return err
	}
	prof, ok := m.Profiles[name]
	if !ok {
		return fmt.Errorf("no profile named %q", name)
	}
	for _, t := range ts {
		if e, ok := prof[t.Name()]; ok {
			blobDel(t.Name()+"/"+name, e.Chunks)
			delete(prof, t.Name())
			row(t.Name(), e.Label, "removed from ["+name+"]")
		}
	}
	if len(prof) == 0 {
		delete(m.Profiles, name)
	}
	return saveMeta(m)
}

func cmdDoctor(_ []string) error {
	fmt.Printf("aisw %s  %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("profiles: %s\nsecrets:  %s\n\n", metaPath(), secretStoreName())
	for _, t := range tools {
		snap, err := t.Capture(false)
		state := "not logged in"
		switch {
		case err != nil:
			state = "error: " + err.Error()
		case snap != nil:
			state = "logged in"
		}
		fmt.Printf("%s — %s\n", t.Name(), state)
		for _, n := range t.Notes() {
			fmt.Println("    " + n)
		}
	}
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return cmdMenu()
	}
	cmds := map[string]func([]string) error{
		"status": cmdStatus, "st": cmdStatus, "save": cmdSave, "use": cmdUse, "switch": cmdUse,
		"undo": cmdUndo, "add": cmdAdd, "list": cmdList, "ls": cmdList, "rm": cmdRm, "prompt": cmdPrompt,
		"doctor": cmdDoctor,
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	case "-v", "--version", "version":
		fmt.Println("aisw " + version)
		return nil
	}
	if c, ok := cmds[args[0]]; ok {
		return c(args[1:])
	}
	m, err := loadMeta()
	if err != nil {
		return err
	}
	if t := toolByName(args[0]); t != nil { // aisw <tool> [name]
		var names []string
		for _, n := range sortedNames(m) {
			if _, ok := m.Profiles[n][t.Name()]; ok && n != backupName {
				names = append(names, n)
			}
		}
		if len(args) == 1 {
			if len(names) == 0 {
				fmt.Printf("No saved %s accounts yet. Add one with: aisw add %s\n", t.Title(), t.Name())
			}
			for _, n := range names {
				row(t.Name(), m.Profiles[n][t.Name()].Label, n)
			}
			return nil
		}
		if _, ok := m.Profiles[args[1]][t.Name()]; !ok || args[1] == backupName {
			if len(names) == 0 {
				return fmt.Errorf("no saved %s accounts yet — add one with: aisw add %s", t.Title(), t.Name())
			}
			return fmt.Errorf("no %s account named %q (saved: %s)", t.Title(), args[1], strings.Join(names, ", "))
		}
		return switchTo(args[1], args[:1])
	}
	if m.Profiles[args[0]] != nil && args[0] != backupName {
		return switchTo(args[0], args[1:]) // aisw <profile> [tool...]
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "unknown command %q\n\n%s", args[0], usage)
	return errors.New(buf.String())
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aisw: "+err.Error())
		os.Exit(1)
	}
}
