package cli_test

import (
	"os"
	"testing"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestMain(m *testing.M) {
	mcptest.MainIfFake()
	os.Exit(m.Run())
}
