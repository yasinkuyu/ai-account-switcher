package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Every test runs against throwaway tool directories and a test-only
// credential-store service, so it never touches real logins.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "aisw-test-home-")
	if err != nil {
		panic(err)
	}
	// HOME stays as it is (macOS finds the login keychain through it); each
	// tool is pointed at the temp dir through its own variable instead.
	os.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	os.Setenv("COPILOT_HOME", filepath.Join(home, ".copilot"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Setenv("KIMI_SHARE_DIR", filepath.Join(home, ".kimi"))
	os.Setenv("AISW_HOME", filepath.Join(home, ".aisw"))
	ownService = fmt.Sprintf("aisw-test-%d", os.Getpid())
	// Only tools whose login lives in files under HOME take part in
	// end-to-end tests; the others read real OS keychain entries.
	tools = []Tool{codexTool{}, opencodeTool{}, copilotTool{}}

	code := m.Run()

	if meta, err := loadMeta(); err == nil {
		for name, ts := range meta.Profiles {
			for t, e := range ts {
				blobDel(t+"/"+name, e.Chunks)
			}
		}
	}
	os.RemoveAll(home)
	os.Exit(code)
}

func fakeJWT(claims map[string]interface{}) string {
	b, _ := json.Marshal(claims)
	return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
}

func codexAuth(email, plan, account string) string {
	tok := fakeJWT(map[string]interface{}{"email": email,
		"https://api.openai.com/auth": map[string]interface{}{"chatgpt_plan_type": plan}})
	return fmt.Sprintf(`{"tokens":{"id_token":%q,"account_id":%q,"refresh_token":"r-%s"}}`, tok, account, account)
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resetState(t *testing.T) *Meta {
	t.Helper()
	if m, err := loadMeta(); err == nil {
		for name, ts := range m.Profiles {
			for tl, e := range ts {
				blobDel(tl+"/"+name, e.Chunks)
			}
		}
	}
	os.RemoveAll(stateDir())
	m, err := loadMeta()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ------------------------------------------------------------- identity --

func TestCodexIdentity(t *testing.T) {
	id := codexTool{}.Identify(Snapshot{"auth": codexAuth("a@b.com", "pro", "acc1")})
	if id.Label != "a@b.com · Pro" || id.ID != "acc1:a@b.com" {
		t.Fatalf("got %+v", id)
	}
	key := codexTool{}.Identify(Snapshot{"auth": `{"OPENAI_API_KEY":"sk-abcdef123456"}`})
	if key.ID != "key:123456" || key.Label != "API key …3456" {
		t.Fatalf("api key identity: %+v", key)
	}
}

func claudeSnap(orgType, orgUUID, orgName, tier, sub string) Snapshot {
	oa, _ := json.Marshal(map[string]string{"accountUuid": "user-1", "emailAddress": "me@x.com",
		"organizationUuid": orgUUID, "organizationType": orgType, "organizationName": orgName,
		"organizationRateLimitTier": tier})
	return Snapshot{"oauthAccount": string(oa),
		"keychain": fmt.Sprintf(`{"claudeAiOauth":{"subscriptionType":%q}}`, sub)}
}

func TestClaudeTeamAndMaxAreDifferentAccounts(t *testing.T) {
	team := claudeTool{}.Identify(claudeSnap("claude_team", "org-team", "Acme", "default_raven", "team"))
	max := claudeTool{}.Identify(claudeSnap("claude_max", "org-max", "me@x.com's Organization", "default_claude_max_5x", "max"))
	if team.ID == max.ID {
		t.Fatal("Team and Max on one email must not share an identity")
	}
	if team.Label != "me@x.com · Team (Acme)" || max.Label != "me@x.com · Max 5x" {
		t.Fatalf("labels: %q / %q", team.Label, max.Label)
	}
}

func TestClaudeMismatchIsFlagged(t *testing.T) {
	// account info says Team, credential says Max (e.g. an open session wrote its login back)
	id := claudeTool{}.Identify(claudeSnap("claude_team", "org-team", "Acme", "", "max"))
	if !strings.Contains(id.Label, "mismatch") {
		t.Fatalf("expected a mismatch warning, got %q", id.Label)
	}
}

func TestOpenCodeIdentityDependsOnKeys(t *testing.T) {
	a := opencodeTool{}.Identify(Snapshot{"auth": `{"openrouter":{"type":"api","key":"sk-or-AAAAAAAA"}}`})
	b := opencodeTool{}.Identify(Snapshot{"auth": `{"openrouter":{"type":"api","key":"sk-or-BBBBBBBB"}}`})
	if a.ID == b.ID || a.Label != "1 provider (openrouter)" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestAutoNameAvoidsCollisions(t *testing.T) {
	m := &Meta{Version: metaVersion, Profiles: map[string]map[string]ToolEntry{
		"you": {"codex": {ID: "acc1:you@a.com"}},
	}}
	same := autoName(m, codexTool{}, Snapshot{"auth": codexAuth("you@a.com", "plus", "acc1")})
	other := autoName(m, codexTool{}, Snapshot{"auth": codexAuth("you@b.com", "plus", "acc2")})
	if same != "you" || other != "you-2" {
		t.Fatalf("same=%q other=%q", same, other)
	}
}

// --------------------------------------------------------- secret store --

func TestBlobRoundTripLarge(t *testing.T) {
	snap := Snapshot{"big": strings.Repeat("x", 9000), "unicode": "çğıöşü → ✓"}
	n, err := blobPut("test/blob", snap)
	if err != nil {
		t.Fatal(err)
	}
	defer blobDel("test/blob", n)
	if n < 2 {
		t.Fatalf("expected several chunks, got %d", n)
	}
	got, err := blobGet("test/blob", n)
	if err != nil || got["big"] != snap["big"] || got["unicode"] != snap["unicode"] {
		t.Fatalf("round trip failed: %v", err)
	}
}

// --------------------------------------------------------------- copilot --

func TestCopilotKeepsCommentsAndKeyStyle(t *testing.T) {
	cfg := "// User settings belong in settings.json.\n// This file is managed automatically.\n" +
		`{"firstLaunchAt":"2026-03-11T00:00:00.000Z","lastLoggedInUser":{"host":"https://github.com","login":"alice"},` +
		`"loggedInUsers":[{"host":"https://github.com","login":"alice"}]}`
	writeFile(t, copilotCfgPath(), cfg)
	cur, err := copilotTool{}.Capture(false)
	if err != nil || cur["login"] != "alice" {
		t.Fatalf("capture: %v %v", cur, err)
	}
	if err := (copilotTool{}).Apply(Snapshot{"host": "https://github.com", "login": "bob"}); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, copilotCfgPath())
	if !strings.HasPrefix(out, "// User settings belong in settings.json.\n// This file is managed automatically.\n{") {
		t.Fatalf("comment header lost:\n%s", out)
	}
	for _, want := range []string{`"lastLoggedInUser"`, `"login": "bob"`, `"login": "alice"`, `"firstLaunchAt"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "last_logged_in_user") {
		t.Fatal("must keep the camelCase style the file already uses")
	}
	now, _ := copilotTool{}.Capture(false)
	if now["login"] != "bob" {
		t.Fatalf("active user should be bob, got %v", now)
	}
}

// ------------------------------------------------------------- switching --

func TestSaveSwitchUndo(t *testing.T) {
	resetState(t)
	authA := codexAuth("a@x.com", "plus", "accA")
	authB := codexAuth("b@x.com", "team", "accB")

	writeFile(t, codexAuthPath(), authA)
	if err := cmdSave([]string{"alpha", "codex"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, codexAuthPath(), authB)
	if err := cmdSave([]string{"beta", "codex"}); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"codex", "alpha"}); err != nil { // aisw <tool> <name>
		t.Fatal(err)
	}
	if readFile(t, codexAuthPath()) != authA {
		t.Fatal("switch to alpha did not restore A")
	}
	if err := cmdUndo(nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, codexAuthPath()) != authB {
		t.Fatal("undo did not go back to B")
	}
	if err := cmdUndo(nil); err != nil { // undo toggles
		t.Fatal(err)
	}
	if readFile(t, codexAuthPath()) != authA {
		t.Fatal("second undo did not toggle back to A")
	}
}

func TestSwitchResavesRefreshedLogin(t *testing.T) {
	resetState(t)
	writeFile(t, codexAuthPath(), codexAuth("a@x.com", "plus", "accA"))
	if err := cmdSave([]string{"alpha", "codex"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, codexAuthPath(), codexAuth("b@x.com", "plus", "accB"))
	if err := cmdSave([]string{"beta", "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := switchTo("alpha", []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	// the tool refreshes its tokens while alpha is active (same account, new secret)
	refreshed := strings.Replace(codexAuth("a@x.com", "plus", "accA"), "r-accA", "r-accA-v2", 1)
	writeFile(t, codexAuthPath(), refreshed)
	if err := switchTo("beta", []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if err := switchTo("alpha", []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, codexAuthPath()) != refreshed {
		t.Fatal("the refreshed login of alpha was not kept when switching away")
	}
}

func TestStatusJSONHasNoSecrets(t *testing.T) {
	m := resetState(t)
	writeFile(t, codexAuthPath(), codexAuth("a@x.com", "plus", "accA"))
	if err := cmdSave([]string{"alpha", "codex"}); err != nil {
		t.Fatal(err)
	}
	m, _ = loadMeta()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := cmdStatusJSON(m)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	var parsed []statusJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if strings.Contains(out, "r-accA") || strings.Contains(out, "id_token") {
		t.Fatalf("status --json leaked a credential:\n%s", out)
	}
}

// ------------------------------------------------------------ add account --

// fakeLogin puts a fake `codex` on PATH that either fails or logs in as b@x.com.
func fakeLogin(t *testing.T, succeed bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the fake login command")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if succeed {
		script = "#!/bin/sh\ncat > \"$CODEX_HOME_FAKE\" <<'EOF'\n" + codexAuth("b@x.com", "team", "accB") + "\nEOF\n"
	}
	writeFile(t, filepath.Join(dir, "codex"), script)
	os.Chmod(filepath.Join(dir, "codex"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME_FAKE", codexAuthPath())
}

func TestAddAccountCancelledRestoresOldLogin(t *testing.T) {
	m := resetState(t)
	authA := codexAuth("a@x.com", "plus", "accA")
	writeFile(t, codexAuthPath(), authA)
	fakeLogin(t, false)
	if err := addAccount(m, codexTool{}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, codexAuthPath()) != authA {
		t.Fatal("a cancelled login must restore the previous account")
	}
}

func TestAddAccountSavesNewLogin(t *testing.T) {
	m := resetState(t)
	writeFile(t, codexAuthPath(), codexAuth("a@x.com", "plus", "accA"))
	fakeLogin(t, true)
	if err := addAccount(m, codexTool{}); err != nil {
		t.Fatal(err)
	}
	m, _ = loadMeta()
	views, err := loadViews(m, false)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, v := range views {
		if v.tool.Name() == "codex" {
			for _, a := range v.accounts {
				labels = append(labels, fmt.Sprintf("%s active=%v", a.entry.Label, a.active))
			}
		}
	}
	got := strings.Join(labels, "; ")
	if !strings.Contains(got, "a@x.com · Plus active=false") || !strings.Contains(got, "b@x.com · Team active=true") {
		t.Fatalf("expected both accounts with b active, got: %s", got)
	}
}

func TestToolNameErrorsAreHelpful(t *testing.T) {
	resetState(t)
	writeFile(t, codexAuthPath(), codexAuth("a@x.com", "plus", "accA"))
	if err := cmdSave([]string{"alpha", "codex"}); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"codex", "nosuch"})
	if err == nil || !strings.Contains(err.Error(), `no Codex account named "nosuch" (saved: alpha)`) {
		t.Fatalf("got %v", err)
	}
	if err := run([]string{"codex"}); err != nil { // lists the accounts
		t.Fatal(err)
	}
	if err := run([]string{"opencode", "x"}); err == nil || !strings.Contains(err.Error(), "aisw add opencode") {
		t.Fatalf("got %v", err)
	}
	if err := run([]string{"_previous"}); err == nil {
		t.Fatal("the internal undo slot must not be addressable by name")
	}
}

func TestMixedClaudeLoginIsNeverSaved(t *testing.T) {
	mixed := claudeSnap("claude_team", "org-team", "Acme", "", "max")
	if !unreliable(claudeTool{}, mixed) {
		t.Fatal("a Team label with a Max credential must be unreliable")
	}
	if unreliable(claudeTool{}, claudeSnap("claude_team", "org-team", "Acme", "", "team")) {
		t.Fatal("a consistent login must be reliable")
	}
	m := &Meta{Version: metaVersion, Profiles: map[string]map[string]ToolEntry{}}
	if name, err := autoSave(m, claudeTool{}, mixed); err != nil || name != "" || len(m.Profiles) != 0 {
		t.Fatalf("mixed login was auto-saved: %q %v", name, err)
	}
}

func TestCodexUsageFromLogs(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Unix()
	past := time.Now().Add(-2 * time.Hour).Unix()
	log := `{"type":"session_meta","payload":{"creator_account_id":"accU"}}
{"timestamp":"2026-11-01T00:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":42,"resets_at":` + fmt.Sprint(future) + `},"secondary":{"used_percent":12,"resets_at":` + fmt.Sprint(past) + `}}}}
`
	writeFile(t, filepath.Join(codexHome(), "sessions", "2026", "11", "01", "rollout-x.jsonl"), log)
	u := codexUsage(Snapshot{"auth": codexAuth("u@x.com", "plus", "accU")})
	if u.FiveHour != 42 || u.Week != 0 {
		t.Fatalf("got 5h=%v wk=%v (week window already reset, so 0 expected)", u.FiveHour, u.Week)
	}
	other := codexUsage(Snapshot{"auth": codexAuth("o@x.com", "plus", "accOther")})
	if other.known() {
		t.Fatal("usage of one account must not be shown for another")
	}
}
