package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marvell/sprut/internal/cli"
)

func TestBareSprutPrintsUsageToStderrAndExits2(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer

	code := cli.Run(context.Background(), []string{"sprut"}, nil, strings.NewReader(""), &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "serve") {
		t.Errorf("stderr does not show usage mentioning serve:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}
