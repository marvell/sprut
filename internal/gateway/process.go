package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// stopGrace is how long a stopped Upstream gets to exit after SIGTERM
	// before it is killed.
	stopGrace = 5 * time.Second
	// drainGrace is how long the Upstream's stdout and stderr are still read
	// after it exits, for its last output, before they are cut off even if
	// something that outlived it holds them open.
	drainGrace = time.Second
	// exitGrace is how long a stopped Upstream whose process group is gone,
	// or was killed, still gets to be reaped and drained before it is given
	// up on.
	exitGrace = drainGrace + time.Second
	// maxStderrLine is the longest stderr line logged whole; a longer one is
	// logged in pieces.
	maxStderrLine = 64 << 10
)

// stdioTransport runs a stdio Upstream in its own process group and talks MCP
// to it over its stdin and stdout. The Upstream's stderr is logged line by
// line to log. Closing the connection stops the whole process group, so that
// the Upstream's own children go with it.
type stdioTransport struct {
	cmd  *exec.Cmd
	log  *slog.Logger
	proc *process // once connected
}

// started marks the Upstream as started: from now on, its exiting without
// being stopped is logged as a warning.
func (t *stdioTransport) started() { t.proc.started.Store(true) }

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &lineLogger{log: t.log}
	t.cmd.Stderr = stderr
	// Once the Upstream has exited, stop reading a stderr still held open by
	// something that outlived it.
	t.cmd.WaitDelay = drainGrace
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// A plain pipe rather than exec's, which Wait would close while it is
	// still being read.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	t.cmd.Stdout = stdoutW
	err = t.cmd.Start()
	_ = stdoutW.Close() // the Upstream holds its own copy
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}

	p := &process{
		pgid:   t.cmd.Process.Pid, // the leader's, by Setpgid
		log:    t.log,
		stdin:  stdin,
		stdout: stdout,
		exited: make(chan struct{}),
	}
	t.proc = p
	go func() {
		err := t.cmd.Wait()
		if !p.stopping.Load() {
			// What the Upstream left running in its group; Close, which
			// ending stdout leads to, kills whatever ignores this.
			_ = p.signal(syscall.SIGTERM)
		}
		// A descendant that inherited stdout would keep it from ending, and
		// so the connection from closing; the reader takes the deadline as
		// the end.
		_ = stdout.SetReadDeadline(time.Now().Add(drainGrace))
		stderr.flush()
		// Logged before exited is closed, so that it is out once Close returns.
		if p.started.Load() && !p.stopping.Load() {
			if err == nil {
				err = errors.New("exit status 0")
			}
			t.log.Warn("upstream exited", "err", err)
		}
		close(p.exited)
	}()
	// The connection is closed by closing stdin (p), not stdout.
	return (&mcp.IOTransport{Reader: io.NopCloser(&eofReader{r: stdout, eof: &p.hungUp}), Writer: p}).Connect(ctx)
}

// process is a started stdio Upstream. Writing to it writes to its stdin;
// closing it stops it.
type process struct {
	pgid   int
	log    *slog.Logger
	stdin  io.WriteCloser
	stdout *os.File      // read end
	exited chan struct{} // closed once the process is reaped and stderr drained

	started  atomic.Bool // see stdioTransport.started
	stopping atomic.Bool // set by Close, so that the exit it causes isn't reported
	hungUp   atomic.Bool // whether stdout reached EOF, as when the process exits
}

func (p *process) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close closes stdin and sends SIGTERM to the process group, then SIGKILL if
// any of it is still running after stopGrace. It gives up on an Upstream not
// reaped exitGrace later, such as one that can't be signalled or is stuck in
// the kernel, and logs it; the Upstream is still reaped if it ever exits.
func (p *process) Close() error {
	// The SDK closes the connection itself once stdout ends, which an
	// Upstream exiting on its own causes; that exit is still reported.
	if !p.hungUp.Load() {
		p.stopping.Store(true)
	}
	_ = p.stdin.Close()
	sigErr := p.signal(syscall.SIGTERM)
	if !p.waitGone(stopGrace) {
		sigErr = errors.Join(sigErr, p.signal(syscall.SIGKILL))
		// SIGKILL can't be ignored; this only covers the reaping.
		p.waitGone(time.Second)
	}
	// A signal error only says why the Upstream didn't stop, as one that did
	// can get EPERM too (see signal).
	select {
	case <-p.exited:
	case <-time.After(exitGrace):
		attrs := []any{"pid", p.pgid}
		if sigErr != nil {
			attrs = append(attrs, "err", sigErr)
		}
		p.log.Warn("upstream did not stop", attrs...)
	}
	return p.stdout.Close()
}

// signal sends sig to the process group. A group already gone is no error,
// but on darwin one whose only member is its unreaped leader gives EPERM.
func (p *process) signal(sig syscall.Signal) error {
	err := syscall.Kill(-p.pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("sending %s: %w", sig, err)
}

// waitGone waits up to timeout for the whole process group to be gone, and
// reports whether it is. An unreaped leader still counts as a member.
func (p *process) waitGone(timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := syscall.Kill(-p.pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// eofReader reads from r and records in eof that r reached its end. Its read
// deadline passing counts as the end too.
type eofReader struct {
	r   io.Reader
	eof *atomic.Bool
}

func (e *eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		err = io.EOF
	}
	if err == io.EOF {
		e.eof.Store(true)
	}
	return n, err
}

// lineLogger logs each line written to it.
type lineLogger struct {
	log *slog.Logger
	buf []byte // the line so far
}

func (l *lineLogger) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	for {
		line, rest, found := bytes.Cut(l.buf, []byte{'\n'})
		if !found {
			if len(l.buf) < maxStderrLine {
				return len(b), nil
			}
			line, rest = l.buf[:maxStderrLine], l.buf[maxStderrLine:]
		}
		l.logLine(line)
		l.buf = rest
	}
}

// flush logs what is left after the last newline.
func (l *lineLogger) flush() {
	if len(l.buf) > 0 {
		l.logLine(l.buf)
		l.buf = nil
	}
}

func (l *lineLogger) logLine(line []byte) {
	l.log.Info("upstream stderr", "line", string(bytes.TrimSuffix(line, []byte{'\r'})))
}
