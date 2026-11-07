// Package audit writes the Audit log: one JSONL file per sprut serve, of
// the Agent's session, every Namespaced tool call that reaches an Upstream
// and every Script, with timings, outcomes and sizes, but never arguments
// or results.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/marvell/sprut/internal/config"
)

// Outcome is how a call or a Script ended.
type Outcome string

// The outcomes of a call or a Script.
const (
	OK        Outcome = "ok"
	Error     Outcome = "error"     // an isError result, or a failure
	Cancelled Outcome = "cancelled" // the request's context ended first
)

// Session is the Agent's session, the first event of an Audit log.
type Session struct {
	Sprut        string // sprut's version
	Agent        string // the Agent's clientInfo name, as it sent it
	AgentVersion string
	Protocol     string
	CodeMode     bool
}

// Call is a Namespaced tool call that reached an Upstream.
type Call struct {
	Tool     string // the Namespaced tool
	Upstream string
	Script   int // the number of the Script that made it, or 0 for a direct call
	Took     time.Duration
	Outcome  Outcome
	Err      string // with the Error outcome, why
	// ArgsBytes is the size of the arguments' JSON; ResultBytes that of the
	// result as a direct call sends it to the Agent, or 0 for none.
	ArgsBytes, ResultBytes int
}

// Script is one call to execute.
type Script struct {
	N           int // 1 for the session's first Script, and so on
	Took        time.Duration
	Outcome     Outcome
	Err         string // with the Error outcome, why
	Calls       int    // the Calls it made
	Code        string
	ResultBytes int // the size of execute's result, as sent to the Agent
}

// errLimit is how many characters of an error an event keeps.
const errLimit = 1000

// Log is the Audit log of one sprut serve. Its file is created by Start,
// and events before that are dropped. After any failure to write, it logs
// one ERROR line and drops every later event. A nil Log drops every event.
type Log struct {
	root    string // sprut's state directory, or "" if there is none
	started time.Time
	log     *slog.Logger

	mu       sync.Mutex
	f        *os.File // nil until Start, and after a failure
	finished bool     // whether Start has been called, or the Log closed or failed
}

// New returns the Audit log of a sprut serve started at started, under
// $XDG_STATE_HOME/sprut/audit, or ~/.local/state/sprut/audit. Failures are
// logged to log.
func New(lookupEnv config.LookupEnv, started time.Time, log *slog.Logger) *Log {
	l := &Log{started: started, log: log}
	if dir, ok := config.XDGDir(lookupEnv, "XDG_STATE_HOME", filepath.Join(".local", "state")); ok {
		l.root = filepath.Join(dir, "sprut")
	}
	return l
}

// Start creates the file of the Agent's session s and writes s to it, unless
// it is already started.
func (l *Log) Start(s Session) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finished {
		return
	}
	l.finished = true
	f, err := l.create(s.Agent)
	if err != nil {
		l.fail(err)
		return
	}
	l.f = f
	l.write(session{newHeader("session"), s.Sprut, s.Agent, s.AgentVersion, s.Protocol, s.CodeMode})
}

// Call writes c.
func (l *Log) Call(c Call) {
	if l == nil {
		return
	}
	ev := call{header: newHeader("call"), Tool: c.Tool, Upstream: c.Upstream, MS: c.Took.Milliseconds(), Outcome: c.Outcome,
		Err: cut(c.Err), ArgsBytes: c.ArgsBytes, ResultBytes: c.ResultBytes}
	if c.Script != 0 {
		ev.Script = &c.Script
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.write(ev)
}

// Script writes s.
func (l *Log) Script(s Script) {
	if l == nil {
		return
	}
	ev := script{header: newHeader("script"), N: s.N, MS: s.Took.Milliseconds(), Outcome: s.Outcome, Err: cut(s.Err), Calls: s.Calls,
		Code: s.Code, CodeBytes: len(s.Code), ResultBytes: s.ResultBytes}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.write(ev)
}

// Close closes the file, after which every event is dropped.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finished = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// The events as written, each starting with its header.
type (
	header struct {
		TS   string `json:"ts"`
		Type string `json:"type"`
	}
	session struct {
		header
		Sprut        string `json:"sprut"`
		Agent        string `json:"agent"`
		AgentVersion string `json:"agent_version"`
		Protocol     string `json:"protocol"`
		CodeMode     bool   `json:"code_mode"`
	}
	call struct {
		header
		Tool        string  `json:"tool"`
		Upstream    string  `json:"upstream"`
		Script      *int    `json:"script"`
		MS          int64   `json:"ms"`
		Outcome     Outcome `json:"outcome"`
		Err         string  `json:"error,omitempty"`
		ArgsBytes   int     `json:"args_bytes"`
		ResultBytes int     `json:"result_bytes"`
	}
	script struct {
		header
		N           int     `json:"script"`
		MS          int64   `json:"ms"`
		Outcome     Outcome `json:"outcome"`
		Err         string  `json:"error,omitempty"`
		Calls       int     `json:"calls"`
		Code        string  `json:"code"`
		CodeBytes   int     `json:"code_bytes"`
		ResultBytes int     `json:"result_bytes"`
	}
)

// newHeader is the header of an event of type typ that happens now.
func newHeader(typ string) header {
	return header{TS: time.Now().Format("2006-01-02T15:04:05.000Z07:00"), Type: typ}
}

// write writes the event ev as one line, with l.mu held.
func (l *Log) write(ev any) {
	if l.f == nil {
		return
	}
	line, err := json.Marshal(ev)
	if err == nil {
		_, err = l.f.Write(append(line, '\n'))
	}
	if err != nil {
		l.fail(err)
	}
}

// fail logs err, closes the file and drops every later event, with l.mu
// held.
func (l *Log) fail(err error) {
	l.log.Error("audit log disabled", "err", err)
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	l.finished = true
}

// create creates the file of a session of the Agent named agent: under the
// Agent's directory, named by the start time, numbered if that is taken.
func (l *Log) create(agent string) (*os.File, error) {
	if l.root == "" {
		return nil, errors.New("cannot locate the audit log: HOME is not set")
	}
	dir := filepath.Join(l.root, "audit", agentDir(agent))
	if err := mkdirPrivate(l.root, dir); err != nil {
		return nil, err
	}
	base := l.started.Format("2006-01-02T15-04-05")
	for n := 1; ; n++ {
		name := base + ".jsonl"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.jsonl", base, n)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
}

// mkdirPrivate makes dir, which is under root, and every directory between
// them readable only by the user, including those that already exist.
func mkdirPrivate(root, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
		if d == root {
			return nil
		}
	}
}

// unsafe is a run of characters left out of an Agent's directory name.
var unsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// agentDir is the directory name of the Agent named agent: lowercase, with
// only safe characters, never empty and never a path of its own.
func agentDir(agent string) string {
	name := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(agent), "-"), "-.")
	if name == "" {
		return "unknown"
	}
	return name
}

// cut is s cut to errLimit characters.
func cut(s string) string {
	if r := []rune(s); len(r) > errLimit {
		return string(r[:errLimit])
	}
	return s
}
