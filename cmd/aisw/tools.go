package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Snapshot is everything needed to restore one tool's login.
type Snapshot map[string]string

type Identity struct {
	ID    string // stable account identifier used to match profiles
	Label string // human readable (email, plan, org)
}

type Tool interface {
	Name() string
	Capture(full bool) (Snapshot, error) // nil snapshot = not logged in
	Identify(Snapshot) Identity
	Apply(Snapshot) error
	// Clear removes the login from this machine only. It never calls the
	// tool's own logout, which revokes the session server-side and would
	// kill the saved profile.
	Clear() error
	Title() string      // display name, e.g. "Claude Code"
	LoginCmd() []string // the tool's own login command
	Notes() []string    // compatibility details for `doctor`
}

func loginHint(t Tool) string { return strings.Join(t.LoginCmd(), " ") }

var tools = []Tool{claudeTool{}, codexTool{}, cursorTool{}, copilotTool{}, opencodeTool{}, antigravityTool{}, kimiTool{}}

func toolByName(n string) Tool {
	for _, t := range tools {
		if t.Name() == n {
			return t
		}
	}
	return nil
}

// ------------------------------------------------------------- helpers --

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func removeOptional(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readOptional(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

func jwtClaims(tok string) map[string]interface{} {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var c map[string]interface{}
	_ = json.Unmarshal(raw, &c)
	return c
}

func str(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func last(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------------------------------------------------------- Claude Code --
// macOS: Keychain "Claude Code-credentials". Linux/Windows: ~/.claude/.credentials.json.
// The signed-in account info lives in ~/.claude.json under "oauthAccount".

type claudeTool struct{}

const claudeService = "Claude Code-credentials"

func (claudeTool) Name() string { return "claude" }

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(), ".claude")
}

func claudeCfgPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home(), ".claude.json")
}

func claudeCredPath() string { return filepath.Join(claudeDir(), ".credentials.json") }

func readClaudeCfg() (map[string]json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	s, ok, err := readOptional(claudeCfgPath())
	if err != nil || !ok {
		return cfg, err
	}
	return cfg, json.Unmarshal([]byte(s), &cfg)
}

// claudeKeychainAccount is the account Claude Code reads its Keychain item
// with: the OS user name. Other items with the same service (e.g. an old
// "root" one) are ignored by Claude Code, so aisw ignores them too.
func claudeKeychainAccount() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

func (claudeTool) Capture(bool) (Snapshot, error) {
	snap := Snapshot{}
	if runtime.GOOS == "darwin" {
		acct := claudeKeychainAccount()
		v, _, ok, err := secretGet(claudeService, acct)
		if err != nil {
			return nil, err
		}
		if ok && v != "" {
			snap["keychain"], snap["acct"] = v, acct
		}
	}
	if v, ok, err := readOptional(claudeCredPath()); err != nil {
		return nil, err
	} else if ok {
		snap["file"] = v
	}
	if len(snap) == 0 {
		return nil, nil
	}
	cfg, err := readClaudeCfg()
	if err != nil {
		return nil, err
	}
	if oa, ok := cfg["oauthAccount"]; ok {
		snap["oauthAccount"] = string(oa)
	}
	return snap, nil
}

// One email can belong to several organizations (e.g. a Team seat and a
// personal Max plan). They share accountUuid, so the organization is part of
// the identity.
func (claudeTool) Identify(s Snapshot) Identity {
	var oa map[string]interface{}
	_ = json.Unmarshal([]byte(s["oauthAccount"]), &oa)
	email := str(oa, "emailAddress")
	user := str(oa, "accountUuid")
	if user == "" {
		user = email
	}
	id := user
	if org := str(oa, "organizationUuid"); org != "" {
		id += ":" + org
	}
	label := email
	if plan := claudePlan(oa); plan != "" {
		label += " · " + plan
	}
	if claudeMismatch(s, oa) {
		label += " ⚠ login mismatch"
	}
	return Identity{ID: id, Label: label}
}

// claudeMismatch: the credential says one plan ("team", "max", ...) while
// the account info says another — e.g. a still-running Claude session wrote
// its own login back after a switch.
func claudeMismatch(s Snapshot, oa map[string]interface{}) bool {
	cred := s["keychain"]
	if cred == "" {
		cred = s["file"]
	}
	var c struct {
		O struct {
			Sub string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal([]byte(cred), &c) != nil || c.O.Sub == "" {
		return false
	}
	typ := strings.TrimPrefix(str(oa, "organizationType"), "claude_")
	return typ != "" && !strings.EqualFold(typ, c.O.Sub)
}

// claudePlan renders e.g. "Team (Acme)", "Max 5x", "Pro".
func claudePlan(oa map[string]interface{}) string {
	typ := strings.TrimPrefix(str(oa, "organizationType"), "claude_")
	if typ == "" {
		return ""
	}
	plan := strings.ToUpper(typ[:1]) + typ[1:]
	if m := regexp.MustCompile(`max_(\d+x)`).FindStringSubmatch(str(oa, "organizationRateLimitTier")); m != nil {
		plan += " " + m[1]
	}
	// personal plans get an auto-named org ("x's Organization"); only show real names
	if name := str(oa, "organizationName"); name != "" && !strings.HasSuffix(name, "'s Organization") &&
		!strings.Contains(name, str(oa, "emailAddress")) {
		plan += " (" + name + ")"
	}
	return plan
}

// suggestName proposes a short profile name from a snapshot.
func suggestName(t Tool, s Snapshot) string {
	base := ""
	if t.Name() == "claude" {
		var oa map[string]interface{}
		_ = json.Unmarshal([]byte(s["oauthAccount"]), &oa)
		typ := strings.TrimPrefix(str(oa, "organizationType"), "claude_")
		name := str(oa, "organizationName")
		if (typ == "team" || typ == "enterprise") && name != "" {
			base = name
		} else {
			base = typ
		}
	}
	if base == "" {
		switch t.Name() {
		case "opencode":
			base = "keys"
		case "kimi":
			base = "kimi"
		default:
			base = strings.SplitN(t.Identify(s).Label, "@", 2)[0]
		}
	}
	base = strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9_.-]+`).ReplaceAllString(base, "-"))
	return strings.Trim(base, "-.")
}

func (claudeTool) Apply(s Snapshot) error {
	if v := s["keychain"]; v != "" && runtime.GOOS == "darwin" {
		acct := claudeKeychainAccount() // always the item Claude Code reads
		if err := secretPutVerified(claudeService, acct, v); err != nil {
			return err
		}
	}
	if v := s["file"]; v != "" {
		if err := os.MkdirAll(claudeDir(), 0o700); err != nil {
			return err
		}
		if err := atomicWrite(claudeCredPath(), []byte(v), 0o600); err != nil {
			return err
		}
	}
	cfg, err := readClaudeCfg()
	if err != nil {
		return err
	}
	if oa := s["oauthAccount"]; oa != "" {
		cfg["oauthAccount"] = json.RawMessage(oa)
	} else {
		delete(cfg, "oauthAccount")
	}
	return writeJSON(claudeCfgPath(), cfg)
}

func writeJSON(path string, v interface{}) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return atomicWrite(path, buf.Bytes(), 0o600)
}

func (claudeTool) Clear() error {
	if runtime.GOOS == "darwin" {
		_ = secretDel(claudeService, claudeKeychainAccount())
	}
	if err := removeOptional(claudeCredPath()); err != nil {
		return err
	}
	cfg, err := readClaudeCfg()
	if err != nil {
		return err
	}
	if _, ok := cfg["oauthAccount"]; !ok {
		return nil
	}
	delete(cfg, "oauthAccount")
	return writeJSON(claudeCfgPath(), cfg)
}

func (claudeTool) Title() string      { return "Claude Code" }
func (claudeTool) LoginCmd() []string { return []string{"claude", "auth", "login"} }

func (claudeTool) Notes() []string {
	n := []string{"credentials: " + map[bool]string{true: "Keychain \"" + claudeService + "\" + ", false: ""}[runtime.GOOS == "darwin"] + claudeCredPath(),
		"account info: " + claudeCfgPath() + " (oauthAccount)"}
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" && runtime.GOOS == "darwin" {
		n = append(n, "warning: CLAUDE_CONFIG_DIR is set; on macOS Claude may use a differently named Keychain item")
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		n = append(n, "warning: ANTHROPIC_API_KEY is set and overrides the logged-in account")
	}
	return n
}

// ---------------------------------------------------------------- Codex --
// All platforms: $CODEX_HOME/auth.json (default ~/.codex/auth.json).

type codexTool struct{}

func (codexTool) Name() string { return "codex" }

func codexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".codex")
}

func codexAuthPath() string { return filepath.Join(codexHome(), "auth.json") }

func (codexTool) Capture(bool) (Snapshot, error) {
	v, ok, err := readOptional(codexAuthPath())
	if err != nil || !ok {
		return nil, err
	}
	return Snapshot{"auth": v}, nil
}

func (codexTool) Identify(s Snapshot) Identity {
	var a struct {
		Key    string `json:"OPENAI_API_KEY"`
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal([]byte(s["auth"]), &a) != nil {
		return Identity{}
	}
	c := jwtClaims(a.Tokens.IDToken)
	if email := str(c, "email"); email != "" {
		label := email
		if auth, ok := c["https://api.openai.com/auth"].(map[string]interface{}); ok {
			if plan := str(auth, "chatgpt_plan_type"); plan != "" {
				label += " · " + strings.ToUpper(plan[:1]) + plan[1:]
			}
		}
		return Identity{ID: a.Tokens.AccountID + ":" + email, Label: label}
	}
	if a.Key != "" {
		return Identity{ID: "key:" + last(a.Key, 6), Label: "API key …" + last(a.Key, 4)}
	}
	return Identity{}
}

func (codexTool) Apply(s Snapshot) error {
	if err := os.MkdirAll(codexHome(), 0o700); err != nil {
		return err
	}
	return atomicWrite(codexAuthPath(), []byte(s["auth"]), 0o600)
}

func (codexTool) Clear() error { return removeOptional(codexAuthPath()) }

func (codexTool) Title() string      { return "Codex" }
func (codexTool) LoginCmd() []string { return []string{"codex", "login"} }

func (codexTool) Notes() []string {
	n := []string{"credentials: " + codexAuthPath()}
	if cfg, ok, _ := readOptional(filepath.Join(codexHome(), "config.toml")); ok &&
		regexp.MustCompile(`(?m)^\s*cli_auth_credentials_store\s*=\s*"(keyring|auto)"`).MatchString(cfg) {
		n = append(n, "warning: config.toml stores Codex credentials in the OS keyring; aisw only switches auth.json")
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		n = append(n, "note: OPENAI_API_KEY is set in this shell")
	}
	return n
}

// ------------------------------------------------------------ Cursor CLI --
// macOS: Keychain "cursor-access-token" / "cursor-refresh-token".
// Windows: %APPDATA%\Cursor\auth.json. Linux: $XDG_CONFIG_HOME/cursor/auth.json.
// (Paths taken from cursor-agent's own credential code.)

type cursorTool struct{}

var cursorServices = []string{"cursor-access-token", "cursor-refresh-token"}

func (cursorTool) Name() string { return "cursor" }

func cursorAuthPath() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(home(), "AppData", "Roaming")
		}
		return filepath.Join(base, "Cursor", "auth.json")
	case "darwin":
		return filepath.Join(home(), ".cursor", "auth.json")
	default:
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home(), ".config")
		}
		return filepath.Join(base, "cursor", "auth.json")
	}
}

var emailRe = regexp.MustCompile(`[\w.+-]+@[\w-]+(\.[\w-]+)+`)

func cursorEmail() string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, exe := range []string{"cursor-agent", "agent"} {
		out, _ := exec.CommandContext(ctx, exe, "status").CombinedOutput()
		if m := emailRe.FindString(string(out)); m != "" {
			return m
		}
	}
	return ""
}

func (cursorTool) Capture(full bool) (Snapshot, error) {
	snap := Snapshot{}
	if runtime.GOOS == "darwin" {
		for _, svc := range cursorServices {
			v, acct, ok, err := secretGet(svc, "")
			if err != nil {
				return nil, err
			}
			if ok {
				snap[svc], snap[svc+"@acct"] = v, acct
			}
		}
	}
	if v, ok, err := readOptional(cursorAuthPath()); err != nil {
		return nil, err
	} else if ok {
		snap["file"] = v
	}
	if len(snap) == 0 {
		return nil, nil
	}
	if full {
		snap["email"] = cursorEmail()
	}
	return snap, nil
}

func (cursorTool) Identify(s Snapshot) Identity {
	tok := s["cursor-access-token"]
	var f struct {
		AccessToken string `json:"accessToken"`
		APIKey      string `json:"apiKey"`
	}
	_ = json.Unmarshal([]byte(s["file"]), &f)
	if tok == "" {
		tok = f.AccessToken
	}
	id := str(jwtClaims(tok), "sub")
	if id == "" && f.APIKey != "" {
		id = "key:" + last(f.APIKey, 6)
	}
	label := s["email"]
	if label == "" {
		label = id
	}
	return Identity{ID: id, Label: label}
}

func (cursorTool) Apply(s Snapshot) error {
	if runtime.GOOS == "darwin" {
		for _, svc := range cursorServices {
			if v, ok := s[svc]; ok {
				acct := s[svc+"@acct"]
				if acct == "" {
					acct = "cursor-user"
				}
				if err := secretPutVerified(svc, acct, v); err != nil {
					return err
				}
			}
		}
	}
	if v := s["file"]; v != "" {
		if err := os.MkdirAll(filepath.Dir(cursorAuthPath()), 0o700); err != nil {
			return err
		}
		return atomicWrite(cursorAuthPath(), []byte(v), 0o600)
	}
	return nil
}

func (cursorTool) Clear() error {
	if runtime.GOOS == "darwin" {
		for _, svc := range cursorServices {
			if _, acct, ok, _ := secretGet(svc, ""); ok {
				_ = secretDel(svc, acct)
			}
		}
	}
	return removeOptional(cursorAuthPath())
}

func (cursorTool) Title() string      { return "Cursor CLI" }
func (cursorTool) LoginCmd() []string { return []string{"cursor-agent", "login"} }

func (cursorTool) Notes() []string {
	n := []string{}
	if runtime.GOOS == "darwin" {
		n = append(n, "credentials: Keychain \"cursor-access-token\", \"cursor-refresh-token\"")
	}
	n = append(n, "credentials file: "+cursorAuthPath(),
		"note: only the Cursor CLI (cursor-agent) is switched; the Cursor editor app has its own login")
	if os.Getenv("CURSOR_API_KEY") != "" {
		n = append(n, "warning: CURSOR_API_KEY is set and overrides the logged-in account")
	}
	return n
}
