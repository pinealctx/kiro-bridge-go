package main

import (
	"flag"
	"log"

	"kiro-bridge-go/api"
	"kiro-bridge-go/config"
	"kiro-bridge-go/cw"
	"kiro-bridge-go/token"
)

func main() {
	debug := flag.Bool("debug", false, "Enable debug logging")
	port := flag.Int("port", 0, "Server port (overrides config/env)")
	flag.Parse()

	cfg := config.Load()
	cfg.Debug = *debug
	if *port != 0 {
		cfg.Port = *port
	}

	if cfg.Debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
		log.Println("Debug mode enabled")
	}

	tm := token.NewManager(cfg.KiroDBPath, cfg.ProfileARN)

	// Probe token to detect external-idp vs legacy
	_, err := tm.GetAccessToken(cfg.IdcRefreshURL)
	if err != nil {
		log.Printf("WARNING: Could not load token: %v", err)
		log.Printf("Make sure kiro-cli is logged in before making requests")
	}

	client := cw.NewClient(cfg.CodeWhispererURL, cfg)
	client.IsExternalIdP = tm.IsExternalIdP

	server := api.NewServer(cfg, tm, client)
	if err := server.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
