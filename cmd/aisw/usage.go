package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Usage limits shown next to accounts. Read only: aisw never refreshes a
// login to get them (that would change the saved credential).
//
//   Claude Code — the usage endpoint Claude Code's own /usage screen calls,
//                 with that account's saved access token (skipped if expired).
//   Codex       — the latest rate-limit snapshot Codex wrote into its own
//                 session logs for that account (no network).

type usageInfo struct {
	FiveHour  float64 // percent used, -1 = unknown
	Week      float64
	FiveReset time.Time
	AsOf      time.Time // for data taken from logs
}

func (u usageInfo) known() bool { return u.FiveHour >= 0 || u.Week >= 0 }

func pct(v float64) string {
	s := fmt.Sprintf("%.0f%%", v)
	switch {
	case v >= 90:
		return paint("31", s)
	case v >= 70:
		return paint("33", s)
	}
	return s
}

// short renders e.g. "5h 31% ↻04:19 · wk 12%".
func (u usageInfo) short() string {
	var parts []string
	if u.FiveHour >= 0 {
		p := "5h " + pct(u.FiveHour)
		if u.FiveHour > 0 && !u.FiveReset.IsZero() && u.FiveReset.After(time.Now()) {
			p += " ↻" + u.FiveReset.Local().Format("15:04")
		}
		parts = append(parts, p)
	}
	if u.Week >= 0 {
		parts = append(parts, "wk "+pct(u.Week))
	}
	return strings.Join(parts, " · ")
}

func usageFor(t Tool, s Snapshot) usageInfo {
	switch t.Name() {
	case "claude":
		return claudeUsage(s)
	case "codex":
		return codexUsage(s)
	}
	return usageInfo{FiveHour: -1, Week: -1}
}

// ------------------------------------------------------------- Claude --

var usageClient = &http.Client{Timeout: 6 * time.Second}

func claudeUsage(s Snapshot) usageInfo {
	none := usageInfo{FiveHour: -1, Week: -1}
	cred := s["keychain"]
	if cred == "" {
		cred = s["file"]
	}
	var c struct {
		O struct {
			Token   string `json:"accessToken"`
			Expires int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal([]byte(cred), &c) != nil || c.O.Token == "" {
		return none
	}
	if c.O.Expires > 0 && time.UnixMilli(c.O.Expires).Before(time.Now()) {
		return none // expired: refreshing it would rotate the saved login
	}
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	req.Header.Set("Authorization", "Bearer "+c.O.Token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := usageClient.Do(req)
	if err != nil {
		return none
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return none
	}
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var d struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return none
	}
	u := none
	if d.FiveHour != nil && d.FiveHour.Utilization != nil {
		u.FiveHour = *d.FiveHour.Utilization
		u.FiveReset, _ = time.Parse(time.RFC3339Nano, d.FiveHour.ResetsAt)
	}
	if d.SevenDay != nil && d.SevenDay.Utilization != nil {
		u.Week = *d.SevenDay.Utilization
	}
	return u
}

// -------------------------------------------------------------- Codex --

func codexUsage(s Snapshot) usageInfo {
	none := usageInfo{FiveHour: -1, Week: -1}
	var a struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal([]byte(s["auth"]), &a) != nil || a.Tokens.AccountID == "" {
		return none
	}
	files := recentFiles(filepath.Join(codexHome(), "sessions"), ".jsonl", 300)
	for _, f := range files {
		if u, ok := codexLogUsage(f, a.Tokens.AccountID); ok {
			return u
		}
	}
	return none
}

func recentFiles(root, ext string, limit int) []string {
	type fm struct {
		path string
		mod  time.Time
	}
	var all []fm
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ext) {
			all = append(all, fm{p, info.ModTime()})
		}
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]string, len(all))
	for i, f := range all {
		out[i] = f.path
	}
	return out
}

func codexLogUsage(path, accountID string) (usageInfo, bool) {
	f, err := os.Open(path)
	if err != nil {
		return usageInfo{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	if !sc.Scan() {
		return usageInfo{}, false
	}
	var meta struct {
		Payload struct {
			Account string `json:"creator_account_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &meta) != nil || meta.Payload.Account != accountID {
		return usageInfo{}, false
	}
	type window struct {
		Used     float64 `json:"used_percent"`
		ResetsAt int64   `json:"resets_at"`
	}
	var last struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type   string `json:"type"`
			Limits *struct {
				Primary   *window `json:"primary"`
				Secondary *window `json:"secondary"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"token_count"`) {
			continue
		}
		var ev = last
		if json.Unmarshal(line, &ev) == nil && ev.Payload.Limits != nil {
			last, found = ev, true
		}
	}
	if !found {
		return usageInfo{}, false
	}
	u := usageInfo{FiveHour: -1, Week: -1}
	u.AsOf, _ = time.Parse(time.RFC3339Nano, last.Timestamp)
	now := time.Now()
	if w := last.Payload.Limits.Primary; w != nil {
		u.FiveHour, u.FiveReset = w.Used, time.Unix(w.ResetsAt, 0)
		if w.ResetsAt > 0 && u.FiveReset.Before(now) {
			u.FiveHour = 0 // that window has reset since
		}
	}
	if w := last.Payload.Limits.Secondary; w != nil {
		u.Week = w.Used
		if w.ResetsAt > 0 && time.Unix(w.ResetsAt, 0).Before(now) {
			u.Week = 0
		}
	}
	return u, true
}

// ------------------------------------------------------------ helpers --

// fillUsage looks up usage for many accounts in parallel.
func fillUsage(jobs []func()) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j func()) { defer wg.Done(); j() }(j)
	}
	wg.Wait()
}
