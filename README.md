# trapeze

A prototype shell that operates very differently: the interaction model
of an **agent harness**, applied to an interactive shell.

trapeze is built on the [Charm](https://charm.land) Bubble Tea TUI from
[Crush](https://github.com/charmbracelet/crush) (this repo is a Crush
fork being converted into other things; the LLM machinery is bypassed in
shell mode). Where Crush sends your prompt to a model that emits tool
uses, trapeze treats *you* as the model:

- A **single command entry bar** sits at the bottom. It's treated as one
  line; hitting return executes the command.
- Each executed command renders **above, like a tool use**: a header with
  a status icon (`●` running, `✓` ok, `×` non-zero exit) and the
  command, with the output as the collapsible tool body.
- **Background jobs appear on the right**, where the agent harness shows
  its skills list. Ending a command with a single trailing `&` runs it
  as a harness-managed job: a `Job (Start)` block lands immediately, the
  job ticks in the sidebar while it runs, and a `Job (Output)` block
  surfaces its output in the history when it finishes.
- **Sessions are shell tabs**: each session is named after its first
  command, keeps its own working directory, and the session switcher
  flips between them. The first `esc esc` interrupts the running
  foreground command (it kills the whole process group), just like
  canceling an agent.

The shell syntax is **fish** (for now). Each submitted command runs in a
fresh `fish -c` process:

- `cd` persists across commands per session (an epilogue records `$PWD`
  and restores it for the next command, preserving the command's exit
  status; syntax errors skip the epilogue and keep the previous cwd).
- fish **universal variables** (`set -Ux`) persist across commands — and
  across trapeze restarts — for free, via fish's own universal variable
  store. Plain `set -x` exports do *not* survive to the next command;
  that's a known gap of the process-per-command model.
- Pipes, `&&`/`||`, command substitution, etc. all work; it's just fish.

## Running it

```bash
go run . --shell           # or: crush --shell
```

Or persist it in `crush.json` so plain `go run .` starts the shell:

```json
{ "shell": {} }
```

`shell.command` overrides the fish binary path. Shell mode needs no
provider/API-key onboarding and is mutually exclusive with the XMPP
chat mode that also lives in this playground.

> Toolchain note: see AGENTS.md — the host `go` lane is currently pinned
> behind nixpkgs (trapeze#3), so build with `GOTOOLCHAIN=go1.26.4 go
> build .` (after temporarily bumping the `go` directive) or use the
> `-nix` just recipes.

## How it works

Shell mode rides the same seam as the XMPP slice: `AppWorkspace.AgentRun`
(`internal/workspace/shell.go`). Instead of running the LLM coordinator,
each submitted line synthesizes the message sequence an agent turn would
produce — an assistant message carrying a finished `bash` tool call,
then a tool message carrying the result — so the existing chat UI
renders commands with the same tool renderers the agent uses
(`internal/ui/chat/bash.go`).

`internal/fish` is the execution engine: per-session cwd tracking,
process-group cancellation, capped output buffers, and a background job
table that publishes lifecycle events over the in-process pubsub. The
TUI folds those events into the sidebar jobs list
(`internal/ui/model/jobs.go`).

## Known gaps / next passes

- **Tab completion / autocomplete** for the command bar is explicitly
  deferred to a later pass (fish's `complete -C` can back it).
- **No PTY yet**: commands run with pipes and an empty stdin, so
  interactive/full-screen programs (vim, less, sudo prompts) and
  programs that only colorize on a TTY don't behave like a real
  terminal. The plan is to run commands on a PTY with real terminal
  emulation behind each block — see the sibling
  [posh](../posh) repo (`posh-term`) for the kind of VT emulation this
  should grow into.
- **No live output streaming**: a foreground command's output lands when
  it finishes (matching how the agent harness shows tool results).
  Long-running commands show a spinner until then; backgrounding with
  `&` is the workaround, as is the eventual streaming/PTY pass.
- Exported (non-universal) environment variables don't persist across
  commands.
- Job control is start/observe only: no `job kill`/foregrounding UI yet
  (the runner already supports kill; it needs a surface).
- The trailing-`&` detection is a heuristic; a command *ending* in a
  literal `&` inside a string would be misread.

## Repo layout

This is a playground. The Crush codebase it forked from is largely
intact underneath (and `AGENTS.md` still describes it); the XMPP chat
client vertical slice from the previous experiment also remains. Shell
mode's own surface area:

```
internal/fish/                fish execution engine + job table
internal/workspace/shell.go   AgentRun → tool-use message synthesis
internal/ui/model/jobs.go     sidebar jobs section, shell-mode helpers
internal/config/config.go     `shell` config block
```
