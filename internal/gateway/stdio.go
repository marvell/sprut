package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/config"
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

// connectStdio launches u's process, connects to it over its stdin and
// stdout, and lists its tools. env is the environment the process inherits.
func connectStdio(ctx context.Context, client *mcp.Client, u config.Upstream, env []string, log *slog.Logger) (*ready, error) {
	s := newStdioUpstream(u, env, log)
	session, err := client.Connect(ctx, s, nil)
	if err != nil {
		return nil, err
	}
	r, err := list(ctx, session)
	if err != nil {
		return nil, err
	}
	s.listed()
	return r, nil
}

// upstreamEnv is the environment of an Upstream process: its Config env
// layered on top of the inherited env. exec uses the last value of a
// duplicated key.
func upstreamEnv(env []string, extra map[string]string) []string {
	out := slices.Clone(env)
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		out = append(out, k+"="+extra[k])
	}
	return out
}

// stdioUpstream is the Transport to a stdio Upstream, and the process that
// connecting to it launches: in its own process group, talking MCP over its
// stdin and stdout, with its stderr logged line by line. Closing the
// connection stops the whole process group, so that the Upstream's own
// children go with it. The Upstream's exit is logged if it ends the session
// after its tools are listed.
type stdioUpstream struct {
	cmd *exec.Cmd
	log *slog.Logger

	// Set by Connect.
	pgid   int
	stdin  io.WriteCloser
	stdout *os.File      // read end
	exited chan struct{} // closed once the process is reaped and stderr drained

	mu       sync.Mutex
	state    state
	reaped   bool  // whether reap has run
	waitErr  error // from reaping it
	reported bool  // whether its exit is logged
}

// newStdioUpstream returns the Transport to u, whose process is launched on
// Connect. env is the environment the process inherits.
func newStdioUpstream(u config.Upstream, env []string, log *slog.Logger) *stdioUpstream {
	cmd := exec.Command(u.Command, u.Args...)
	cmd.Env = upstreamEnv(env, u.Env)
	return &stdioUpstream{cmd: cmd, log: log.With("upstream", u.Name)}
}

// listed records that the Upstream has listed its tools, from when on its
// exit is reported.
func (s *stdioUpstream) listed() { s.on(toolsListed) }

// Connect launches the Upstream. Writing to the connection writes to its
// stdin; closing the connection closes s.
func (s *stdioUpstream) Connect(ctx context.Context) (mcp.Connection, error) {
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &lineLogger{log: s.log}
	s.cmd.Stderr = stderr
	// Once the Upstream has exited, stop reading a stderr still held open by
	// something that outlived it.
	s.cmd.WaitDelay = drainGrace
	stdin, err := s.cmd.StdinPipe()
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
	s.cmd.Stdout = stdoutW
	err = s.cmd.Start()
	_ = stdoutW.Close() // the Upstream holds its own copy
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}

	s.pgid = s.cmd.Process.Pid // the leader's, by Setpgid
	s.stdin, s.stdout = stdin, stdout
	s.exited = make(chan struct{})
	go func() {
		err := s.cmd.Wait()
		end := s.on(upstreamEnded)
		if end != stopping {
			// What the Upstream left running in its group; Close, which
			// ending stdout leads to, kills whatever ignores this.
			_ = s.signal(syscall.SIGTERM)
		}
		// A descendant that inherited stdout would keep it from ending, and
		// so the connection from closing; the reader takes the deadline as
		// the end.
		_ = stdout.SetReadDeadline(time.Now().Add(drainGrace))
		stderr.flush()
		// Before exited is closed, so that an exit reported here is out once
		// Close returns.
		s.reap(err)
		close(s.exited)
	}()
	// The connection is closed by closing stdin (s), not stdout.
	return (&mcp.IOTransport{Reader: io.NopCloser(&eofReader{r: stdout, s: s}), Writer: s}).Connect(ctx)
}

// state is where a stdio Upstream is in its life, which decides whether its
// exit is reported and whether what it left in its group is stopped.
type state int

const (
	connecting state = iota // launched; its tools not listed yet
	serving                 // its tools listed
	stopping                // the Gateway ended the session, so its exit is expected
	quit                    // it ended the session before its tools were listed
	exited                  // it ended the session after they were listed: reported
)

// event is something that moves a stdio Upstream to another state.
type event int

const (
	toolsListed   event = iota // connectStdio listed its tools
	upstreamEnded              // its stdout ended, or it was reaped
	gatewayClosed              // the connection to it is being closed
)

// next is the state a stdio Upstream in state s moves to on e. Whoever ends
// the session first, the Gateway or the Upstream, settles the state. The
// only move after that is a quit Upstream turning out to have listed its
// tools after all, as when it answered tools/list and then hung up.
func next(s state, e event) state {
	switch {
	case e == toolsListed && s == connecting:
		return serving
	case e == toolsListed && s == quit:
		return exited
	case e == upstreamEnded && s == connecting:
		return quit
	case e == upstreamEnded && s == serving:
		return exited
	case e == gatewayClosed && (s == connecting || s == serving):
		return stopping
	}
	return s
}

// on moves s on e and returns the state s is in afterwards.
func (s *stdioUpstream) on(e event) state {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = next(s.state, e)
	s.report()
	return s.state
}

// reap records that s's process is reaped, with err from waiting for it,
// and that its stderr is drained.
func (s *stdioUpstream) reap(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaped, s.waitErr = true, err
	s.report()
}

// report logs, once, that the Upstream exited, as soon as it is both
// exited and reaped: whichever comes second, its exit or its tools turning
// out to be listed, reports it.
func (s *stdioUpstream) report() {
	if s.state != exited || !s.reaped || s.reported {
		return
	}
	s.reported = true
	err := s.waitErr
	if err == nil {
		err = errors.New("exit status 0")
	}
	s.log.Warn("upstream exited", "err", err)
}

func (s *stdioUpstream) Write(b []byte) (int, error) { return s.stdin.Write(b) }

// Close closes stdin and sends SIGTERM to the process group, then SIGKILL if
// any of it is still running after stopGrace. It gives up on an Upstream not
// reaped exitGrace later, such as one that can't be signalled or is stuck in
// the kernel, and logs it; the Upstream is still reaped if it ever exits.
func (s *stdioUpstream) Close() error {
	// No move if the Upstream ended the session first, as the SDK closes the
	// connection itself once stdout ends.
	s.on(gatewayClosed)
	_ = s.stdin.Close()
	sigErr := s.signal(syscall.SIGTERM)
	if !s.waitGone(stopGrace) {
		sigErr = errors.Join(sigErr, s.signal(syscall.SIGKILL))
		// SIGKILL can't be ignored; this only covers the reaping.
		s.waitGone(time.Second)
	}
	// A signal error only says why the Upstream didn't stop, as one that did
	// can get EPERM too (see signal).
	select {
	case <-s.exited:
	case <-time.After(exitGrace):
		attrs := []any{"pid", s.pgid}
		if sigErr != nil {
			attrs = append(attrs, "err", sigErr)
		}
		s.log.Warn("upstream did not stop", attrs...)
	}
	return s.stdout.Close()
}

// signal sends sig to the process group. A group already gone is no error,
// but on darwin one whose only member is its unreaped leader gives EPERM.
func (s *stdioUpstream) signal(sig syscall.Signal) error {
	err := syscall.Kill(-s.pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("sending %s: %w", sig, err)
}

// waitGone waits up to timeout for the whole process group to be gone, and
// reports whether it is. An unreaped leader still counts as a member.
func (s *stdioUpstream) waitGone(timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := syscall.Kill(-s.pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// eofReader reads from r, s's stdout, and moves s on upstreamEnded once r
// reaches its end. Its read deadline passing counts as the end too.
type eofReader struct {
	r io.Reader
	s *stdioUpstream
}

func (e *eofReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		err = io.EOF
	}
	if err == io.EOF {
		e.s.on(upstreamEnded)
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
