<div align="center">

<img src="assets/icon.svg" width="128" height="128" alt="AI Account Switcher icon">

# AI Account Switcher

`aisw`

**One command to switch accounts in your AI coding CLIs.**

Claude Code · Codex · Cursor · Copilot · OpenCode · Antigravity · Kimi

Use several accounts of the same AI CLI on one machine — a company Claude Team
seat and a personal Max plan, a work and a personal Codex login — and switch
between them in a second, in every terminal at once.

[![Release](https://img.shields.io/github/v/release/yasinkuyu/ai-account-switcher?style=flat-square)](https://github.com/yasinkuyu/ai-account-switcher/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue?style=flat-square)](LICENSE)
![Platforms](https://img.shields.io/badge/macOS%20·%20Linux%20·%20Windows-lightgrey?style=flat-square)
![Dependencies](https://img.shields.io/badge/dependencies-none-brightgreen?style=flat-square)

</div>

<p align="center">
  <img src="assets/demo.svg" width="760" alt="aisw interactive menu: accounts grouped by tool, the active one marked">
</p>

## Why

- **Zero install hassle** — a single ~2 MB binary. No Node, no Python, no packages.
- **Works everywhere** — macOS, Linux, Windows. Any terminal, any editor.
- **Safe** — saved logins live in your OS credential store (Keychain / Credential Manager), never in plain config.
- **Team + Max on one email** — same email, different organizations? They are two separate accounts.
- **Undo** — every switch keeps the previous login. `aisw undo` brings it back.

## Install

> [!WARNING]
> **Tested on macOS only so far.** The Windows and Linux builds compile and run
> the automated tests in CI, but they have not yet been tried with the real
> tools on those systems. If you use them there, please
> [report how it goes](https://github.com/yasinkuyu/ai-account-switcher/issues).


**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/yasinkuyu/ai-account-switcher/main/install.sh | sh
```

**Windows** (PowerShell)

```powershell
irm https://raw.githubusercontent.com/yasinkuyu/ai-account-switcher/main/install.ps1 | iex
```

Or download a binary from [Releases](https://github.com/yasinkuyu/ai-account-switcher/releases), or `go install github.com/yasinkuyu/ai-account-switcher/cmd/aisw@latest`.

## Quick start

```sh
aisw
```

That's the whole thing. Pick an account and press **Enter** to switch.
Press **a** to add an account: `aisw` keeps your current one, opens the tool's
own login, and saves the new account when you're done.

Accounts you are logged in to are saved automatically — no setup, no naming.

> [!IMPORTANT]
> Add accounts with **`aisw`** (or `aisw add`), not the tool's own logout.
> Claude Code and Codex revoke your session on the server when you log out,
> so a saved account would stop working. `aisw` only removes the login from
> this machine; the session stays valid and safe.

## Commands

For scripts and power users — everything is also in the menu.

| Command | What it does |
|---|---|
| `aisw` | Interactive menu |
| `aisw add [tool]` | Add another account to a tool |
| `aisw <tool> <name>` | Switch one tool, e.g. `aisw claude max` |
| `aisw <name>` | Switch every tool saved under that name |
| `aisw undo` | Go back to the login you had before the last switch |
| `aisw status [--json]` | Which account each tool is using right now (`--json` for scripts and agents) |
| `aisw list` | List saved accounts |
| `aisw save [name] [tool]` | Save the current logins under a name of your choice |
| `aisw rm <name> [tool]` | Delete a saved account |
| `aisw prompt` | One-line status for your shell prompt |
| `aisw doctor` | Show where each tool keeps its login on this machine |

Tools: `claude` `codex` `cursor` `copilot` `opencode` `antigravity` `kimi`

## How it works

Each CLI keeps its login in a known place. `aisw` copies it into a named profile
and copies it back when you switch:

| Tool | macOS | Linux | Windows |
|---|---|---|---|
| Claude Code | Keychain + `~/.claude.json` | `~/.claude/.credentials.json` + `~/.claude.json` | same as Linux |
| Codex | `~/.codex/auth.json` | `~/.codex/auth.json` | `%USERPROFILE%\.codex\auth.json` |
| Cursor CLI | Keychain | `~/.config/cursor/auth.json` | `%APPDATA%\Cursor\auth.json` |
| GitHub Copilot CLI | `~/.copilot/config.json` ¹ | same | same |
| OpenCode | `~/.local/share/opencode/auth.json` | same | same |
| Antigravity CLI (`agy`) | Keychain | Secret Service (`secret-tool`) | Credential Manager |
| Kimi CLI | `~/.kimi/credentials/kimi-code.json` | same | same |

¹ Copilot already stores one login per GitHub user. `aisw` only changes which
one is active and never touches Copilot's tokens — log in once per account with `copilot login`.

Profiles are stored in the macOS Keychain, Windows Credential Manager, or
`~/.aisw/secrets` (mode `0600`) on Linux. Profile names and labels are in
`~/.aisw/profiles.json` — no tokens there.

### Tested

| | macOS | Linux | Windows |
|---|---|---|---|
| Claude Code (Team ↔ Max on one email) | ✅ | ⚠️ | ⚠️ |
| Codex, GitHub Copilot CLI, OpenCode, Cursor CLI, Antigravity | ☑️ | ⚠️ | ⚠️ |
| Kimi | ⚠️ | ⚠️ | ⚠️ |

✅ switching between two real accounts confirmed ·
☑️ saving and restoring a real login confirmed; switching between two accounts
covered by automated tests, not yet tried with two real accounts ·
⚠️ builds and passes the automated tests in CI, not yet confirmed with the real tool

### How accounts are remembered

```text
 you log in ──► aisw saves the account (OS credential store)
 add account ─► save the current one, remove it from this machine only
                (the session on the server stays valid), run the tool's login,
                save the new one — or restore the old one if you cancel
 switch ─────► 1. re-save the account you are leaving (tokens refresh)
                2. keep it as "_previous" for `aisw undo`
                3. write the chosen account back where the tool reads it
```

A saved account keeps working as long as its session is valid on the server.
If you revoke it elsewhere (or it expires after a long time unused), add it again.

## For AI agents

`aisw` is easy for coding agents to use and to understand:

- [`AGENTS.md`](AGENTS.md) — how agents should use `aisw` safely and how to work on this code
- `aisw status --json` — machine-readable state of every tool (never contains tokens)

## FAQ

**How do I use two Claude Code accounts on one computer?**
Log in to the first one as usual, then run `aisw add claude` and log in to the
second. Switch with `aisw` or `aisw claude <name>`.

**How do I switch accounts in Codex CLI without logging in again?**
Same idea: `aisw add codex` once, then `aisw codex <name>` to switch. Your
`~/.codex/auth.json` is swapped for you.


**Do I have to close my open sessions?**
New sessions use the new account right away. Sessions that are already open keep
the old one — and when they refresh their login (every few hours) they can write
it back, quietly switching you back. Restart open sessions after switching;
`aisw` tells you how many are open and marks a login that was written back
with `⚠ login mismatch`.

**I have a Team seat and a Max plan on the same email. Will they clash?**
No. Claude accounts are identified by user *and* organization, so they show up as two accounts.

**Should I use `/logout`, `codex logout`, … to change accounts?**
No — press **a** in `aisw` (or run `aisw add`). A tool's logout revokes that
session on the server, so the account you saved would stop working.

**Does it switch the Cursor editor?**
No — only the Cursor CLI (`cursor-agent`). The editor app has its own login.
The Antigravity desktop app *does* share its login with `agy`, so it follows
the switch after a restart.

**An API key environment variable is set.**
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `CURSOR_API_KEY` or `GH_TOKEN` / `GITHUB_TOKEN` override logins in those tools.
`aisw doctor` warns you about them.

**Is this against the tools' terms?**
`aisw` only moves your own login files between your own accounts on your own machine.
It does not share, proxy or pool accounts. Use each account according to its plan's terms.

## Build from source

```sh
git clone https://github.com/yasinkuyu/ai-account-switcher && cd ai-account-switcher
go build -o aisw ./cmd/aisw       # current platform
go test ./...                     # tests (never touch your real logins)
./scripts/build.sh          # all platforms → dist/
```

## License

[MIT](LICENSE)
