package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// ------------------------------------------------------------- OpenCode --
// All platforms: $XDG_DATA_HOME/opencode/auth.json (default ~/.local/share/opencode/auth.json).
// One file holds every provider key/login, so a profile is the whole file.

type opencodeTool struct{}

func (opencodeTool) Name() string { return "opencode" }

func opencodeAuthPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(home(), ".local", "share")
	}
	return filepath.Join(base, "opencode", "auth.json")
}

func (opencodeTool) Capture(bool) (Snapshot, error) {
	v, ok, err := readOptional(opencodeAuthPath())
	if err != nil || !ok {
		return nil, err
	}
	return Snapshot{"auth": v}, nil
}

func (opencodeTool) Identify(s Snapshot) Identity {
	var auth map[string]map[string]interface{}
	if json.Unmarshal([]byte(s["auth"]), &auth) != nil || len(auth) == 0 {
		return Identity{}
	}
	providers := make([]string, 0, len(auth))
	for p := range auth {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	// identity = providers + the tail of each long-lived secret, so two
	// profiles with the same providers but different keys stay distinct
	h := sha256.New()
	for _, p := range providers {
		e := auth[p]
		secret := str(e, "key")
		if secret == "" {
			secret = str(e, "accountId") + str(e, "refresh")
		}
		fmt.Fprintf(h, "%s:%s:%s;", p, str(e, "type"), last(secret, 8))
	}
	label := fmt.Sprintf("%d providers (%s)", len(providers), strings.Join(providers, ", "))
	if len(providers) == 1 {
		label = "1 provider (" + providers[0] + ")"
	}
	if len(label) > 42 {
		label = label[:39] + "…)"
	}
	return Identity{ID: hex.EncodeToString(h.Sum(nil))[:16], Label: label}
}

func (opencodeTool) Apply(s Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(opencodeAuthPath()), 0o700); err != nil {
		return err
	}
	return atomicWrite(opencodeAuthPath(), []byte(s["auth"]), 0o600)
}

func (opencodeTool) Clear() error { return removeOptional(opencodeAuthPath()) }

func (opencodeTool) Title() string      { return "OpenCode" }
func (opencodeTool) LoginCmd() []string { return []string{"opencode", "auth", "login"} }

func (opencodeTool) Notes() []string {
	return []string{"credentials: " + opencodeAuthPath() + " (all provider keys and logins)"}
}

// ---------------------------------------------------------- Copilot CLI --
// Copilot keeps one stored login per GitHub user itself and picks the one in
// $COPILOT_HOME/config.json "last_logged_in_user". aisw only changes that
// pointer and never touches Copilot's tokens.

var acctPattern = regexp.MustCompile(`"acct"<blob>="((?:[^"\\]|\\.)*)"`)

type copilotTool struct{}

func (copilotTool) Name() string { return "copilot" }

func copilotHome() string {
	if d := os.Getenv("COPILOT_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".copilot")
}

func copilotCfgPath() string { return filepath.Join(copilotHome(), "config.json") }

type ghUser struct {
	Host  string `json:"host"`
	Login string `json:"login"`
}

// config.json starts with "//" comment lines and, depending on the Copilot
// version, uses camelCase (lastLoggedInUser) or snake_case (last_logged_in_user).
func readCopilotCfg() (cfg map[string]json.RawMessage, header string, err error) {
	cfg = map[string]json.RawMessage{}
	s, ok, err := readOptional(copilotCfgPath())
	if err != nil || !ok {
		return cfg, "", err
	}
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) && (strings.HasPrefix(strings.TrimSpace(lines[i]), "//") || strings.TrimSpace(lines[i]) == "") {
		i++
	}
	header = strings.Join(lines[:i], "\n")
	body := strings.Join(lines[i:], "\n")
	if strings.TrimSpace(body) == "" {
		return cfg, header, nil
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return nil, "", fmt.Errorf("cannot read %s: %v", copilotCfgPath(), err)
	}
	return cfg, header, nil
}

func copilotKeys(cfg map[string]json.RawMessage) (current, all string) {
	if _, ok := cfg["last_logged_in_user"]; ok {
		return "last_logged_in_user", "logged_in_users"
	}
	if _, ok := cfg["logged_in_users"]; ok {
		return "last_logged_in_user", "logged_in_users"
	}
	return "lastLoggedInUser", "loggedInUsers"
}

// copilotKeychainUser finds the stored login without reading the token.
func copilotKeychainUser() *ghUser {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", "copilot-cli").Output()
	if err != nil {
		return nil
	}
	m := acctPattern.FindStringSubmatch(string(out))
	if m == nil {
		return nil
	}
	i := strings.LastIndex(m[1], ":")
	if i <= 0 {
		return nil
	}
	return &ghUser{Host: m[1][:i], Login: m[1][i+1:]}
}

func (copilotTool) Capture(bool) (Snapshot, error) {
	cfg, _, err := readCopilotCfg()
	if err != nil {
		return nil, err
	}
	cur, all := copilotKeys(cfg)
	var u ghUser
	if raw, ok := cfg[cur]; ok {
		_ = json.Unmarshal(raw, &u)
	}
	if u.Login == "" {
		var users []ghUser
		_ = json.Unmarshal(cfg[all], &users)
		if len(users) > 0 {
			u = users[0]
		} else if k := copilotKeychainUser(); k != nil {
			u = *k
		}
	}
	if u.Login == "" {
		return nil, nil
	}
	if u.Host == "" {
		u.Host = "https://github.com"
	}
	return Snapshot{"host": u.Host, "login": u.Login}, nil
}

func (copilotTool) Identify(s Snapshot) Identity {
	if s["login"] == "" {
		return Identity{}
	}
	label := s["login"]
	if s["host"] != "https://github.com" {
		label += " (" + strings.TrimPrefix(s["host"], "https://") + ")"
	}
	return Identity{ID: s["host"] + ":" + s["login"], Label: label}
}

func (copilotTool) Apply(s Snapshot) error {
	if s["login"] == "" {
		return errors.New("profile has no GitHub user")
	}
	cfg, header, err := readCopilotCfg()
	if err != nil {
		return err
	}
	cur, all := copilotKeys(cfg)
	u := ghUser{Host: s["host"], Login: s["login"]}
	var users []ghUser
	_ = json.Unmarshal(cfg[all], &users)
	found := false
	for _, x := range users {
		if x == u {
			found = true
		}
	}
	if !found {
		users = append(users, u)
	}
	cfg[cur], _ = json.Marshal(u)
	cfg[all], _ = json.Marshal(users)
	if err := os.MkdirAll(copilotHome(), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if header != "" {
		body = append([]byte(header+"\n"), body...)
	}
	if err := atomicWrite(copilotCfgPath(), append(body, '\n'), 0o600); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		if err := exec.Command("/usr/bin/security", "find-generic-password", "-s", "copilot-cli",
			"-a", u.Host+":"+u.Login).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  note: Copilot has no stored login for %s — run `copilot login` once with that account\n", u.Login)
		}
	}
	return nil
}

// Copilot keeps every account it has logged in to, so nothing is cleared:
// `copilot login` simply adds the new account next to the old one.
func (copilotTool) Clear() error { return nil }

func (copilotTool) Title() string      { return "GitHub Copilot" }
func (copilotTool) LoginCmd() []string { return []string{"copilot", "login"} }

func (copilotTool) Notes() []string {
	n := []string{"active user: " + copilotCfgPath() + " (lastLoggedInUser)",
		"tokens stay in Copilot's own store; log in once per account with `copilot login`"}
	for _, v := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if os.Getenv(v) != "" {
			n = append(n, "warning: "+v+" is set and overrides the stored login")
		}
	}
	return n
}
