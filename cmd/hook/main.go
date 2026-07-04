// Command hook is invoked once per Claude Code hook event. It is a thin
// wrapper around internal/hookrun, kept so `go install .../cmd/hook` and the
// legacy scripts/install-hooks.sh entry keep working; the unified
// `observe-claude hook` subcommand shares the same code.
package main

import (
	"fmt"
	"os"

	"github.com/papikayo/observability-code/internal/hookrun"
)

func main() {
	if err := hookrun.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "observability-code hook error:", err)
	}
	os.Exit(0)
}
