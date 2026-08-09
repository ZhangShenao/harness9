package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/harness9/internal/dashboard"
	"github.com/harness9/internal/mission"
)

func runDashboard(args []string) {
	addr := "127.0.0.1:7777"
	if len(args) > 0 {
		addr = args[0]
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("home dir: %v", err)
	}
	dbPath := filepath.Join(homeDir, ".harness9", "state.db")
	os.MkdirAll(filepath.Dir(dbPath), 0755)

	store, err := dashboard.OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	cs := mission.NewCommandService(store)

	srv := dashboard.NewServer(store, cs, addr)
	fmt.Printf("harness9 Mission Control Dashboard\n")
	fmt.Printf("Listening on http://%s\n", addr)
	fmt.Printf("Press Ctrl+C to stop\n")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("dashboard: %v", err)
	}
}
