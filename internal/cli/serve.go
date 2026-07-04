package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/papikayo/observability-code/internal/config"
	"github.com/papikayo/observability-code/internal/store"
	"github.com/papikayo/observability-code/internal/web"
)

// Serve runs the read-only web UI over the SQLite database the hook populates.
// Empty addr/dbPath fall back to env vars ($OBS_ADDR / $OBS_DB_PATH) and then
// built-in defaults, so `observe-claude serve` needs no flags.
func Serve(addr, dbPath string, openBrowser bool) error {
	if dbPath == "" {
		p, err := config.DBPath()
		if err != nil {
			return err
		}
		dbPath = p
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store %s: %w", dbPath, err)
	}
	defer st.Close()

	srv, err := web.NewServer(st)
	if err != nil {
		return err
	}

	if addr == "" {
		if addr = os.Getenv("OBS_ADDR"); addr == "" {
			addr = "127.0.0.1:4790"
		}
	}

	url := "http://" + addr
	fmt.Printf("observe-claude UI on %s  (db: %s)\n", url, dbPath)
	if openBrowser {
		go openInBrowser(url)
	}
	return http.ListenAndServe(addr, srv.Routes())
}

// openInBrowser opens url with the platform's default handler. Best-effort:
// a failure just means the user opens the printed URL themselves.
func openInBrowser(url string) {
	time.Sleep(300 * time.Millisecond) // let the listener come up first
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
