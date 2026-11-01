package main

// aisw cost — what each project cost, per tool, across every AI CLI.
//
// The CLIs keep per-request token counts in their own logs, but they also
// delete old logs (Claude Code after 30 days by default) and worktree folders
// disappear. aisw therefore copies every usage record it sees into its own
// ledger (~/.aisw/ledger.jsonl.gz), resolving the project at that moment, and
// reports from the ledger. Costs are computed at report time from token
// counts, so price corrections apply to history too. No credentials are read.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------- ledger --

type usageRec struct {
	ID      string  `json:"id"`
	Tool    string  `json:"tool"`
	Project string  `json:"project"`
	Dir     string  `json:"dir,omitempty"` // working directory, for regrouping later
	Model   string  `json:"model"`
	Time    int64   `json:"t"`             // unix seconds
	Count   int     `json:"n"`             // requests (or 1 session for Codex)
	In      int64   `json:"in"`            // uncached input tokens
	Out     int64   `json:"out"`           // output tokens (incl. reasoning)
	CRead   int64   `json:"cr,omitempty"`  // cache reads
	CWrite  int64   `json:"cw,omitempty"`  // cache writes (5-minute / default)
	CWrite1 int64   `json:"cw1,omitempty"` // cache writes (1-hour)
	USD     float64 `json:"usd,omitempty"` // cost reported by the tool itself
	HasUSD  bool    `json:"has_usd,omitempty"`
	AIU     float64 `json:"aiu,omitempty"` // GitHub Copilot AI units reported by Copilot
	Src     string  `json:"src,omitempty"` // where the record came from when not the CLI's own log
}

func (r usageRec) tokens() int64 { return r.In + r.Out + r.CRead + r.CWrite + r.CWrite1 }

// The ledger is gzip-compressed JSON lines. New records are appended as extra
// gzip members (readers see one stream); superseded lines are dropped by a
// full rewrite when they pile up.
func ledgerPath() string    { return filepath.Join(stateDir(), "ledger.jsonl.gz") }
func oldLedgerPath() string { return filepath.Join(stateDir(), "ledger.jsonl") } // before compression
func ingestPath() string    { return filepath.Join(stateDir(), "ingest.json") }

type ledger struct {
	recs    map[string]usageRec
	lines   int
	rewrite bool // write a fresh compressed copy on the next flush
}

func loadLedger() (*ledger, error) {
	l := &ledger{recs: map[string]usageRec{}}
	if err := l.read(oldLedgerPath(), false); err != nil {
		return nil, err
	}
	if l.lines > 0 { // migrate the uncompressed ledger
		l.rewrite = true
	}
	if err := l.read(ledgerPath(), true); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *ledger) read(path string, compressed bool) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var src io.Reader = f
	if compressed {
		zr, err := gzip.NewReader(f) // reads every appended member
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		defer zr.Close()
		src = zr
	}
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var r usageRec
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			l.recs[r.ID] = r // later lines win (Codex sessions grow)
			l.lines++
		}
	}
	return sc.Err()
}

// add returns true when the record is new or changed.
func (l *ledger) add(r usageRec, out *bytes.Buffer) {
	if old, ok := l.recs[r.ID]; ok {
		// keep the project of the first sighting: the folder may be gone later
		if old.Project != "" {
			r.Project = old.Project
		}
		if old == r {
			return
		}
	}
	l.recs[r.ID] = r
	b, _ := json.Marshal(r)
	out.Write(b)
	out.WriteByte('\n')
	l.lines++
}

func gz(data []byte) []byte {
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	zw.Write(data)
	zw.Close()
	return out.Bytes()
}

func (l *ledger) flush(buf *bytes.Buffer) error {
	stale := l.lines - len(l.recs)
	if buf.Len() == 0 && !l.rewrite && stale <= len(l.recs)/5 {
		return nil
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	// superseded lines over 20% (or a migration): write only current records
	if l.rewrite || stale > len(l.recs)/5 || !fileExists(ledgerPath()) {
		var all bytes.Buffer
		ids := make([]string, 0, len(l.recs))
		for id := range l.recs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b, _ := json.Marshal(l.recs[id])
			all.Write(b)
			all.WriteByte('\n')
		}
		if err := atomicWrite(ledgerPath(), gz(all.Bytes()), 0o600); err != nil {
			return err
		}
		l.lines, l.rewrite = len(l.recs), false
		return removeOptional(oldLedgerPath())
	}
	f, err := os.OpenFile(ledgerPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(gz(buf.Bytes()))
	return err
}

// ------------------------------------------------------------ projects --

var projectCache = map[string]string{}

// projectOf names the project a working directory belongs to: the git
// repository (all worktrees of one repo count as one project), else the
// folder name. Works without git installed and for deleted folders.
func projectOf(cwd string) string {
	if cwd == "" {
		return "(unknown)"
	}
	cwd = filepath.Clean(cwd)
	if p, ok := projectCache[cwd]; ok {
		return p
	}
	p := resolveProject(cwd)
	projectCache[cwd] = p
	return p
}

func resolveProject(cwd string) string {
	h := home()
	slash := filepath.ToSlash(cwd)
	// Claude Code's per-session temp folders encode the real project:
	// /private/tmp/claude-501/-Users-me-DEV-Shop/<session>/scratchpad/…
	if m := claudeTmpRe.FindStringSubmatch(slash); m != nil {
		if dir := decodeClaudeSlug(m[1]); dir != "" {
			return resolveProject(dir)
		}
	}
	if slash == "/" || slash == "/tmp" || slash == "/private/tmp" {
		return "(temporary)"
	}
	// worktree folders managed by tools: <...>/worktrees/<Repo>-sc-<name>-<id>
	for _, marker := range []string{"/.super.engineering/worktrees/", "/.superconductor/worktrees/"} {
		if i := strings.Index(slash, marker); i >= 0 {
			name := strings.SplitN(slash[i+len(marker):], "/", 2)[0]
			if j := strings.Index(name, "-sc-"); j > 0 {
				name = name[:j]
			}
			return name
		}
	}
	if i := strings.Index(slash, "/.claude/worktrees/"); i >= 0 { // Claude Code worktrees live inside the repo
		return resolveProject(filepath.FromSlash(slash[:i]))
	}
	for _, internal := range []string{"/.super.engineering/", "/.superconductor/", "/.claude/", "/.codex/"} {
		if strings.Contains(slash, internal) {
			return "(tool internal)"
		}
	}
	// walk up to the git repository root
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if dir == h || dir == filepath.Dir(dir) {
			break
		}
		g := filepath.Join(dir, ".git")
		fi, err := os.Stat(g)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			return climbGeneric(dir)
		}
		// worktree: ".git" file with "gitdir: <repo>/.git/worktrees/<name>"
		if b, err := os.ReadFile(g); err == nil {
			s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if i := strings.Index(filepath.ToSlash(s), "/.git/worktrees/"); i > 0 {
				return climbGeneric(filepath.FromSlash(filepath.ToSlash(s)[:i]))
			}
		}
		return climbGeneric(dir)
	}
	if cwd == h {
		return "(home)"
	}
	return climbGeneric(cwd)
}

// climbGeneric names a folder, skipping generic sub-folder names such as
// src, app or backend: ~/code/my-app/app → "my-app".
func climbGeneric(dir string) string {
	h := home()
	for genericDirs[strings.ToLower(filepath.Base(dir))] && filepath.Dir(dir) != h && filepath.Dir(dir) != dir {
		dir = filepath.Dir(dir)
	}
	return filepath.Base(dir)
}

var claudeTmpRe = regexp.MustCompile(`/claude-\d+/(-[^/]+)/`)

// decodeClaudeSlug turns "-Users-me-DEV-my-app" back into a path. Dashes are
// ambiguous (separator or part of a name), so it follows existing folders and
// keeps the rest of the name together.
func decodeClaudeSlug(slug string) string {
	parts := strings.Split(strings.TrimPrefix(slug, "-"), "-")
	dir := string(filepath.Separator)
	for i := 0; i < len(parts); {
		found := false
		for j := len(parts); j > i; j-- { // longest existing name first
			cand := filepath.Join(dir, strings.Join(parts[i:j], "-"))
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				dir, i, found = cand, j, true
				break
			}
		}
		if !found {
			return filepath.Join(dir, strings.Join(parts[i:], "-"))
		}
	}
	return dir
}

var genericDirs = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`src source sources app apps api backend frontend server client web www
		public public_html htdocs httpdocs static assets images img media headers internal pkg cmd lib libs packages package core modules
		module application tools scripts script bin build dist out tmp temp test tests spec docs doc
		ios android macos mobile desktop engine re ghidra decompiled fastlane appstore screenshots
		metadata resources res config tr en de fr es`) {
		m[n] = true
	}
	return m
}()

// -------------------------------------------------------------- ingest --

type fileState struct {
	Size int64 `json:"size"`
	Mod  int64 `json:"mod"`
}

// fileDelta reports whether a file changed and from which byte offset it can
// be read again: logs only grow, so a larger file is read from the old end.
func fileDelta(st map[string]fileState, path string) (changed bool, from int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, 0
	}
	prev, seen := st[path]
	cur := fileState{fi.Size(), fi.ModTime().UnixNano()}
	if seen && prev == cur {
		return false, 0
	}
	st[path] = cur
	if seen && cur.Size >= prev.Size {
		return true, prev.Size
	}
	return true, 0
}

func loadIngestState() map[string]fileState {
	st := map[string]fileState{}
	if b, err := os.ReadFile(ingestPath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// ingest copies new usage records from every tool into the ledger.
func ingest() (*ledger, []string, error) {
	l, err := loadLedger()
	if err != nil {
		return nil, nil, err
	}
	st := loadIngestState()
	var buf bytes.Buffer
	var notes []string

	// Claude Code: one record per API request; only the new end of a log is read
	for _, f := range walkFiles(filepath.Join(claudeDir(), "projects"), ".jsonl") {
		if ok, from := fileDelta(st, f); ok {
			ingestClaudeFile(f, from, l, &buf)
		}
	}
	// Codex: one record per session (cumulative totals, so the whole file)
	for _, f := range walkFiles(filepath.Join(codexHome(), "sessions"), ".jsonl") {
		if ok, _ := fileDelta(st, f); ok {
			ingestCodexFile(f, l, &buf)
		}
	}
	// GitHub Copilot and OpenCode keep SQLite databases
	if _, err := exec.LookPath("sqlite3"); err == nil {
		if db := filepath.Join(copilotHome(), "session-store.db"); fileExists(db) {
			ingestCopilot(db, l, &buf)
		}
		if db := filepath.Join(filepath.Dir(opencodeAuthPath()), "opencode.db"); fileExists(db) {
			ingestOpenCode(db, l, &buf)
		}
		if db := superEngineeringDB(); db != "" {
			importSuperEngineering(db, l, &buf, st)
		}
	} else {
		notes = append(notes, "sqlite3 not found: GitHub Copilot and OpenCode usage skipped")
	}

	if err := l.flush(&buf); err != nil {
		return nil, nil, err
	}
	b, _ := json.Marshal(st)
	if err := atomicWrite(ingestPath(), b, 0o600); err != nil {
		return nil, nil, err
	}
	return l, notes, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func walkFiles(root, ext string) []string {
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ext) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func parseTime(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.Unix()
	}
	return 0
}

func ingestClaudeFile(path string, from int64, l *ledger, buf *bytes.Buffer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, 0); err != nil {
			return
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var d struct {
			RequestID string `json:"requestId"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					In    int64 `json:"input_tokens"`
					Out   int64 `json:"output_tokens"`
					CRead int64 `json:"cache_read_input_tokens"`
					CW    int64 `json:"cache_creation_input_tokens"`
					CC    *struct {
						W5 int64 `json:"ephemeral_5m_input_tokens"`
						W1 int64 `json:"ephemeral_1h_input_tokens"`
					} `json:"cache_creation"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &d) != nil || d.Message.Usage == nil {
			continue
		}
		m := d.Message.Model
		if m == "" || strings.HasPrefix(m, "<") {
			continue
		}
		key := d.RequestID
		if key == "" {
			key = d.Message.ID
		}
		if key == "" {
			continue
		}
		u := d.Message.Usage
		w5, w1 := u.CW, int64(0)
		if u.CC != nil && (u.CC.W5 > 0 || u.CC.W1 > 0) {
			w5, w1 = u.CC.W5, u.CC.W1
		}
		l.add(usageRec{ID: "claude:" + key, Tool: "claude", Project: projectOf(d.Cwd), Dir: d.Cwd, Model: m,
			Time: parseTime(d.Timestamp), Count: 1, In: u.In, Out: u.Out, CRead: u.CRead, CWrite: w5, CWrite1: w1}, buf)
	}
}

func ingestCodexFile(path string, l *ledger, buf *bytes.Buffer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var id, cwd, model string
	var t int64
	var tot *struct {
		In     int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Out    int64 `json:"output_tokens"`
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.Contains(line, []byte(`"session_meta"`)) && id == "":
			var d struct {
				Payload struct {
					ID        string `json:"id"`
					Cwd       string `json:"cwd"`
					Timestamp string `json:"timestamp"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &d) == nil {
				id, cwd, t = d.Payload.ID, d.Payload.Cwd, parseTime(d.Payload.Timestamp)
			}
		case bytes.Contains(line, []byte(`"turn_context"`)):
			var d struct {
				Payload struct {
					Model string `json:"model"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &d) == nil && d.Payload.Model != "" {
				model = d.Payload.Model
			}
		case bytes.Contains(line, []byte(`"token_count"`)):
			var d struct {
				Payload struct {
					Info *struct {
						Total *struct {
							In     int64 `json:"input_tokens"`
							Cached int64 `json:"cached_input_tokens"`
							Out    int64 `json:"output_tokens"`
						} `json:"total_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &d) == nil && d.Payload.Info != nil && d.Payload.Info.Total != nil {
				tot = d.Payload.Info.Total
			}
		}
	}
	if id == "" || tot == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	// OpenAI counts cached input inside input_tokens
	l.add(usageRec{ID: "codex:" + id, Tool: "codex", Project: projectOf(cwd), Dir: cwd, Model: model, Time: t, Count: 1,
		In: tot.In - tot.Cached, CRead: tot.Cached, Out: tot.Out}, buf)
}

func sqliteJSON(db, query string) []map[string]interface{} {
	out, err := exec.Command("sqlite3", "-readonly", "-json", "file:"+db+"?mode=ro", query).Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var rows []map[string]interface{}
	_ = json.Unmarshal(out, &rows)
	return rows
}

func num(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func ingestCopilot(db string, l *ledger, buf *bytes.Buffer) {
	rows := sqliteJSON(db, `select e.id, e.session_id, e.model, e.input_tokens, e.output_tokens,
		e.cache_read_tokens, e.cache_write_tokens, e.total_nano_aiu, e.created_at, s.cwd
		from assistant_usage_events e left join sessions s on s.id = e.session_id`)
	for _, r := range rows {
		in, cr, cw := int64(num(r["input_tokens"])), int64(num(r["cache_read_tokens"])), int64(num(r["cache_write_tokens"]))
		cwd, _ := r["cwd"].(string)
		model, _ := r["model"].(string)
		at, _ := r["created_at"].(string)
		sid, _ := r["session_id"].(string)
		// Copilot's input_tokens include the cached part
		l.add(usageRec{ID: fmt.Sprintf("copilot:%s:%v", sid, r["id"]), Tool: "copilot", Project: projectOf(cwd), Dir: cwd,
			Model: model, Time: parseTime(at), Count: 1, In: in - cr - cw, CRead: cr, CWrite: cw,
			Out: int64(num(r["output_tokens"])), AIU: num(r["total_nano_aiu"]) / 1e9}, buf)
	}
}

func ingestOpenCode(db string, l *ledger, buf *bytes.Buffer) {
	rows := sqliteJSON(db, `select id, data from message where json_extract(data,'$.role') = 'assistant'`)
	for _, r := range rows {
		id, _ := r["id"].(string)
		data, _ := r["data"].(string)
		var d struct {
			ModelID  string   `json:"modelID"`
			Provider string   `json:"providerID"`
			Cost     *float64 `json:"cost"`
			Tokens   struct {
				In        int64 `json:"input"`
				Out       int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Path struct {
				Cwd string `json:"cwd"`
			} `json:"path"`
			Time struct {
				Created int64 `json:"created"`
			} `json:"time"`
		}
		if id == "" || json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		rec := usageRec{ID: "opencode:" + id, Tool: "opencode", Project: projectOf(d.Path.Cwd), Dir: d.Path.Cwd, Model: d.ModelID,
			Time: d.Time.Created / 1000, Count: 1, In: d.Tokens.In, Out: d.Tokens.Out + d.Tokens.Reasoning,
			CRead: d.Tokens.Cache.Read, CWrite: d.Tokens.Cache.Write}
		if d.Cost != nil {
			rec.USD, rec.HasUSD = *d.Cost, true
		}
		l.add(rec, buf)
	}
}

// -------------------------------------------------------------- prices --

// price per million tokens. CacheRead < 0 means "0.1 × input";
// CacheWrite < 0 means "1.25 × input" (Anthropic's 5-minute cache write).
type price struct {
	In, Out, CacheRead, CacheWrite float64
}

// Anthropic first-party API list prices. Cache writes cost 1.25× input
// (5-minute) or 2× input (1-hour).
var builtinPrices = map[string]price{
	"claude-fable-5-1":  {10, 50, 0.25, -1},
	"claude-mythos-5-1": {10, 50, 0.25, -1},
	"claude-fable-5":    {10, 50, 1.00, -1},
	"claude-mythos-5":   {10, 50, 1.00, -1},
	"claude-opus-5-5":   {4, 20, 0.20, -1},
	"claude-opus-5":     {5, 25, -1, -1},
	"claude-opus-4-8":   {5, 25, -1, -1},
	"claude-opus-4-7":   {5, 25, -1, -1},
	"claude-opus-4-6":   {5, 25, -1, -1},
	"claude-sonnet-5-5": {2, 10, 0.20, -1},
	"claude-sonnet-5":   {2, 10, -1, -1},
	"claude-sonnet-4-6": {3, 15, -1, -1},
	"claude-haiku-5-5":  {0.10, 0.50, -1, -1},
	"claude-haiku-4-5":  {1, 5, -1, -1},
}

func pricesPath() string { return filepath.Join(stateDir(), "prices.json") }

// loadPrices: ~/.aisw/prices.json (yours) > built-in Anthropic prices >
// models.dev catalog. Format of prices.json: {"gpt-6.1": {"input": 1.25,
// "output": 10, "cache_read": 0.125}} — dollars per million tokens, matched
// by prefix.
func loadPrices() map[string]price {
	p := map[string]price{}
	for k, v := range catalogPrices() {
		p[k] = v
	}
	for k, v := range builtinPrices {
		p[k] = v
	}
	b, err := os.ReadFile(pricesPath())
	if err != nil {
		return p
	}
	var user map[string]struct {
		In         float64  `json:"input"`
		Out        float64  `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	}
	if json.Unmarshal(b, &user) == nil {
		for k, v := range user {
			cr, cw := -1.0, -1.0
			if v.CacheRead != nil {
				cr = *v.CacheRead
			}
			if v.CacheWrite != nil {
				cw = *v.CacheWrite
			}
			p[k] = price{v.In, v.Out, cr, cw}
		}
	}
	return p
}

// priceMemo remembers each model's price: the catalog has thousands of
// entries and a report prices tens of thousands of records.
var priceMemo = map[string]struct {
	p  price
	ok bool
	n  int
}{}

func findPrice(prices map[string]price, model string) (price, bool) {
	if m, ok := priceMemo[model]; ok && m.n == len(prices) {
		return m.p, m.ok
	}
	p, ok := findPriceSlow(prices, model)
	priceMemo[model] = struct {
		p  price
		ok bool
		n  int
	}{p, ok, len(prices)}
	return p, ok
}

func findPriceSlow(prices map[string]price, model string) (price, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	best := ""
	for k := range prices {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return price{}, false
	}
	return prices[best], true
}

// costOf returns the API-equivalent dollars of a record, or false if unknown.
func costOf(r usageRec, prices map[string]price) (float64, bool) {
	if r.HasUSD {
		return r.USD, true
	}
	p, ok := findPrice(prices, r.Model)
	if !ok {
		return 0, false
	}
	cr, cw := p.CacheRead, p.CacheWrite
	if cr < 0 {
		cr = p.In * 0.1
	}
	if cw < 0 {
		cw = p.In * 1.25
	}
	return (float64(r.In)*p.In + float64(r.Out)*p.Out + float64(r.CRead)*cr +
		float64(r.CWrite)*cw + float64(r.CWrite1)*p.In*2) / 1e6, true
}

// maybeIngest keeps the ledger current from any aisw command, so usage is
// saved before the CLIs delete their logs (Claude Code: after 30 days).
func maybeIngest() {
	if fi, err := os.Stat(ingestPath()); err == nil && time.Since(fi.ModTime()) < 6*time.Hour {
		return
	}
	msg := "Saving AI usage history…"
	if !fileExists(ingestPath()) {
		msg = "Saving AI usage history (first time, a few seconds)…"
	}
	stop := spinner(msg)
	_, _, _ = ingest()
	stop()
}
