package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	markBegin = "<!-- bbs:begin -->"
	markEnd   = "<!-- bbs:end -->"
)

// Instructions appended to each agent's global context file.
const snippet = markBegin + `
## Agent BBS (talking to other agents)

You share this machine with other coding agents: **claude** (Claude Code), **codex** (Codex CLI) and **grok** (pi).
You can all talk through the ` + "`bbs`" + ` CLI. Your identity is detected automatically (check with ` + "`bbs whoami`" + `).

Each project has its own board: a ` + "`.bbs.db`" + ` file at the project root (run ` + "`bbs where`" + ` to see which one is active). Only agents working in the same project see each other's messages.

- ` + "`bbs read`" + ` — unread messages from others. Run it when you start a task and before you finish one.
- ` + "`bbs post \"msg\"`" + ` — post to [general]. Use ` + "`-c <channel>`" + ` for a topic channel, ` + "`-r <id>`" + ` to reply, ` + "`@name`" + ` to address someone.
- ` + "`bbs wait --mentions -t 60`" + ` — block until someone messages you (exit 2 on timeout). Set your shell tool timeout above -t. It marks what it shows as read, so run it yourself — if a *separate* background watcher loops on it, add ` + "`--peek`" + ` there so it wakes you without consuming the message before your own ` + "`bbs read`" + ` sees it.
- ` + "`bbs ask <claude|codex|grok> \"question\"`" + ` — get a one-off answer from another agent (runs it headless; can take minutes, so use a long tool timeout).
- ` + "`bbs who`" + ` / ` + "`bbs channels`" + ` — who is active and where.

Guidelines: keep messages short and concrete (file paths, commands, results). If someone @mentions you with a request, reply on the bbs.
Before editing files another agent says it is working on, coordinate first. Don't loop on ` + "`bbs wait`" + ` forever. Give up after a few timeouts and tell the user.
` + markEnd + "\n"

func setup() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, p := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".codex", "AGENTS.md"),
		filepath.Join(home, ".pi", "agent", "AGENTS.md"),
	} {
		if err := upsertBlock(p); err != nil {
			return err
		}
	}
	// The board now lives inside each project (.bbs.db at the project root), so
	// Codex's workspace-write sandbox can reach it without a global exception.
	// Remove the one an older `bbs setup` may have added.
	return codexUnwritableRoot(filepath.Join(home, ".codex", "config.toml"))
}

// upsertBlock writes the snippet into path, replacing any previous bbs block.
func upsertBlock(path string) error {
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(old)
	if i := strings.Index(s, markBegin); i >= 0 {
		if j := strings.Index(s[i:], markEnd); j >= 0 {
			end := i + j + len(markEnd)
			if end < len(s) && s[end] == '\n' {
				end++
			}
			if s[i:end] == snippet {
				fmt.Println("up to date:", path)
				return nil
			}
			s = s[:i] + snippet + s[end:]
			fmt.Println("updated:   ", path)
			return writeWithBackup(path, old, s)
		}
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	s += snippet
	fmt.Println("added to:  ", path)
	return writeWithBackup(path, old, s)
}

func writeWithBackup(path string, old []byte, s string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if old != nil {
		if err := os.WriteFile(path+".bbs.bak", old, 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

// codexUnwritableRoot removes the sandbox_workspace_write block that an older
// `bbs setup` added for the global ~/.agent-bbs board. It is now unneeded.
func codexUnwritableRoot(path string) error {
	old, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	const marker = "# added by bbs setup: let sandboxed commands write the agent bbs database\n"
	s := string(old)
	i := strings.Index(s, marker)
	if i < 0 {
		return nil
	}
	// Drop the marker and the two lines after it ([sandbox_workspace_write] and
	// writable_roots = [...]), plus one blank line before the marker if present.
	end := i + len(marker)
	for n := 0; n < 2 && end < len(s); n++ {
		if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
			end += nl + 1
		} else {
			end = len(s)
		}
	}
	start := i
	if start > 0 && s[start-1] == '\n' {
		start--
	}
	s = s[:start] + s[end:]
	fmt.Println("removed:   ", path, "(stale sandbox writable_roots)")
	return writeWithBackup(path, old, s)
}
