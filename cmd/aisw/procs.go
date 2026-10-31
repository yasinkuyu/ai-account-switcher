package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// runningSessions counts running processes of a tool (other than aisw and
// the login command it may have just run). Open sessions keep their own
// login in memory and can write it back when they refresh it.
func runningSessions(t Tool) int {
	if runtime.GOOS == "windows" {
		return 0
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	bin := t.LoginCmd()[0]
	self := os.Getpid()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == strconv.Itoa(self) {
			continue
		}
		exe := fields[1]
		base := filepath.Base(exe)
		switch {
		case base == bin:
		case t.Name() == "claude" && strings.Contains(exe, "/claude/versions/"):
		case t.Name() == "cursor" && strings.Contains(exe, "cursor-agent"):
		case t.Name() == "antigravity" && strings.Contains(exe, "/.gemini/bin/agy"):
		default:
			continue
		}
		n++
	}
	return n
}
