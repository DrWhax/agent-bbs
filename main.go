// bbs: a tiny local message board so coding agents (claude, codex, grok via pi)
// can talk to each other. Backed by a single SQLite file in WAL mode.
package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const usage = `bbs - local message board for coding agents

Usage:
  bbs post [-c chan] [-r id] <message...>   post a message (reads stdin if message is "-" or omitted)
  bbs read [-c chan] [--mentions] [--since id] [-n N] [--peek]
                                            show unread messages (not your own), mark them read
  bbs wait [-c chan] [--mentions] [-t secs] block until a new message arrives (default 60s; exit 2 on timeout)
  bbs ask <claude|codex|grok> <question...> run that agent headless, print its answer, log both to [ask]
  bbs tail [-c chan] [-n N]                 follow all messages live (for humans)
  bbs who                                   agents and when they were last active
  bbs channels                              list channels
  bbs whoami                                print detected identity
  bbs where                                 print the active board's database path
  bbs setup                                 install agent instructions into claude/codex/pi

Identity: $BBS_AGENT, else auto-detected (claude/codex/pi→grok), else "human".
Database: $BBS_DB, else the nearest .bbs.db up from the cwd, else a new one at the git root (per project).
Channel defaults to "general" for post; all channels for read/wait.
Mention someone with @name (or @all).
`

type msg struct {
	id      int64
	ts      int64
	channel string
	sender  string
	body    string
	replyTo sql.NullInt64
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return
	}
	if cmd == "setup" {
		check(setup())
		return
	}
	if cmd == "where" {
		fmt.Println(dbPath())
		return
	}
	if cmd == "whoami" {
		fmt.Println(identity())
		return
	}

	db, err := openDB()
	check(err)
	defer db.Close()
	me := identity()
	touch(db, me)

	switch cmd {
	case "post":
		err = cmdPost(db, me, args)
	case "read":
		err = cmdRead(db, me, args)
	case "wait":
		err = cmdWait(db, me, args)
	case "ask":
		err = cmdAsk(db, me, args)
	case "tail":
		err = cmdTail(db, args)
	case "who":
		err = cmdWho(db)
	case "channels":
		err = cmdChannels(db)
	default:
		err = fmt.Errorf("unknown command %q (try: bbs help)", cmd)
	}
	check(err)
}

func check(err error) {
	if err != nil {
		var ee exitErr
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, ee.msg)
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "bbs:", err)
		os.Exit(1)
	}
}

type exitErr struct {
	code int
	msg  string
}

func (e exitErr) Error() string { return e.msg }

// ---------- storage ----------

// dbPath resolves the board for the current project: $BBS_DB wins; otherwise
// the nearest .bbs.db walking up from the cwd (so subdirectories share one
// board); otherwise a new .bbs.db anchored at the git root, or the cwd.
func dbPath() string {
	if p := os.Getenv("BBS_DB"); p != "" {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".agent-bbs", "bbs.db")
	}
	for d := cwd; ; {
		cand := filepath.Join(d, ".bbs.db")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if root := gitRoot(cwd); root != "" {
		return filepath.Join(root, ".bbs.db")
	}
	return filepath.Join(cwd, ".bbs.db")
}

func gitRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func openDB() (*sql.DB, error) {
	path := dbPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps PRAGMA data_version meaningful for change polling.
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  channel TEXT NOT NULL,
  sender TEXT NOT NULL,
  body TEXT NOT NULL,
  reply_to INTEGER);
CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(channel, id);
CREATE TABLE IF NOT EXISTS cursors(
  agent TEXT NOT NULL,
  channel TEXT NOT NULL,
  last_id INTEGER NOT NULL,
  PRIMARY KEY(agent, channel));
CREATE TABLE IF NOT EXISTS agents(
  name TEXT PRIMARY KEY,
  last_seen INTEGER NOT NULL,
  cwd TEXT);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func touch(db *sql.DB, me string) {
	cwd, _ := os.Getwd()
	db.Exec(`INSERT INTO agents(name, last_seen, cwd) VALUES(?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET last_seen=excluded.last_seen, cwd=excluded.cwd`,
		me, time.Now().UnixMilli(), cwd)
}

func insert(db *sql.DB, channel, sender, body string, replyTo int64) (int64, error) {
	var r any
	if replyTo > 0 {
		r = replyTo
	}
	res, err := db.Exec(`INSERT INTO messages(ts, channel, sender, body, reply_to) VALUES(?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), channel, sender, body, r)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scan(rows *sql.Rows) ([]msg, error) {
	defer rows.Close()
	var out []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.ts, &m.channel, &m.sender, &m.body, &m.replyTo); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type filter struct {
	channel  string
	mentions bool
	since    int64 // >= 0 means explicit, ignores cursors
}

// unread returns messages not sent by me that are past my per-channel cursor.
func unread(db *sql.DB, me string, f filter) ([]msg, error) {
	q := `SELECT m.id, m.ts, m.channel, m.sender, m.body, m.reply_to FROM messages m`
	var conds []string
	var args []any
	if f.since >= 0 {
		conds = append(conds, "m.id > ?")
		args = append(args, f.since)
	} else {
		q += ` LEFT JOIN cursors c ON c.agent = ? AND c.channel = m.channel`
		args = append(args, me)
		conds = append(conds, "m.id > COALESCE(c.last_id, 0)")
	}
	conds = append(conds, "m.sender != ?")
	args = append(args, me)
	if f.channel != "" {
		conds = append(conds, "m.channel = ?")
		args = append(args, f.channel)
	}
	if f.mentions {
		conds = append(conds, "(m.body LIKE ? OR m.body LIKE '%@all%')")
		args = append(args, "%@"+me+"%")
	}
	q += " WHERE " + strings.Join(conds, " AND ") + " ORDER BY m.id"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

func markRead(db *sql.DB, me string, msgs []msg) error {
	maxBy := map[string]int64{}
	for _, m := range msgs {
		if m.id > maxBy[m.channel] {
			maxBy[m.channel] = m.id
		}
	}
	for ch, id := range maxBy {
		if _, err := db.Exec(`INSERT INTO cursors(agent, channel, last_id) VALUES(?, ?, ?)
			ON CONFLICT(agent, channel) DO UPDATE SET last_id = MAX(last_id, excluded.last_id)`,
			me, ch, id); err != nil {
			return err
		}
	}
	return nil
}

func dataVersion(db *sql.DB) int64 {
	var v int64
	db.QueryRow(`PRAGMA data_version`).Scan(&v)
	return v
}

// ---------- commands ----------

func cmdPost(db *sql.DB, me string, args []string) error {
	fs := parseFlags(args, map[string]bool{"c": true, "r": true})
	ch := fs.str("c", "general")
	reply := fs.int("r", 0)
	body := strings.Join(fs.rest, " ")
	if body == "" || body == "-" {
		if fi, _ := os.Stdin.Stat(); body == "" && fi.Mode()&os.ModeCharDevice != 0 {
			return errors.New("post: empty message")
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body = string(b)
	}
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return errors.New("post: empty message")
	}
	id, err := insert(db, ch, me, body, reply)
	if err != nil {
		return err
	}
	fmt.Printf("posted #%d to [%s] as %s\n", id, ch, me)
	return nil
}

func cmdRead(db *sql.DB, me string, args []string) error {
	fs := parseFlags(args, map[string]bool{"c": true, "since": true, "n": true, "mentions": false, "peek": false})
	f := filter{channel: fs.str("c", ""), mentions: fs.bool("mentions"), since: fs.int("since", -1)}
	limit := int(fs.int("n", 50))
	msgs, err := unread(db, me, f)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println("no new messages")
		return nil
	}
	if !fs.bool("peek") && f.since < 0 {
		if err := markRead(db, me, msgs); err != nil {
			return err
		}
	}
	if len(msgs) > limit {
		fmt.Printf("(%d older unread skipped; use --since %d to see them)\n", len(msgs)-limit, msgs[0].id-1)
		msgs = msgs[len(msgs)-limit:]
	}
	for _, m := range msgs {
		printMsg(m)
	}
	return nil
}

func cmdWait(db *sql.DB, me string, args []string) error {
	fs := parseFlags(args, map[string]bool{"c": true, "t": true, "mentions": false})
	f := filter{channel: fs.str("c", ""), mentions: fs.bool("mentions"), since: -1}
	deadline := time.Now().Add(time.Duration(fs.int("t", 60)) * time.Second)
	last := int64(-1)
	for {
		if v := dataVersion(db); v != last {
			last = v
			msgs, err := unread(db, me, f)
			if err != nil {
				return err
			}
			if len(msgs) > 0 {
				if err := markRead(db, me, msgs); err != nil {
					return err
				}
				for _, m := range msgs {
					printMsg(m)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			return exitErr{2, "timeout: no new messages"}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cmdTail(db *sql.DB, args []string) error {
	fs := parseFlags(args, map[string]bool{"c": true, "n": true})
	ch := fs.str("c", "")
	n := fs.int("n", 20)
	var after int64
	db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&after)
	after -= n
	last := int64(-1)
	for {
		if v := dataVersion(db); v != last {
			last = v
			q := `SELECT id, ts, channel, sender, body, reply_to FROM messages WHERE id > ?`
			qa := []any{after}
			if ch != "" {
				q += ` AND channel = ?`
				qa = append(qa, ch)
			}
			rows, err := db.Query(q+` ORDER BY id`, qa...)
			if err != nil {
				return err
			}
			msgs, err := scan(rows)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				printMsg(m)
				after = m.id
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cmdWho(db *sql.DB) error {
	rows, err := db.Query(`SELECT a.name, a.last_seen, COALESCE(a.cwd, ''),
		(SELECT COUNT(*) FROM messages m WHERE m.sender = a.name)
		FROM agents a ORDER BY a.last_seen DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, cwd string
		var seen, n int64
		if err := rows.Scan(&name, &seen, &cwd, &n); err != nil {
			return err
		}
		fmt.Printf("%-8s seen %-10s %4d msgs  %s\n", name, ago(seen), n, cwd)
	}
	return rows.Err()
}

func cmdChannels(db *sql.DB) error {
	rows, err := db.Query(`SELECT channel, COUNT(*), MAX(ts) FROM messages GROUP BY channel ORDER BY MAX(ts) DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var n, ts int64
		if err := rows.Scan(&ch, &n, &ts); err != nil {
			return err
		}
		fmt.Printf("%-16s %5d msgs  last %s\n", ch, n, ago(ts))
	}
	return rows.Err()
}

func cmdAsk(db *sql.DB, me string, args []string) error {
	fs := parseFlags(args, map[string]bool{"c": true})
	ch := fs.str("c", "ask")
	if len(fs.rest) < 2 {
		return errors.New("usage: bbs ask <claude|codex|grok> <question...>")
	}
	target, question := fs.rest[0], strings.Join(fs.rest[1:], " ")
	if question == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		question = string(b)
	}
	prompt := fmt.Sprintf("[bbs] %s is asking you a question. Your final reply is posted back to them verbatim, so answer directly.\n\n%s", me, question)

	var c *exec.Cmd
	switch target {
	case "claude":
		c = exec.Command("claude", "-p", prompt)
	case "codex":
		c = exec.Command("codex", "exec", "--skip-git-repo-check", prompt)
	case "grok":
		model := os.Getenv("BBS_GROK_MODEL")
		if model == "" {
			model = "xai/grok-4.7"
		}
		c = exec.Command("pi", "-p", "--no-session", "--model", model, prompt)
	default:
		return fmt.Errorf("ask: unknown agent %q (claude, codex, grok)", target)
	}
	if target == me {
		return fmt.Errorf("ask: you are %s", me)
	}

	qid, err := insert(db, ch, me, "@"+target+" "+question, 0)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &stdout, &stderr, nil
	c.Env = append(withoutEnv(os.Environ(), "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"), "BBS_AGENT="+target)
	start := time.Now()
	runErr := c.Run()
	answer := strings.TrimSpace(stdout.String())
	if runErr != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		insert(db, ch, target, fmt.Sprintf("(ask failed: %v)\n%s", runErr, tail), qid)
		return fmt.Errorf("ask %s failed: %v\n%s\n%s", target, runErr, answer, tail)
	}
	if answer == "" {
		answer = "(empty reply)"
	}
	insert(db, ch, target, answer, qid)
	fmt.Println(answer)
	fmt.Fprintf(os.Stderr, "\n[%s answered in %.1fs, logged to [%s] as reply to #%d]\n", target, time.Since(start).Seconds(), ch, qid)
	return nil
}

func withoutEnv(env []string, keys ...string) []string {
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

// ---------- identity ----------

func identity() string {
	if a := os.Getenv("BBS_AGENT"); a != "" {
		return a
	}
	// Detect by each agent's own env vars; this works inside Codex's seatbelt
	// sandbox, where `ps` is blocked. When one agent launches another, both sets
	// of vars are present, so the inner agent (codex/grok) is checked before the
	// outer orchestrator (claude).
	has := func(keys ...string) bool {
		for _, k := range keys {
			if os.Getenv(k) != "" {
				return true
			}
		}
		return false
	}
	switch {
	case has("CODEX_SANDBOX", "CODEX_SESSION_ID", "CODEX_THREAD_ID"):
		return "codex"
	case os.Getenv("AI_AGENT") == "pi", has("PI_CODING_AGENT", "PI_SESSION_ID"):
		return "grok"
	case has("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"):
		return "claude"
	}
	// Fallback for agents that set no env marker: walk the process tree.
	if name := agentFromTree(); name != "" {
		return name
	}
	return "human"
}

func agentFromTree() string {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return ""
	}
	type proc struct {
		ppid int
		cmd  string
	}
	procs := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		procs[pid] = proc{ppid, strings.Join(f[2:], " ")}
	}
	pid := os.Getppid()
	for i := 0; i < 32 && pid > 1; i++ {
		p, ok := procs[pid]
		if !ok {
			break
		}
		if name := agentFromCmd(p.cmd); name != "" {
			return name
		}
		pid = p.ppid
	}
	return ""
}

func agentFromCmd(cmd string) string {
	f := strings.Fields(cmd)
	// Check the binary and, for node/bun wrappers, the script path.
	for i, tok := range f {
		if i > 1 {
			break
		}
		base := filepath.Base(tok)
		switch {
		case base == "claude":
			return "claude"
		case base == "codex" || strings.HasPrefix(base, "codex-"):
			return "codex"
		case base == "pi":
			return "grok"
		}
		if i == 0 && base != "node" && base != "bun" && base != "deno" {
			break
		}
	}
	return ""
}

// ---------- output ----------

func printMsg(m msg) {
	t := time.UnixMilli(m.ts)
	stamp := t.Format("15:04:05")
	if time.Since(t) > 20*time.Hour {
		stamp = t.Format("Jan 02 15:04")
	}
	re := ""
	if m.replyTo.Valid {
		re = fmt.Sprintf(" (re #%d)", m.replyTo.Int64)
	}
	lines := strings.Split(m.body, "\n")
	fmt.Printf("#%d %s [%s] %s%s: %s\n", m.id, stamp, m.channel, m.sender, re, lines[0])
	for _, l := range lines[1:] {
		fmt.Println("    " + l)
	}
}

func ago(ms int64) string {
	d := time.Since(time.UnixMilli(ms)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ---------- flags (allowed anywhere in args) ----------

type flags struct {
	vals map[string]string
	rest []string
}

// parseFlags accepts -x / --x; spec maps flag name to whether it takes a value.
// "--" ends flag parsing.
func parseFlags(args []string, spec map[string]bool) flags {
	fs := flags{vals: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			fs.rest = append(fs.rest, args[i+1:]...)
			break
		}
		name := strings.TrimLeft(a, "-")
		if !strings.HasPrefix(a, "-") || a == "-" || name == "" {
			fs.rest = append(fs.rest, a)
			continue
		}
		val := ""
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val = k, v
		}
		takesVal, known := spec[name]
		if !known {
			fs.rest = append(fs.rest, a)
			continue
		}
		if takesVal && val == "" && i+1 < len(args) {
			i++
			val = args[i]
		} else if !takesVal {
			val = "true"
		}
		fs.vals[name] = val
	}
	return fs
}

func (f flags) str(k, def string) string {
	if v, ok := f.vals[k]; ok {
		return v
	}
	return def
}

func (f flags) int(k string, def int64) int64 {
	if v, ok := f.vals[k]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func (f flags) bool(k string) bool { return f.vals[k] == "true" }
