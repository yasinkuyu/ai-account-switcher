package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type agg struct {
	USD      float64 // priced part
	Unpriced int64   // tokens with no known price
	Tokens   int64
	Count    int
	AIU      float64
}

func (a *agg) add(r usageRec, prices map[string]price) {
	a.Tokens += r.tokens()
	a.Count += r.Count
	a.AIU += r.AIU
	if c, ok := costOf(r, prices); ok {
		a.USD += c
	} else {
		a.Unpriced += r.tokens()
	}
}

var costToolOrder = []string{"claude", "codex", "copilot", "opencode"}

var costToolTitle = map[string]string{"claude": "Claude Code", "codex": "Codex", "copilot": "Copilot", "opencode": "OpenCode"}

func money(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	intPart, frac := s[:len(s)-3], s[len(s)-3:]
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return "$" + b.String() + frac
}

func tok(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// showTokens switches the tables from dollars to token counts (--tokens).
var showTokens bool

// cell shows dollars, with "+" when part of the usage has no known price,
// or just the token count when nothing is priced.
func (a agg) cell() string {
	if a.Tokens == 0 && a.Count == 0 {
		return "—"
	}
	if showTokens {
		return tok(a.Tokens)
	}
	if a.USD == 0 && a.Unpriced > 0 {
		return tok(a.Tokens) + " tok"
	}
	s := money(a.USD)
	if a.Unpriced > 0 {
		s += "+"
	}
	return s
}

func parseSince(s string) (int64, error) {
	if m := regexp.MustCompile(`^(\d+)([hdwm])$`).FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "m": 30 * 24 * time.Hour}[m[2]]
		return time.Now().Add(-time.Duration(n) * unit).Unix(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("--since: use e.g. 7d, 12h, 4w, 3m or 2026-10-01 (got %q)", s)
}

func cmdCost(args []string) error {
	var target string
	var since int64
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
			asJSON = true
		case a == "--tokens":
			showTokens = true
		case a == "--since" && i+1 < len(args):
			s, err := parseSince(args[i+1])
			if err != nil {
				return err
			}
			since, i = s, i+1
		case strings.HasPrefix(a, "--since="):
			s, err := parseSince(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return err
			}
			since = s
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %q (use: aisw cost [project|.] [--tokens] [--since 30d] [--json])", a)
		default:
			target = a
		}
	}
	enableVT()
	stop := spinner("Reading usage logs and prices…")
	catNote := refreshCatalog()
	l, notes, err := ingest()
	stop()
	if catNote != "" {
		notes = append(notes, catNote)
	}
	imported := 0
	for _, r := range l.recs {
		if r.Src != "" {
			imported++
		}
	}
	if imported > 0 {
		notes = append(notes, fmt.Sprintf("%d older records were recovered from super.engineering's history.", imported))
	}
	if err != nil {
		return err
	}
	prices := loadPrices()

	var recs []usageRec
	first := int64(0)
	for _, r := range l.recs {
		if since > 0 && r.Time > 0 && r.Time < since {
			continue
		}
		recs = append(recs, r)
		if r.Time > 0 && (first == 0 || r.Time < first) {
			first = r.Time
		}
	}
	if target == "." {
		wd, _ := os.Getwd()
		target = projectOf(wd)
	}
	if target != "" {
		return costDetail(target, recs, prices, asJSON, notes)
	}
	return costOverview(recs, prices, asJSON, first, notes)
}

func costOverview(recs []usageRec, prices map[string]price, asJSON bool, first int64, notes []string) error {
	byProject := map[string]map[string]*agg{}
	totals := map[string]*agg{}
	grand := &agg{}
	for _, r := range recs {
		if byProject[r.Project] == nil {
			byProject[r.Project] = map[string]*agg{}
		}
		if byProject[r.Project][r.Tool] == nil {
			byProject[r.Project][r.Tool] = &agg{}
		}
		if totals[r.Tool] == nil {
			totals[r.Tool] = &agg{}
		}
		byProject[r.Project][r.Tool].add(r, prices)
		totals[r.Tool].add(r, prices)
		grand.add(r, prices)
	}
	type row struct {
		name  string
		tools map[string]*agg
		total agg
	}
	var rows []row
	for name, ts := range byProject {
		rw := row{name: name, tools: ts}
		for _, a := range ts {
			rw.total.USD += a.USD
			rw.total.Unpriced += a.Unpriced
			rw.total.Tokens += a.Tokens
			rw.total.Count += a.Count
			rw.total.AIU += a.AIU
		}
		rows = append(rows, rw)
	}
	sort.Slice(rows, func(i, j int) bool {
		ii, jj := strings.HasPrefix(rows[i].name, "("), strings.HasPrefix(rows[j].name, "(")
		if ii != jj {
			return jj
		}
		if rows[i].total.USD != rows[j].total.USD {
			return rows[i].total.USD > rows[j].total.USD
		}
		return rows[i].total.Tokens > rows[j].total.Tokens
	})
	var cols []string
	for _, t := range costToolOrder {
		if totals[t] != nil {
			cols = append(cols, t)
		}
	}

	if asJSON {
		type toolJSON struct {
			USD            float64 `json:"usd"`
			UnpricedTokens int64   `json:"unpriced_tokens"`
			Tokens         int64   `json:"tokens"`
			Requests       int     `json:"requests"`
			CopilotAIU     float64 `json:"copilot_ai_units,omitempty"`
		}
		conv := func(a *agg) toolJSON {
			return toolJSON{a.USD, a.Unpriced, a.Tokens, a.Count, a.AIU}
		}
		type projJSON struct {
			Project string              `json:"project"`
			Tools   map[string]toolJSON `json:"tools"`
			Total   toolJSON            `json:"total"`
		}
		out := struct {
			Since    string              `json:"since,omitempty"`
			Projects []projJSON          `json:"projects"`
			Tools    map[string]toolJSON `json:"tools"`
			Total    toolJSON            `json:"total"`
			Note     string              `json:"note"`
		}{Tools: map[string]toolJSON{}, Total: conv(grand),
			Note: "usd = API-equivalent cost at list prices; unpriced_tokens have no known price"}
		if first > 0 {
			out.Since = time.Unix(first, 0).UTC().Format(time.RFC3339)
		}
		for _, rw := range rows {
			pj := projJSON{Project: rw.name, Tools: map[string]toolJSON{}, Total: conv(&rw.total)}
			for t, a := range rw.tools {
				pj.Tools[t] = conv(a)
			}
			out.Projects = append(out.Projects, pj)
		}
		for t, a := range totals {
			out.Tools[t] = conv(a)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	if len(rows) == 0 {
		fmt.Println("No AI usage found yet.")
		return nil
	}
	nameW := len("All projects")
	for _, rw := range rows {
		if n := len([]rune(rw.name)); n > nameW {
			nameW = n
		}
	}
	if nameW > 32 {
		nameW = 32
	}
	const colW = 13
	pad := func(s string, w int, right bool) string {
		n := len([]rune(s))
		if n > w {
			return string([]rune(s)[:w-1]) + "…"
		}
		if right {
			return strings.Repeat(" ", w-n) + s
		}
		return s + strings.Repeat(" ", w-n)
	}
	head := "  " + pad("Project", nameW, false)
	for _, c := range cols {
		head += pad(costToolTitle[c], colW, true)
	}
	head += pad("Total", colW+1, true)
	fmt.Println(bold(head))
	for _, rw := range rows {
		line := "  " + pad(rw.name, nameW, false)
		for _, c := range cols {
			cell := "—"
			if a := rw.tools[c]; a != nil {
				cell = a.cell()
			}
			line += pad(cell, colW, true)
		}
		line += pad(rw.total.cell(), colW+1, true)
		fmt.Println(line)
	}
	fmt.Println("  " + strings.Repeat("─", nameW+colW*len(cols)+colW+1))
	line := "  " + pad("All projects", nameW, false)
	for _, c := range cols {
		line += pad(totals[c].cell(), colW, true)
	}
	line += pad(grand.cell(), colW+1, true)
	fmt.Println(bold(line))

	fmt.Println()
	if showTokens {
		fmt.Println(dim("  Tokens of every kind: input, output (incl. reasoning), cache reads and cache writes."))
	} else {
		fmt.Println(dim("  API-equivalent cost at list prices. With a subscription you pay your plan, not this. Tokens: --tokens"))
	}
	if grand.Unpriced > 0 && !showTokens {
		fmt.Println(dim("  + / \"tok\": " + tok(grand.Unpriced) + " tokens have no known price — add prices to " + pricesPath()))
	}
	if grand.AIU > 0 {
		fmt.Println(dim(fmt.Sprintf("  Copilot also reports %.1f GitHub AI units (not converted to dollars).", grand.AIU)))
	}
	fmt.Println(dim("  Prices: Anthropic list prices built in; other models from models.dev (refreshed daily)."))
	fmt.Println(dim("  Cursor, Antigravity, Kimi and Grok keep no token counts locally, so they are not included."))
	if first > 0 {
		fmt.Println(dim("  History since " + time.Unix(first, 0).Format("2006-01-02") + ", kept in " + ledgerPath() + ". Details: aisw cost <project>"))
	}
	for _, n := range notes {
		fmt.Println(dim("  " + n))
	}
	return nil
}

func costDetail(target string, recs []usageRec, prices map[string]price, asJSON bool, notes []string) error {
	// match the project name case-insensitively
	names := map[string]string{}
	for _, r := range recs {
		names[strings.ToLower(r.Project)] = r.Project
	}
	project, ok := names[strings.ToLower(target)]
	if !ok {
		var all []string
		for _, n := range names {
			all = append(all, n)
		}
		sort.Strings(all)
		if len(all) > 12 {
			all = append(all[:12], "…")
		}
		return fmt.Errorf("no AI usage recorded for project %q (projects: %s)", target, strings.Join(all, ", "))
	}
	type key struct{ tool, model string }
	by := map[key]*agg{}
	tools := map[string]*agg{}
	total := &agg{}
	var cr, cw, in, out = map[key]int64{}, map[key]int64{}, map[key]int64{}, map[key]int64{}
	for _, r := range recs {
		if r.Project != project {
			continue
		}
		k := key{r.Tool, r.Model}
		if by[k] == nil {
			by[k] = &agg{}
		}
		if tools[r.Tool] == nil {
			tools[r.Tool] = &agg{}
		}
		by[k].add(r, prices)
		tools[r.Tool].add(r, prices)
		total.add(r, prices)
		in[k] += r.In
		out[k] += r.Out
		cr[k] += r.CRead
		cw[k] += r.CWrite + r.CWrite1
	}
	if asJSON {
		type m struct {
			Tool, Model                      string
			Requests                         int
			Input, Output, CacheRead, CacheW int64
			USD                              float64
			Unpriced                         int64
		}
		var rows []m
		for k, a := range by {
			rows = append(rows, m{k.tool, k.model, a.Count, in[k], out[k], cr[k], cw[k], a.USD, a.Unpriced})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].USD > rows[j].USD })
		b, _ := json.MarshalIndent(map[string]interface{}{"project": project, "models": rows,
			"total_usd": total.USD, "unpriced_tokens": total.Unpriced}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	// every amount ends on the same right edge
	const width = 98 // 52 (model, count, tokens) + 32 (breakdown) + 14 (cost)
	right := func(label, value string) string {
		return label + strings.Repeat(" ", max1(width-len([]rune(label))-len([]rune(value)))) + value
	}
	fmt.Println(bold("  "+project) + dim("  —  AI cost by tool and model (API-equivalent)"))
	for _, t := range costToolOrder {
		ta := tools[t]
		if ta == nil {
			continue
		}
		fmt.Println()
		fmt.Println(bold(right("  "+costToolTitle[t], ta.cell())))
		var ks []key
		for k := range by {
			if k.tool == t {
				ks = append(ks, k)
			}
		}
		sort.Slice(ks, func(i, j int) bool { return by[ks[i]].USD > by[ks[j]].USD })
		unit := "req"
		if t == "codex" {
			unit = "ses"
		}
		for _, k := range ks {
			a := by[k]
			model := k.model
			if len([]rune(model)) > 21 {
				model = string([]rune(model)[:20]) + "…"
			}
			left := fmt.Sprintf("    %-21s %6d %s %9s tok  ", model, a.Count, unit, tok(a.Tokens))
			parts := fmt.Sprintf("%-32s", fmt.Sprintf("in %-5s out %-5s cache %s/%s", tok(in[k]), tok(out[k]), tok(cr[k]), tok(cw[k])))
			line := right(left+parts, a.cell())
			fmt.Println(left + dim(parts) + line[len(left)+len(parts):])
		}
	}
	fmt.Println()
	fmt.Println("  " + strings.Repeat("─", width-2))
	fmt.Println(bold(right("  Total", total.cell())))
	if total.Unpriced > 0 {
		fmt.Println(dim("  + / \"tok\": " + tok(total.Unpriced) + " tokens have no known price — add prices to " + pricesPath()))
	}
	if total.AIU > 0 {
		fmt.Println(dim(fmt.Sprintf("  Copilot reports %.1f GitHub AI units (not converted to dollars).", total.AIU)))
	}
	for _, n := range notes {
		fmt.Println(dim("  " + n))
	}
	return nil
}
