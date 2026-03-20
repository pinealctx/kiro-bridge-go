package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"

	"kiro-bridge-go/api"
	"kiro-bridge-go/config"
	"kiro-bridge-go/cw"
	"kiro-bridge-go/token"
)

func main() {
	if len(os.Args) > 1 && os.Args[0] != "-" {
		switch os.Args[1] {
		case "login":
			runLogin(os.Args[2:])
			return
		}
	}
	runServe()
}

func runServe() {
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

	tm := token.NewManager(cfg.KiroDBPath, cfg.ProfileARN, cfg.TokenFilePath)

	// Probe token to detect external-idp vs legacy
	_, err := tm.GetAccessToken(cfg.IdcRefreshURL)
	if err != nil {
		log.Printf("WARNING: Could not load token: %v", err)
		log.Printf("Make sure kiro-cli is logged in or run './kiro-gateway login' first")
	}

	client := cw.NewClient(cfg.CodeWhispererURL, cfg)
	client.IsExternalIdP = tm.IsExternalIdP

	server := api.NewServer(cfg, tm, client)
	if err := server.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	port := fs.Int("port", token.DefaultCallbackPort, "Callback port for OAuth")
	fs.Parse(args)

	cfg := config.Load()

	am := token.NewAuthManager()
	session, err := am.StartLogin(*port)
	if err != nil {
		log.Fatalf("Failed to start login: %v", err)
	}

	fmt.Println()
	fmt.Println("Open this URL in your browser to log in:")
	fmt.Println()
	fmt.Println("  " + session.AuthURL)
	fmt.Println()

	// Auto-open on macOS
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", session.AuthURL).Start()
	}

	fmt.Printf("Waiting for login (timeout: 5 minutes)...\n")

	select {
	case <-session.Done():
	case <-time.After(5 * time.Minute):
		log.Fatal("Login timed out")
	}

	if session.Status != "completed" {
		log.Fatalf("Login failed: %s", session.Error)
	}

	lt := &token.LoginToken{
		AccessToken:   session.AccessToken,
		RefreshToken:  session.RefreshToken,
		ClientID:      session.ClientID,
		ClientSecret:  session.ClientSecret,
		TokenEndpoint: session.TokenEndpoint,
		ExpiresAt:     session.TokenExpiresAt,
		IsExternalIdP: session.IsExternalIdP(),
		ProfileArn:    session.ProfileArn,
	}

	lt.RefreshScope = token.ExtractRefreshScope(lt.AccessToken)

	if err := token.SaveLoginToken(cfg.TokenFilePath, lt); err != nil {
		log.Fatalf("Failed to save token: %v", err)
	}

	fmt.Println()
	fmt.Printf("Login successful! Token saved to %s\n", cfg.TokenFilePath)
	if lt.IsExternalIdP {
		fmt.Println("Auth type: External IdP (Microsoft OAuth2)")
	} else if session.IsBuilderID() {
		fmt.Println("Auth type: AWS Builder ID")
	} else {
		fmt.Println("Auth type: Direct (Kiro)")
	}
}
