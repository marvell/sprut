package cli_test

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantGone checks that the process whose PID is in pidFile (one of the
// ...PIDFile fields of mcptest.Stdio) no longer exists.
func wantGone(t *testing.T, pidFile string) {
	t.Helper()
	pid := readPID(t, pidFile)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d (%s) still exists after Gateway exit (kill -0: %v)", pid, pidFile, err)
	}
}

// waitForExit waits for the process whose PID is in pidFile to no longer
// exist, while the Gateway keeps running.
func waitForExit(t *testing.T, pidFile string) {
	t.Helper()
	pid := readPID(t, pidFile)
	var err error
	if !eventually(func() bool { err = syscall.Kill(pid, 0); return errors.Is(err, syscall.ESRCH) }) {
		t.Fatalf("process %d (%s) still exists (kill -0: %v)", pid, pidFile, err)
	}
}

// readPID reads the PID recorded in pidFile.
func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("PID not recorded: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitForFile waits for the file at path to hold want.
func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	var data []byte
	var err error
	if !eventually(func() bool { data, err = os.ReadFile(path); return err == nil && string(data) == want }) {
		t.Fatalf("%s holds %q (err %v), want %q", path, data, err, want)
	}
}

// eventually polls cond until it holds, for up to 10s, and reports whether it
// did.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// resultText returns the one text a tool result holds.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %v, want one text", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %v, want text", res.Content[0])
	}
	return text.Text
}

// toJSON normalises v through a JSON round trip, so that values decoded from
// the wire compare equal to the literals they were encoded from.
func toJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// callResult reduces a tool result to what the Upstream's tool produced: it
// drops the protocol's resultType and checks that the per-hop serverInfo in
// _meta names the Gateway, not the Upstream, before dropping it too.
func callResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	m := toJSON(t, res).(map[string]any)
	delete(m, "resultType")
	if meta, ok := m["_meta"].(map[string]any); ok {
		if info, ok := meta[mcp.MetaKeyServerInfo].(map[string]any); ok {
			if info["name"] != "sprut" {
				t.Errorf("result _meta serverInfo = %v, want the Gateway (sprut)", info)
			}
			delete(meta, mcp.MetaKeyServerInfo)
		}
		if len(meta) == 0 {
			delete(m, "_meta")
		}
	}
	return m
}

// wantEchoed checks that a call to fake__echo that returned res and err
// succeeded.
func wantEchoed(t *testing.T, res *mcp.CallToolResult, err error) {
	t.Helper()
	if err != nil || res.IsError {
		t.Errorf("fake__echo = %v, %v; want it to echo", res, err)
	}
}
