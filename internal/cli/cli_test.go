package cli_test

import (
	"regexp"
	"strings"
	"testing"
)

func TestVersionFlagPrintsVersionToStdout(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := runSprut(t, nil, "--version")

	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if !regexp.MustCompile(`^sprut version \S+\n$`).MatchString(stdout) {
		t.Errorf("stdout = %q, want \"sprut version <version>\\n\"", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestHelpPrintsUsageToStdoutAndExits0(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want []string // in the help text
	}{
		{[]string{"-h"}, []string{"serve", "--version"}},
		{[]string{"--help"}, []string{"serve", "--version"}},
		{[]string{"serve", "-h"}, []string{"--config", "--startup-timeout", "--verbose", "--dry-run"}},
		{[]string{"serve", "--help"}, []string{"--config", "--startup-timeout", "--verbose", "--dry-run"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runSprut(t, nil, tc.args...)

			if code != 0 {
				t.Errorf("exit code = %d, want 0; stderr:\n%s", code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("help on stdout does not mention %s:\n%s", want, stdout)
				}
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// Bare sprut is a usage error too, so that it never starts the server.
func TestUsageErrorPrintsUsageToStderrAndExits2(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args  []string
		usage string // a line of the help text of the command at fault
		err   string // in the error message
	}{
		{nil, "sprut [global options]", ""},
		{[]string{"--bogus"}, "sprut [global options]", "bogus"},
		{[]string{"bogus"}, "sprut [global options]", `unknown command "bogus"`},
		{[]string{"serve", "--bogus"}, "sprut serve [options]", "bogus"},
		{[]string{"serve", "bogus"}, "sprut serve [options]", `unexpected argument "bogus"`},
		{[]string{"serve", "--startup-timeout", "soon"}, "sprut serve [options]", "soon"},
		{[]string{"serve", "--startup-timeout", "0s"}, "sprut serve [options]", "startup-timeout"},
		{[]string{"serve", "--startup-timeout", "-1s"}, "sprut serve [options]", "startup-timeout"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runSprut(t, nil, tc.args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.usage) {
				t.Errorf("stderr does not show usage %q:\n%s", tc.usage, stderr)
			}
			if !strings.Contains(stderr, tc.err) {
				t.Errorf("stderr does not name the error %q:\n%s", tc.err, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}
