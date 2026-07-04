// Package config centralizes the environment-driven settings shared by
// cmd/hook and cmd/server, so the two binaries can't drift on where the
// database lives.
package config

import (
	"os"
	"path/filepath"
)

// DBPath returns the SQLite database path: $OBS_DB_PATH if set, otherwise
// ~/.observability-code/observability.db.
func DBPath() (string, error) {
	if p := os.Getenv("OBS_DB_PATH"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".observability-code", "observability.db"), nil
}
