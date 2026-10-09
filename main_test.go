package main

import (
	"strconv"
	"sync"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/marvell/sprut/internal/mcptest"
)

// TestMain makes this test binary also sprut itself, running main, and the
// fake stdio Upstream, each as a command on the scripts' PATH. sprut is
// there as sprut-main, so that the script command sprut can record its
// exit status; testscript would run a command of its own name instead.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"sprut-main":    main,
		"fake-upstream": mcptest.Main,
	})
}

// TestScripts runs testdata/script/*.txtar: sprut as a user runs it, a
// process of its own, from args, env and exit status to stdout and stderr.
//
// Besides the builtin commands, a script has:
//
//	sprut args...   runs sprut; with !, sprut must fail
//	status N        sprut's last run exited N
//
// A Config entry with "command": "fake-upstream" is a fake stdio Upstream;
// SPRUT_TEST_FAKE_UPSTREAM in its env is an mcptest.Stdio as JSON.
func TestScripts(t *testing.T) {
	t.Parallel()
	var statuses sync.Map // *testscript.TestScript → the exit status of its last sprut
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		RequireExplicitExec: true,
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"sprut": func(ts *testscript.TestScript, neg bool, args []string) {
				err := ts.Exec("sprut-main", args...)
				code := 0
				if err != nil {
					exit, ok := err.(interface{ ExitCode() int })
					if !ok {
						ts.Fatalf("sprut: %v", err)
					}
					code = exit.ExitCode()
				}
				statuses.Store(ts, code)
				if neg != (code != 0) {
					ts.Fatalf("sprut exited %d", code)
				}
			},
			"status": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) != 1 {
					ts.Fatalf("usage: status N")
				}
				code, ok := statuses.Load(ts)
				if !ok {
					ts.Fatalf("status: sprut has not run")
				}
				if want := args[0]; strconv.Itoa(code.(int)) != want {
					ts.Fatalf("sprut exited %d, want %s", code, want)
				}
			},
		},
	})
}
