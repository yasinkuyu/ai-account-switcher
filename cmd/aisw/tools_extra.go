package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ---------------------------------------------------------- Antigravity --
// The Antigravity CLI (agy) keeps its Google login in the OS keyring via
// go-keyring: service "gemini", account "antigravity". The Antigravity
// desktop app reads the same entry, so it follows the switch after a restart.

type antigravityTool struct{}

const (
	agyService = "gemini"
	agyAccount = "antigravity"
)

func (antigravityTool) Name() string { return "antigravity" }

func (antigravityTool) Capture(bool) (Snapshot, error) {
	v, ok, err := osKeyringGet(agyService, agyAccount)
	if err != nil || !ok || v == "" {
		return nil, err
	}
	return Snapshot{"value": v}, nil
}

func agyPayload(v string) map[string]interface{} {
	raw := []byte(v)
	if strings.HasPrefix(v, "go-keyring-base64:") {
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "go-keyring-base64:"))
		if err != nil {
			return nil
		}
		raw = b
	}
	var p map[string]interface{}
	_ = json.Unmarshal(raw, &p)
	return p
}

func (antigravityTool) Identify(s Snapshot) Identity {
	p := agyPayload(s["value"])
	email := str(p, "id_token_email")
	if email == "" {
		if tok, ok := p["token"].(map[string]interface{}); ok {
			email = str(jwtClaims(str(tok, "id_token")), "email")
		}
	}
	return Identity{ID: email, Label: email}
}

func (antigravityTool) Apply(s Snapshot) error {
	return osKeyringPut(agyService, agyAccount, s["value"])
}

func (antigravityTool) Clear() error { return osKeyringDel(agyService, agyAccount) }

func (antigravityTool) Title() string { return "Antigravity" }

// agy has no login subcommand: it asks you to sign in on start. Quit it afterwards.
func (antigravityTool) LoginCmd() []string { return []string{"agy"} }

func (antigravityTool) Notes() []string {
	return []string{"credentials: OS keyring, service \"gemini\", account \"antigravity\"",
		"note: the Antigravity desktop app shares this login; restart it after switching"}
}

// ----------------------------------------------------------------- Kimi --
// Kimi CLI 1.x: $KIMI_SHARE_DIR/credentials/kimi-code.json (default ~/.kimi).
// Older versions used the keyring (service "kimi-code", account
// "oauth/kimi-code"); Kimi migrates that entry to the file on next start.

type kimiTool struct{}

const (
	kimiService = "kimi-code"
	kimiAccount = "oauth/kimi-code"
)

func (kimiTool) Name() string { return "kimi" }

func kimiCredPath() string {
	base := os.Getenv("KIMI_SHARE_DIR")
	if base == "" {
		base = filepath.Join(home(), ".kimi")
	}
	return filepath.Join(base, "credentials", "kimi-code.json")
}

func (kimiTool) Capture(bool) (Snapshot, error) {
	if v, ok, err := readOptional(kimiCredPath()); err != nil {
		return nil, err
	} else if ok {
		return Snapshot{"file": v}, nil
	}
	if runtime.GOOS == "darwin" { // legacy keyring entry
		v, _, ok, err := secretGet(kimiService, kimiAccount)
		if err != nil || !ok || v == "" {
			return nil, err
		}
		return Snapshot{"file": v, "legacy": "1"}, nil
	}
	return nil, nil
}

func (kimiTool) Identify(s Snapshot) Identity {
	var t struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal([]byte(s["file"]), &t)
	id := str(jwtClaims(t.AccessToken), "user_id")
	if id == "" {
		return Identity{}
	}
	return Identity{ID: id, Label: "user " + id[:min(len(id), 12)]}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Apply always writes the file: current Kimi reads it before the keyring.
func (kimiTool) Apply(s Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(kimiCredPath()), 0o700); err != nil {
		return err
	}
	if err := atomicWrite(kimiCredPath(), []byte(s["file"]), 0o600); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" { // a stale legacy entry must not come back
		_ = secretDel(kimiService, kimiAccount)
	}
	return nil
}

func (kimiTool) Clear() error {
	if runtime.GOOS == "darwin" {
		_ = secretDel(kimiService, kimiAccount)
	}
	return removeOptional(kimiCredPath())
}

func (kimiTool) Title() string      { return "Kimi" }
func (kimiTool) LoginCmd() []string { return []string{"kimi", "login"} }

func (kimiTool) Notes() []string {
	return []string{"credentials: " + kimiCredPath()}
}
