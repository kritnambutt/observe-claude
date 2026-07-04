// Package cli is the single entry point behind the `observe-claude` binary.
// It multiplexes the tool's three jobs — being a hook (`hook`), serving the
// web UI (`serve`), and registering itself into Claude Code (`init`) — so the
// whole tool ships as one cross-platform executable.
package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/papikayo/observability-code/internal/hookrun"
)

// Version is stamped at build time via -ldflags "-X …cli.Version=vX.Y.Z".
var Version = "dev"

// Main dispatches a subcommand and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	switch args[0] {
	case "hook":
		// Never fail the user's Claude Code session on a hook error.
		if err := hookrun.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "observe-claude hook error:", err)
		}
		return 0
	case "serve", "server", "ui":
		return runServe(args[1:])
	case "init", "install":
		return runInit(args[1:])
	case "version", "-v", "--version":
		fmt.Println("observe-claude", Version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "observe-claude: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "listen address (default $OBS_ADDR or 127.0.0.1:4790)")
	db := fs.String("db", "", "SQLite path (default $OBS_DB_PATH or ~/.observability-code/observability.db)")
	open := fs.Bool("open", false, "open the UI in your browser")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := Serve(*addr, *db, *open); err != nil {
		fmt.Fprintln(os.Stderr, "observe-claude serve:", err)
		return 1
	}
	return 0
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	global := fs.Bool("global", false, "observe every project (~/.claude/settings.json) [default]")
	project := fs.Bool("project", false, "observe one project only (its .claude/settings.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	settingsPath, scope, err := settingsTarget(*project, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "observe-claude init:", err)
		return 1
	}
	_ = global
	if err := Install(settingsPath); err != nil {
		fmt.Fprintln(os.Stderr, "observe-claude init:", err)
		return 1
	}
	fmt.Println()
	fmt.Printf("Hooks registered for %s. Start (or restart) a Claude Code session, do some\n", scope)
	fmt.Println("work, then run:  observe-claude serve --open")
	return 0
}

func usage(w *os.File) {
	fmt.Fprint(w, `observe-claude — local observability for Claude Code sessions

Usage:
  observe-claude init [--global | --project [dir]]   register the hook into Claude Code
  observe-claude serve [--addr host:port] [--open]   run the web UI
  observe-claude hook                                (invoked by Claude Code; reads stdin)
  observe-claude version

Typical setup:
  observe-claude init            # register hooks for all projects
  observe-claude serve --open    # browse your sessions
`)
}
