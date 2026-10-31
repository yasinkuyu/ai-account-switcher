package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// macOS Keychain through /usr/bin/security, which ships with every macOS.
// Secrets are passed on stdin (`security -i`), never as process arguments.

func secretGet(service, account string) (string, string, bool, error) {
	args := []string{"find-generic-password", "-s", service, "-g"}
	if account != "" {
		args = append(args, "-a", account)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 { // errSecItemNotFound
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("keychain read %s: %s", service, strings.TrimSpace(stderr.String()))
	}
	acct := ""
	if m := acctPattern.FindStringSubmatch(stdout.String()); m != nil {
		acct = m[1]
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if !strings.HasPrefix(line, "password:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "password:"))
		switch {
		case strings.HasPrefix(rest, "0x"): // non-printable data is shown as hex
			h := strings.Fields(rest)[0][2:]
			b, err := hex.DecodeString(h)
			if err != nil {
				return "", "", false, err
			}
			return string(b), acct, true, nil
		case len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"':
			return rest[1 : len(rest)-1], acct, true, nil
		default:
			return "", acct, true, nil
		}
	}
	return "", acct, true, nil
}

func kcQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func secretPut(service, account, value string) error {
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n",
		kcQuote(service), kcQuote(account), hex.EncodeToString([]byte(value)))
	var cmd *exec.Cmd
	if len(line) < 4000 { // `security -i` reads lines of at most ~4 KB
		cmd = exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(line)
	} else {
		cmd = exec.Command("/usr/bin/security", "add-generic-password", "-U",
			"-s", service, "-a", account, "-X", hex.EncodeToString([]byte(value)))
	}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "returned") {
		return fmt.Errorf("keychain write %s/%s failed: %s", service, account, strings.TrimSpace(string(out)))
	}
	return nil
}

func secretDel(service, account string) error {
	_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", service, "-a", account).Run()
	return nil
}

func secretStoreName() string { return "macOS Keychain" }

// osKeyring* access entries other tools keep in the OS keyring. On this
// platform that is the same store aisw uses for its own profiles.
func osKeyringGet(service, account string) (string, bool, error) {
	v, _, ok, err := secretGet(service, account)
	return v, ok, err
}

func osKeyringPut(service, account, value string) error {
	return secretPutVerified(service, account, value)
}

func osKeyringDel(service, account string) error { return secretDel(service, account) }
