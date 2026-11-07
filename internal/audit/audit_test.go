package audit_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/marvell/sprut/internal/audit"
	"github.com/marvell/sprut/internal/config"
)

// started is when the sprut under test started, as its Audit log names it.
var started = time.Date(2026, 11, 7, 14, 3, 22, 0, time.Local)

// openLog returns an Audit log under a fresh state directory, which it
// also returns, and the buffer its errors are logged to.
func openLog(t *testing.T) (*audit.Log, string, *bytes.Buffer) {
	t.Helper()
	state := t.TempDir()
	var stderr bytes.Buffer
	l := audit.New(config.LookupIn([]string{"XDG_STATE_HOME=" + state}), started, slog.New(slog.NewTextHandler(&stderr, nil)))
	t.Cleanup(func() { _ = l.Close() })
	return l, state, &stderr
}

// session is a session event of an Agent named agent.
func session(agent string) audit.Session {
	return audit.Session{Sprut: "1.2.3", Agent: agent, AgentVersion: "0.0.1", Protocol: "2025-06-18", CodeMode: true}
}

// readEvents reads the events in the Audit log file at path, checking the
// ts and type every event has and dropping ts.
func readEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("line %q is not a JSON object: %v", scanner.Text(), err)
		}
		ts, _ := ev["ts"].(string)
		if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts); err != nil {
			t.Errorf("ts %q is not RFC3339 with milliseconds: %v", ts, err)
		}
		if _, ok := ev["type"].(string); !ok {
			t.Errorf("event %v has no type", ev)
		}
		delete(ev, "ts")
		events = append(events, ev)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Errorf("file doesn't end with a newline: %q", data)
	}
	return events
}

// logPath is where the Audit log of an Agent named dir is, under state.
func logPath(state, dir, name string) string {
	return filepath.Join(state, "sprut", "audit", dir, name)
}

func TestStartWritesSessionEventToFileNamedByAgentAndStartTime(t *testing.T) {
	t.Parallel()
	l, state, _ := openLog(t)
	l.Start(session("claude-code"))
	l.Start(session("second-start")) // only the first counts

	path := logPath(state, "claude-code", "2026-11-07T14-03-22.jsonl")
	want := []map[string]any{{
		"type": "session", "sprut": "1.2.3", "agent": "claude-code", "agent_version": "0.0.1",
		"protocol": "2025-06-18", "code_mode": true,
	}}
	if diff := cmp.Diff(want, readEvents(t, path)); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(state, "sprut"), 0o700},
		{filepath.Join(state, "sprut", "audit"), 0o700},
		{filepath.Dir(path), 0o700},
		{path, 0o600},
	} {
		fi, err := os.Stat(p.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != p.mode {
			t.Errorf("%s has mode %o, want %o", p.path, got, p.mode)
		}
	}
}

func TestStartMakesExistingDirectoriesPrivate(t *testing.T) {
	t.Parallel()
	l, state, _ := openLog(t)
	dir := logPath(state, "agent", "")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l.Start(session("agent"))
	for _, d := range []string{filepath.Join(state, "sprut"), filepath.Dir(filepath.Clean(dir)), dir} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s has mode %o, want 700", d, got)
		}
	}
}

func TestStartNamesAgentDirectoryWithSafeCharactersOnly(t *testing.T) {
	t.Parallel()
	for agent, want := range map[string]string{
		"claude-code":      "claude-code",
		"Claude Code":      "claude-code",
		"codex_mcp.client": "codex_mcp.client",
		"a / b":            "a-b",
		"../../etc":        "etc",
		"..":               "unknown",
		"Курсор":           "unknown",
		"":                 "unknown",
	} {
		l, state, _ := openLog(t)
		l.Start(session(agent))
		path := logPath(state, want, "2026-11-07T14-03-22.jsonl")
		if _, err := os.Stat(path); err != nil {
			entries, _ := os.ReadDir(filepath.Join(state, "sprut", "audit"))
			t.Errorf("agent %q: no file at %s (audit holds %v)", agent, path, entries)
		}
	}
}

func TestStartGivesTakenFileNameNumberedSuffix(t *testing.T) {
	t.Parallel()
	l, state, _ := openLog(t)
	for _, name := range []string{"2026-11-07T14-03-22.jsonl", "2026-11-07T14-03-22-2.jsonl"} {
		path := logPath(state, "agent", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("taken\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.Start(session("agent"))
	if got := readEvents(t, logPath(state, "agent", "2026-11-07T14-03-22-3.jsonl")); len(got) != 1 {
		t.Errorf("events = %v, want the session event", got)
	}
}

func TestCallsAndScriptsFollowSessionEvent(t *testing.T) {
	t.Parallel()
	l, state, _ := openLog(t)
	l.Start(session("agent"))
	l.Call(audit.Call{Tool: "fake__echo", Upstream: "fake", Took: 412 * time.Millisecond, Outcome: audit.OK, ArgsBytes: 15, ResultBytes: 120})
	l.Call(audit.Call{Tool: "fake__echo", Upstream: "fake", Script: 1, Took: 3 * time.Millisecond, Outcome: audit.Cancelled})
	l.Call(audit.Call{Tool: "fake__fail", Upstream: "fake", Script: 1, Outcome: audit.Error, Err: "it failed", ArgsBytes: 2, ResultBytes: 60})
	l.Script(audit.Script{N: 1, Took: 1840 * time.Millisecond, Outcome: audit.OK, Calls: 2, Code: `return "é"`, ResultBytes: 90})
	l.Script(audit.Script{N: 2, Outcome: audit.Error, Err: strings.Repeat("é", 1001), Code: "throw"})

	want := []map[string]any{
		{"type": "call", "tool": "fake__echo", "upstream": "fake", "script": nil, "ms": 412.0, "outcome": "ok", "args_bytes": 15.0, "result_bytes": 120.0},
		{"type": "call", "tool": "fake__echo", "upstream": "fake", "script": 1.0, "ms": 3.0, "outcome": "cancelled", "args_bytes": 0.0, "result_bytes": 0.0},
		{"type": "call", "tool": "fake__fail", "upstream": "fake", "script": 1.0, "ms": 0.0, "outcome": "error", "error": "it failed", "args_bytes": 2.0, "result_bytes": 60.0},
		{"type": "script", "script": 1.0, "ms": 1840.0, "outcome": "ok", "calls": 2.0, "code": `return "é"`, "code_bytes": 11.0, "result_bytes": 90.0},
		{"type": "script", "script": 2.0, "ms": 0.0, "outcome": "error", "error": strings.Repeat("é", 1000), "calls": 0.0, "code": "throw", "code_bytes": 5.0, "result_bytes": 0.0},
	}
	got := readEvents(t, logPath(state, "agent", "2026-11-07T14-03-22.jsonl"))
	if diff := cmp.Diff(want, got[1:]); diff != "" {
		t.Errorf("events after the session's (-want +got):\n%s", diff)
	}
}

func TestEventsBeforeStartAreDropped(t *testing.T) {
	t.Parallel()
	l, state, _ := openLog(t)
	l.Call(audit.Call{Tool: "fake__echo", Upstream: "fake", Outcome: audit.OK})
	l.Start(session("agent"))
	if got := readEvents(t, logPath(state, "agent", "2026-11-07T14-03-22.jsonl")); len(got) != 1 {
		t.Errorf("events = %v, want the session event alone", got)
	}
}

func TestFailureToCreateFileIsLoggedOnceAndDisablesLog(t *testing.T) {
	t.Parallel()
	l, state, stderr := openLog(t)
	// sprut's state directory is a file, so nothing can be made under it.
	if err := os.WriteFile(filepath.Join(state, "sprut"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l.Start(session("agent"))
	l.Call(audit.Call{Tool: "fake__echo", Upstream: "fake", Outcome: audit.OK})
	l.Script(audit.Script{N: 1, Outcome: audit.OK})

	if got := strings.Count(stderr.String(), "level=ERROR"); got != 1 {
		t.Errorf("logged %d ERROR lines, want 1:\n%s", got, stderr)
	}
	if !strings.Contains(stderr.String(), `msg="audit log disabled"`) {
		t.Errorf("log lacks the reason:\n%s", stderr)
	}
}

func TestLogWithoutHomeIsLoggedOnceAndDisabled(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	l := audit.New(config.LookupIn(nil), started, slog.New(slog.NewTextHandler(&stderr, nil)))
	l.Start(session("agent"))
	l.Call(audit.Call{Tool: "fake__echo", Upstream: "fake", Outcome: audit.OK})
	if got := strings.Count(stderr.String(), `level=ERROR msg="audit log disabled"`); got != 1 {
		t.Errorf("logged %d ERROR lines, want 1:\n%s", got, stderr.String())
	}
}

func TestNilLogDoesNothing(t *testing.T) {
	t.Parallel()
	var l *audit.Log
	l.Start(session("agent"))
	l.Call(audit.Call{})
	l.Script(audit.Script{})
	if err := l.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
