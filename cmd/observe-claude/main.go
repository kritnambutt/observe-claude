// Command observe-claude is the single, cross-platform entry point for the
// tool: `init` to register with Claude Code, `serve` to run the web UI, and
// `hook` (invoked by Claude Code itself). See internal/cli.
package main

import (
	"os"

	"github.com/papikayo/observability-code/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
