// Command server runs the local read-only web UI. It is a thin wrapper around
// internal/cli, kept so `go run ./cmd/server` and existing docs keep working;
// the unified `observe-claude serve` subcommand shares the same code.
package main

import (
	"log"

	"github.com/papikayo/observability-code/internal/cli"
)

func main() {
	// Empty args → resolve from $OBS_ADDR / $OBS_DB_PATH and defaults.
	if err := cli.Serve("", "", false); err != nil {
		log.Fatal(err)
	}
}
