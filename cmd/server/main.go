// Command server runs the local read-only web UI over the SQLite database
// populated by cmd/hook.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/papikayo/observability-code/internal/config"
	"github.com/papikayo/observability-code/internal/store"
	"github.com/papikayo/observability-code/internal/web"
)

func main() {
	path, err := config.DBPath()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(path)
	if err != nil {
		log.Fatalf("open store %s: %v", path, err)
	}
	defer st.Close()

	srv, err := web.NewServer(st)
	if err != nil {
		log.Fatal(err)
	}

	addr := os.Getenv("OBS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4790"
	}

	fmt.Printf("observability-code UI on http://%s  (db: %s)\n", addr, path)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}
