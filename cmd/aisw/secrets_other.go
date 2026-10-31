//go:build !darwin && !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux / BSD: no keyring is guaranteed to exist, so profile secrets live in
// ~/.aisw/secrets with 0600 permissions — the same protection the CLIs
// themselves use for their own credential files on these systems.

func secretPath(service, account string) string {
	sum := sha256.Sum256([]byte(service + "\x00" + account))
	return filepath.Join(stateDir(), "secrets", hex.EncodeToString(sum[:16]))
}

func secretGet(service, account string) (string, string, bool, error) {
	b, err := os.ReadFile(secretPath(service, account))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return string(b), account, true, nil
}

func secretPut(service, account, value string) error {
	dir := filepath.Join(stateDir(), "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicWrite(secretPath(service, account), []byte(value), 0o600)
}

func secretDel(service, account string) error {
	err := os.Remove(secretPath(service, account))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func secretStoreName() string { return "~/.aisw/secrets (0600 files)" }

// osKeyring* access entries other tools keep in the Linux Secret Service
// (go-keyring / libsecret attributes "service" and "username"). Uses the
// standard `secret-tool` command; without it these entries are skipped.
func osKeyringGet(service, account string) (string, bool, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return "", false, nil
	}
	out, err := exec.Command("secret-tool", "lookup", "service", service, "username", account).Output()
	if err != nil || len(out) == 0 {
		return "", false, nil
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

func osKeyringPut(service, account, value string) error {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return errors.New("secret-tool is required to switch this tool on Linux")
	}
	cmd := exec.Command("secret-tool", "store", "--label="+service, "service", service, "username", account)
	cmd.Stdin = strings.NewReader(value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("secret-tool store: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func osKeyringDel(service, account string) error {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil
	}
	_ = exec.Command("secret-tool", "clear", "service", service, "username", account).Run()
	return nil
}
