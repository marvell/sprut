// Package audit writes the Audit log: one JSONL file per sprut serve, of
// the Agent's session, every Namespaced tool call that reaches an Upstream
// and every Script, with timings, outcomes and sizes. Arguments and results
// are never written, only their sizes; a Script's code is.
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
	"github.com/marvell/sprut/internal/statedir"
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
	Sprut        string `json:"sprut"` // sprut's version
	Agent        string `json:"agent"` // the Agent's clientInfo name, as it sent it
	AgentVersion string `json:"agent_version"`
	Protocol     string `json:"protocol"`
	CodeMode     bool   `json:"code_mode"`
}

// Call is a Namespaced tool call that reached an Upstream.
type Call struct {
	Tool     string        `json:"tool"` // the Namespaced tool
	Upstream string        `json:"upstream"`
	Script   int           `json:"-"` // the number of the Script that made it, or 0 for a direct call
	Took     time.Duration `json:"-"`
	Outcome  Outcome       `json:"outcome"`
	Err      string        `json:"error,omitempty"` // with the Error outcome, why
	// ArgsBytes is the size of the arguments' JSON; ResultBytes that of the
	// result as a direct call sends it to the Agent, or 0 for none.
	ArgsBytes   int `json:"args_bytes"`
	ResultBytes int `json:"result_bytes"`
}

// Script is one call to execute.
type Script struct {
	N           int           `json:"script"` // 1 for the session's first Script, and so on
	Took        time.Duration `json:"-"`
	Outcome     Outcome       `json:"outcome"`
	Err         string        `json:"error,omitempty"` // with the Error outcome, why
	Calls       int           `json:"calls"`           // the Calls it made
	Code        string        `json:"code"`
	ResultBytes int           `json:"result_bytes"` // the size of execute's result, as sent to the Agent
}

// errLimit is how many characters of an error an event keeps.
const errLimit = 1000

// Log is the Audit log of one sprut serve. Its file is created by Start,
// and events before that are dropped. After any failure to write, it logs
// one ERROR line and drops every later event. A nil Log drops every event.
type Log struct {
	root     string // sprut's state directory, or "" if there is none
	launched time.Time
	log      *slog.Logger

	mu      sync.Mutex
	f       *os.File // nil until Start, and after a failure or Close
	started bool     // whether Start or Close has been called
}

// New returns the Audit log of a sprut serve launched at launched, under
// sprut's state directory. Failures are logged to log.
func New(lookupEnv config.LookupEnv, launched time.Time, log *slog.Logger) *Log {
	root, _ := statedir.Dir(lookupEnv)
	return &Log{root: root, launched: launched, log: log}
}

// Start creates the file of the Agent's session s and writes s to it, unless
// it is already started.
func (l *Log) Start(s Session) {
	if l == nil {
		return
	}
	line, err := encode(struct {
		header
		Session
	}{newHeader("session"), s})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return
	}
	l.started = true
	if err == nil {
		l.f, err = l.create(s.Agent)
	}
	if err != nil {
		l.fail(err)
		return
	}
	l.write(line)
}

// Call writes c.
func (l *Log) Call(c Call) {
	if l == nil {
		return
	}
	c.Err = cut(c.Err)
	ev := struct {
		header
		Call
		Script *int  `json:"script"`
		MS     int64 `json:"ms"`
	}{header: newHeader("call"), Call: c, MS: c.Took.Milliseconds()}
	if c.Script != 0 {
		ev.Script = &c.Script
	}
	l.writeEvent(ev)
}

// Script writes s.
func (l *Log) Script(s Script) {
	if l == nil {
		return
	}
	s.Err = cut(s.Err)
	l.writeEvent(struct {
		header
		Script
		MS        int64 `json:"ms"`
		CodeBytes int   `json:"code_bytes"`
	}{newHeader("script"), s, s.Took.Milliseconds(), len(s.Code)})
}

// Close closes the file, after which every event is dropped.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// header starts every event.
type header struct {
	TS   string `json:"ts"`
	Type string `json:"type"`
}

// newHeader is the header of an event of type typ that happens now.
func newHeader(typ string) header {
	return header{TS: time.Now().Format("2006-01-02T15:04:05.000Z07:00"), Type: typ}
}

// encode is the line of the event ev.
func encode(ev any) ([]byte, error) {
	line, err := json.Marshal(ev)
	return append(line, '\n'), err
}

// writeEvent encodes ev, outside the lock, and writes it.
func (l *Log) writeEvent(ev any) {
	line, err := encode(ev)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if err != nil {
		l.fail(err)
		return
	}
	l.write(line)
}

// write writes line, with l.mu held.
func (l *Log) write(line []byte) {
	if l.f == nil {
		return
	}
	if _, err := l.f.Write(line); err != nil {
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
}

// create creates the file of a session of the Agent named agent: under the
// Agent's directory, named by the launch time, numbered if that is taken.
func (l *Log) create(agent string) (*os.File, error) {
	if l.root == "" {
		return nil, errors.New("cannot locate the audit log: HOME is not set")
	}
	dir := filepath.Join(l.root, "audit", agentDir(agent))
	if err := statedir.MkdirPrivate(l.root, dir); err != nil {
		return nil, err
	}
	base := l.launched.Format("2006-01-02T15-04-05")
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
	if len(s) <= errLimit {
		return s
	}
	n := 0
	for i := range s {
		if n == errLimit {
			return s[:i]
		}
		n++
	}
	return s
}
