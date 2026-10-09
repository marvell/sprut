package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// envStdio carries a Stdio spec, as JSON, to the fake Upstream process.
const envStdio = "SPRUT_TEST_FAKE_UPSTREAM"

// Lifetime bounds how long a fake Upstream or its child that only waits to
// be stopped stays alive: longer than any test, so a test that stops it
// proves something, but short enough that one the Gateway failed to stop
// does not linger on the machine.
const Lifetime = time.Minute

// Startup is how a fake stdio Upstream misbehaves at startup.
type Startup string

const (
	// Fail exits with an error before speaking MCP.
	Fail Startup = "fail"
	// Hang never answers and ignores its stdin closing, so only a signal
	// stops it.
	Hang Startup = "hang"
)

// Stdio is the behaviour of a fake stdio Upstream. The zero value offers
// echo and fail and behaves well. Fields named ...File are paths the fake
// writes to, for the test to read back.
type Stdio struct {
	// Protocol, if set, is the one protocol version it supports, which sets
	// its era.
	Protocol string  `json:",omitempty"`
	Startup  Startup `json:",omitempty"`

	// PIDFile gets its PID.
	PIDFile string `json:",omitempty"`
	// EnvFile gets its environment, one VAR=value per line.
	EnvFile string `json:",omitempty"`
	// Stderr is written to its stderr at startup.
	Stderr string `json:",omitempty"`

	// Rendezvous makes it mark its arrival in Rendezvous.Dir and not answer
	// until Rendezvous.N fakes have arrived there.
	Rendezvous *Rendezvous `json:",omitempty"`

	// IgnoreSIGTERM makes it ignore SIGTERM and its stdin closing, so only
	// SIGKILL stops it.
	IgnoreSIGTERM bool `json:",omitempty"`
	// GrandchildPIDFile makes it start a child that shares its stderr and
	// sleeps until killed, and gets that child's PID.
	GrandchildPIDFile string `json:",omitempty"`
	// LeaveGroupPIDFile makes it leave its process group for that of a
	// child that sleeps until killed, and gets the child's PID, which is
	// also the new group's. Signals to its old group no longer reach it,
	// and it ignores its stdin closing, so it never exits on its own.
	LeaveGroupPIDFile string `json:",omitempty"`
	// HangUpAfterList makes it exit 1 as soon as it has written its answer
	// to tools/list.
	HangUpAfterList bool `json:",omitempty"`

	// ExtraTools are names of further tools, each taking no arguments and
	// answering with ID, so a test can tell which Upstream answered.
	ExtraTools []string `json:",omitempty"`
	ID         string   `json:",omitempty"`
	// BadSchemaTool is the name of one more tool, whose input schema is not
	// an object schema.
	BadSchemaTool string `json:",omitempty"`
	// CrashTool adds a tool "crash" that makes it exit 1 mid-call, without
	// answering. It hangs up its stdout first and exits only once the
	// Gateway reacts with SIGTERM, so the Gateway always sees the exit after
	// closing the connection itself.
	CrashTool bool `json:",omitempty"`
	// OrphanToolPIDFile adds a tool "orphan" that starts a child sharing
	// its stdout and stderr, then exits 1 mid-call, without answering. The
	// child sleeps until killed, holding the Upstream's stdout and stderr
	// open; the file gets its PID.
	OrphanToolPIDFile string `json:",omitempty"`
	// BlockToolFile adds a tool "block" that writes "started" to the file,
	// blocks until the call is cancelled, then writes "cancelled" to it.
	BlockToolFile string `json:",omitempty"`
	// GatherTool, if positive, adds a tool "gather" that answers only once
	// GatherTool calls to it are in flight together.
	GatherTool int `json:",omitempty"`
	// AskTool adds a tool "ask" that asks for interactive input mid-call
	// (multi-round-trip), and answers once it has some.
	AskTool bool `json:",omitempty"`
}

// Rendezvous is where and how many fake Upstreams meet; see Stdio.
type Rendezvous struct {
	N   int
	Dir string
}

// Entry returns a Config entry that launches the fake Upstream that s
// describes, with env (VAR=value pairs) added to the entry's env. The test
// binary's TestMain must call MainIfFake.
func (s Stdio) Entry(t testing.TB, env ...string) map[string]any {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{envStdio: string(spec)}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	return map[string]any{"command": exe, "args": []string{"-test.run=^$"}, "env": vars}
}

// MainIfFake runs the fake stdio Upstream and exits, if Entry launched this
// process; otherwise it returns. Call it first thing in TestMain.
func MainIfFake() {
	if _, ok := os.LookupEnv(envStdio); ok {
		Main()
	}
}

// Main runs the fake stdio Upstream that SPRUT_TEST_FAKE_UPSTREAM describes
// as a Stdio in JSON, or a well-behaved one if it is unset, and exits. It
// is the command that a testscript Config launches.
func Main() {
	var s Stdio
	if v := os.Getenv(envStdio); v != "" {
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			os.Exit(1)
		}
	}
	os.Exit(s.run())
}

func (s Stdio) run() int {
	if s.IgnoreSIGTERM {
		signal.Ignore(syscall.SIGTERM)
		defer time.Sleep(Lifetime)
	}
	if err := s.prepare(); err != nil {
		fmt.Fprintln(os.Stderr, "fake upstream:", err)
		return 1
	}
	if s.LeaveGroupPIDFile != "" {
		defer time.Sleep(Lifetime)
	}

	switch s.Startup {
	case Fail:
		fmt.Fprintln(os.Stderr, "fake upstream: failing at startup")
		return 1
	case Hang:
		time.Sleep(Lifetime)
		return 1
	}

	server := NewServer(s.Protocol)
	// Set by crash, so that the stdin EOF the Gateway's SIGTERM follows
	// still exits 1.
	var crashed atomic.Bool
	s.addTools(server, &crashed)

	var transport mcp.Transport = &mcp.StdioTransport{}
	if s.HangUpAfterList {
		var listed atomic.Bool
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					listed.Store(true)
				}
				return next(ctx, method, req)
			}
		})
		transport = &mcp.IOTransport{Reader: os.Stdin, Writer: hangUpWriter{listed: &listed}}
	}
	if err := server.Run(context.Background(), transport); err != nil {
		fmt.Fprintln(os.Stderr, "fake upstream:", err)
		return 1
	}
	if crashed.Load() {
		return 1
	}
	return 0
}

// prepare does what s asks for before the fake speaks MCP.
func (s Stdio) prepare() error {
	if s.PIDFile != "" {
		if err := writePID(s.PIDFile, os.Getpid()); err != nil {
			return err
		}
	}
	if s.LeaveGroupPIDFile != "" {
		pid, err := startSleeper(s.LeaveGroupPIDFile, true, nil, nil)
		if err != nil {
			return err
		}
		if err := syscall.Setpgid(0, pid); err != nil {
			return err
		}
	}
	if s.EnvFile != "" {
		if err := os.WriteFile(s.EnvFile, []byte(strings.Join(os.Environ(), "\n")), 0o644); err != nil {
			return err
		}
	}
	if s.GrandchildPIDFile != "" {
		if _, err := startSleeper(s.GrandchildPIDFile, false, nil, os.Stderr); err != nil {
			return err
		}
	}
	if s.Stderr != "" {
		fmt.Fprint(os.Stderr, s.Stderr)
	}
	if s.Rendezvous != nil {
		return s.Rendezvous.meet()
	}
	return nil
}

// addTools adds the tools s asks for to server.
func (s Stdio) addTools(server *mcp.Server, crashed *atomic.Bool) {
	for _, name := range s.ExtraTools {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: ObjectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s.ID}}}, nil
			})
	}
	if s.CrashTool {
		server.AddTool(&mcp.Tool{Name: "crash", InputSchema: ObjectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				crashed.Store(true)
				sigterm := make(chan os.Signal, 1)
				signal.Notify(sigterm, syscall.SIGTERM)
				_ = os.Stdout.Close()
				select {
				case <-sigterm:
				case <-time.After(Lifetime):
				}
				os.Exit(1)
				return nil, nil
			})
	}
	if path := s.OrphanToolPIDFile; path != "" {
		server.AddTool(&mcp.Tool{Name: "orphan", InputSchema: ObjectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if _, err := startSleeper(path, false, os.Stdout, os.Stderr); err != nil {
					return nil, err
				}
				os.Exit(1)
				return nil, nil
			})
	}
	if path := s.BlockToolFile; path != "" {
		server.AddTool(&mcp.Tool{Name: "block", InputSchema: ObjectSchema},
			func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := os.WriteFile(path, []byte("started"), 0o644); err != nil {
					return nil, err
				}
				<-ctx.Done()
				return nil, os.WriteFile(path, []byte("cancelled"), 0o644)
			})
	}
	if s.GatherTool > 0 {
		server.AddTool(&mcp.Tool{Name: "gather", InputSchema: ObjectSchema}, gather(s.GatherTool))
	}
	if s.AskTool {
		server.AddTool(&mcp.Tool{Name: "ask", InputSchema: ObjectSchema},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if len(req.Params.InputResponses) == 0 {
					return &mcp.CallToolResult{
						InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Message: "Sure?"}},
					}, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answered"}}}, nil
			})
	}
	if name := s.BadSchemaTool; name != "" {
		// AddTool insists on an object schema, so the bad one is put into
		// the tools/list result on its way out.
		server.AddTool(&mcp.Tool{Name: name, InputSchema: ObjectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if list, ok := res.(*mcp.ListToolsResult); ok {
					for i, tool := range list.Tools {
						if tool.Name == name {
							bad := *tool
							bad.InputSchema = map[string]any{"type": "string"}
							list.Tools[i] = &bad
						}
					}
				}
				return res, err
			}
		})
	}
}

// hangUpWriter writes to stdout, and exits 1 after the first write once
// listed is set: the answer to tools/list, the only message the fake
// Upstream sends after it.
type hangUpWriter struct{ listed *atomic.Bool }

func (w hangUpWriter) Write(b []byte) (int, error) {
	n, err := os.Stdout.Write(b)
	if w.listed.Load() {
		os.Exit(1)
	}
	return n, err
}

func (hangUpWriter) Close() error { return os.Stdout.Close() }

// gather implements Stdio.GatherTool.
func gather(want int) mcp.ToolHandler {
	var arrived atomic.Int32
	all := make(chan struct{})
	return func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if int(arrived.Add(1)) == want {
			close(all)
		}
		select {
		case <-all:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "gathered"}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// startSleeper starts a process that sleeps until killed, sharing the given
// stdout and stderr, and in a process group of its own if ownGroup is set.
// It writes the process's PID to pidFile and returns it.
func startSleeper(pidFile string, ownGroup bool, stdout, stderr io.Writer) (int, error) {
	// By absolute path: the fake Upstream's environment has no PATH.
	cmd := exec.Command("/bin/sleep", strconv.Itoa(int(Lifetime.Seconds())))
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, writePID(pidFile, cmd.Process.Pid)
}

func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// meet implements Stdio.Rendezvous.
func (r *Rendezvous) meet() error {
	if err := os.WriteFile(filepath.Join(r.Dir, strconv.Itoa(os.Getpid())), nil, 0o644); err != nil {
		return err
	}
	for {
		entries, err := os.ReadDir(r.Dir)
		if err != nil {
			return err
		}
		if len(entries) >= r.N {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
