package main

// Extra sources for `aisw cost`:
//
//   models.dev  — the public price catalog (https://models.dev/api.json) for
//                 models aisw has no built-in price for (OpenAI, Moonshot, …).
//   super.engineering — its usage database keeps history the CLIs have
//                 already deleted; aisw imports what it doesn't have yet.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ------------------------------------------------------------ models.dev --

const modelsDevURL = "https://models.dev/api.json"

func catalogPath() string { return filepath.Join(stateDir(), "models-dev-prices.json") }

// catalogPrice is dollars per million tokens; -1 = not given.
type catalogPrice struct {
	In         float64 `json:"in"`
	Out        float64 `json:"out"`
	CacheRead  float64 `json:"cr"`
	CacheWrite float64 `json:"cw"`
}

// refreshCatalog downloads models.dev at most once a day and keeps only the
// prices. Failures are reported but never fatal: the last copy is used.
func refreshCatalog() string {
	if os.Getenv("AISW_OFFLINE") != "" {
		return ""
	}
	if fi, err := os.Stat(catalogPath()); err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
		return ""
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(modelsDevURL)
	if err != nil {
		return "price catalog (models.dev) not updated: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Sprintf("price catalog (models.dev) not updated: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "price catalog (models.dev) not updated: " + err.Error()
	}
	var raw map[string]struct {
		Models map[string]struct {
			Cost *struct {
				In         *float64 `json:"input"`
				Out        *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "price catalog (models.dev) not updated: " + err.Error()
	}
	out := map[string]map[string]catalogPrice{}
	val := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	for prov, p := range raw {
		for id, m := range p.Models {
			if m.Cost == nil || m.Cost.In == nil || m.Cost.Out == nil {
				continue
			}
			if out[prov] == nil {
				out[prov] = map[string]catalogPrice{}
			}
			out[prov][id] = catalogPrice{*m.Cost.In, *m.Cost.Out, val(m.Cost.CacheRead), val(m.Cost.CacheWrite)}
		}
	}
	b, _ := json.Marshal(out)
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err.Error()
	}
	if err := atomicWrite(catalogPath(), b, 0o600); err != nil {
		return err.Error()
	}
	return ""
}

// catalogPrices maps model ids to prices, preferring the model's own
// provider when the same id is listed by resellers too.
func catalogPrices() map[string]price {
	b, err := os.ReadFile(catalogPath())
	if err != nil {
		return nil
	}
	var cat map[string]map[string]catalogPrice
	if json.Unmarshal(b, &cat) != nil {
		return nil
	}
	out := map[string]price{}
	put := func(prov string, overwrite bool) {
		for id, c := range cat[prov] {
			if _, ok := out[id]; ok && !overwrite {
				continue
			}
			out[id] = price{In: c.In, Out: c.Out, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}
		}
	}
	for prov := range cat { // resellers first, …
		put(prov, false)
	}
	for _, prov := range []string{"google", "moonshotai", "openai", "anthropic"} { // … then first parties win
		put(prov, true)
	}
	return out
}

// ----------------------------------------------------- super.engineering --

func superEngineeringDB() string {
	if os.Getenv("AISW_NO_IMPORT") != "" {
		return ""
	}
	for _, p := range []string{
		filepath.Join(home(), ".super.engineering", "db", "super.engineering.db"),
		filepath.Join(home(), ".superconductor", "db", "superconductor.db"),
	} {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// importSuperEngineering adds usage that super.engineering recorded and the
// ledger lacks (typically logs the CLIs deleted before aisw was installed).
// Records already in the ledger are never changed: those came from the logs.
func importSuperEngineering(db string, l *ledger, buf *bytes.Buffer, st map[string]fileState) int {
	since := st["super.engineering:claude"].Mod
	added := 0
	rows := sqliteJSON(db, fmt.Sprintf(`select f.fact_key, f.model_id, f.occurred_at_ms, f.input_tokens, f.output_tokens,
		f.cache_read_tokens, f.cache_write_tokens, s.transcript_cwd
		from usage_facts f left join usage_sessions s on s.provider_key = f.provider_key and s.session_key = f.session_key
		where f.provider_key = 'claude' and coalesce(f.occurred_at_ms, 0) > %d`, since))
	maxT := since
	for _, r := range rows {
		key, _ := r["fact_key"].(string)
		id := ""
		for _, part := range strings.Split(key, ":") {
			if strings.HasPrefix(part, "req_") {
				id = part
			}
		}
		if id == "" {
			for _, part := range strings.Split(key, ":") {
				if strings.HasPrefix(part, "msg_") {
					id = part
				}
			}
		}
		at := int64(num(r["occurred_at_ms"]))
		if at > maxT {
			maxT = at
		}
		model, _ := r["model_id"].(string)
		if id == "" || model == "" || strings.HasPrefix(model, "<") {
			continue
		}
		if _, ok := l.recs["claude:"+id]; ok {
			continue
		}
		cwd, _ := r["transcript_cwd"].(string)
		l.add(usageRec{ID: "claude:" + id, Tool: "claude", Project: projectOf(cwd), Dir: cwd, Model: model,
			Time: at / 1000, Count: 1, In: int64(num(r["input_tokens"])), Out: int64(num(r["output_tokens"])),
			CRead: int64(num(r["cache_read_tokens"])), CWrite: int64(num(r["cache_write_tokens"])),
			Src: "super.engineering"}, buf)
		added++
	}
	st["super.engineering:claude"] = fileState{Mod: maxT}

	// Codex: super.engineering keeps one row per turn; aisw keeps one per
	// session, so sum per session and add sessions aisw doesn't have.
	rows = sqliteJSON(db, `select coalesce(s.session_alias, f.session_key) as sid, s.transcript_cwd as cwd,
		min(f.occurred_at_ms) as t, count(*) as n, sum(f.input_tokens) as i, sum(f.output_tokens) as o,
		sum(f.cache_read_tokens) as cr, sum(f.cache_write_tokens) as cw,
		(select f2.model_id from usage_facts f2 where f2.provider_key = 'codex' and f2.session_key = f.session_key
		 group by f2.model_id order by count(*) desc limit 1) as model
		from usage_facts f left join usage_sessions s on s.provider_key = f.provider_key and s.session_key = f.session_key
		where f.provider_key = 'codex' group by f.session_key`)
	for _, r := range rows {
		sid, _ := r["sid"].(string)
		if sid == "" {
			continue
		}
		if _, ok := l.recs["codex:"+sid]; ok {
			continue
		}
		cwd, _ := r["cwd"].(string)
		model, _ := r["model"].(string)
		if model == "" {
			model = "unknown"
		}
		l.add(usageRec{ID: "codex:" + sid, Tool: "codex", Project: projectOf(cwd), Dir: cwd, Model: model,
			Time: int64(num(r["t"])) / 1000, Count: 1, In: int64(num(r["i"])), Out: int64(num(r["o"])),
			CRead: int64(num(r["cr"])), CWrite: int64(num(r["cw"])), Src: "super.engineering"}, buf)
		added++
	}
	return added
}
