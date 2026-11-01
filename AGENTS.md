# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, Cursor, Copilot, …) that
use `aisw` or work on this repository.

## What this project is

`aisw` switches between saved accounts of AI coding CLIs (Claude Code, Codex,
Cursor CLI, GitHub Copilot CLI, OpenCode, Antigravity, Kimi). It is a single Go
binary using only the standard library.

## Using aisw from an agent

- Read state with `aisw status --json`. Do not parse the human output.
- Switch with `aisw <tool> <name>` (for example `aisw codex work`). Only switch
  when the user asks you to.
- Never run a tool's own logout (`/logout`, `codex logout`, `claude auth logout`,
  …) to change accounts. It revokes the session on the server and the saved
  account stops working. Adding an account is `aisw add <tool>`, which is
  interactive — ask the user to run it.
- Claude Code picks up a switch within ~30 seconds, including your own running
  session. For other tools a running session may keep the old account until it
  is restarted.
- Do not use aisw to get around a provider's usage limits by rotating
  accounts automatically. Report usage to the user and let them decide.
- Never print, copy or log credentials. `aisw` output never contains tokens;
  keep it that way.

## Working on the code

| File | Purpose |
|---|---|
| `cmd/aisw/main.go` | commands, profile metadata, switching logic |
| `cmd/aisw/tui.go` | interactive menu, add-account flow, spinner |
| `cmd/aisw/tools.go` | Claude Code, Codex, Cursor adapters and the `Tool` interface |
| `cmd/aisw/tools_more.go` | OpenCode, GitHub Copilot adapters |
| `cmd/aisw/tools_extra.go` | Antigravity, Kimi adapters |
| `cmd/aisw/secrets*.go` | OS credential store per platform (Keychain, Credential Manager, files) |
| `cmd/aisw/term_*.go` | raw terminal mode per platform |
| `cmd/aisw/procs.go` | detects running sessions of a tool |
| `cmd/aisw/aisw_test.go` | tests (temp dirs and a test-only keychain service; never real logins) |

Rules:

- Standard library only. The binary must run with nothing installed.
- Every change must build for all targets: `go vet ./... && GOOS=windows go vet ./... && GOOS=linux go vet ./...`, then `go test ./...` and `./scripts/build.sh`.
- Adding a tool means implementing `Tool` (Capture, Identify, Apply, Clear,
  Title, LoginCmd, Notes). Find where the tool stores its login by reading its
  source, not by guessing, and verify a switch end to end before claiming support.
- Write secrets on macOS through `security -i` on stdin, never as process
  arguments. Read and write the exact Keychain item the tool itself uses
  (service *and* account).
- Never call a tool's logout from code: `Clear` removes the login locally only.
