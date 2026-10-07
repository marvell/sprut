package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stopGrace is how long a stopped Upstream gets to exit after SIGTERM before
// it is killed.
const stopGrace = 5 * time.Second

// stdioTransport runs a stdio Upstream in its own process group and talks MCP
// to it over its stdin and stdout. The Upstream's stderr is logged line by
// line to log. Closing the connection stops the whole process group, so that
// the Upstream's own children go with it.
type stdioTransport struct {
	cmd *exec.Cmd
	log *slog.Logger
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Plain pipes rather than exec's, which Wait would close while they are
	// still being read.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutW.Close()
		return nil, err
	}
	t.cmd.Stdout, t.cmd.Stderr = stdoutW, stderrW
	err = t.cmd.Start()
	// The Upstream holds its own copies of the write ends.
	_ = stdoutW.Close()
	_ = stderrW.Close()
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	p := &process{
		cmd:        t.cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		exited:     make(chan struct{}),
		stderrDone: make(chan struct{}),
	}
	go func() {
		_ = t.cmd.Wait()
		close(p.exited)
	}()
	go func() {
		logLines(stderr, t.log)
		close(p.stderrDone)
	}()
	// The connection is closed by closing stdin (p), not stdout.
	return (&mcp.IOTransport{Reader: io.NopCloser(stdout), Writer: p}).Connect(ctx)
}

// process is a started stdio Upstream. Writing to it writes to its stdin;
// closing it stops it.
type process struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr *os.File      // read ends
	exited         chan struct{} // closed once the process is reaped
	stderrDone     chan struct{} // closed once stderr is drained
}

func (p *process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes stdin and sends SIGTERM to the process group, then SIGKILL if
// any of it is still running after stopGrace.
func (p *process) Close() error {
	_ = p.stdin.Close()
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if !p.waitGone(stopGrace) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		p.waitGone(stopGrace)
	}
	// stderr ends once nothing holds it open. Something that left the
	// process group may still, so stop reading it soon after.
	select {
	case <-p.stderrDone:
	case <-time.After(time.Second):
	}
	_ = p.stderr.Close()
	<-p.stderrDone
	return p.stdout.Close()
}

// waitGone waits up to timeout for the whole process group to be gone, and
// reports whether it is.
func (p *process) waitGone(timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.exited:
			// The group outlives its reaped leader while any member is left.
			if err := syscall.Kill(-p.cmd.Process.Pid, 0); errors.Is(err, syscall.ESRCH) {
				return true
			}
		default:
		}
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// logLines logs each line read from r until EOF.
func logLines(r io.Reader, log *slog.Logger) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		log.Info("upstream stderr", "line", sc.Text())
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Warn("upstream stderr no longer logged", "err", err)
		// Keep draining, or the Upstream blocks on a full pipe.
		_, _ = io.Copy(io.Discard, r)
	}
}
