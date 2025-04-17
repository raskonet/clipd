package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"
)

func setupLogging() (*os.File, error) {
	// Ensure config dir exists
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not get home dir: %w", err)
	}
	configDir := filepath.Join(home, ".config", "sync-clipboard-tui")
	if err := os.MkdirAll(configDir, 0750); err != nil {
		return nil, fmt.Errorf("could not create config dir: %w", err)
	}

	logFilePath := filepath.Join(configDir, "debug.log")
	f, err := tea.LogToFile(logFilePath, "debug")
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}
	log.Printf("--- Session Started ---") // Mark start in log file
	return f, nil
}

func loadEnv() {
	// Try loading from current dir, then config dir
	godotenv.Load(".env") // Load from CWD first

	home, err := os.UserHomeDir()
	if err == nil {
		configEnv := filepath.Join(home, ".config", "sync-clipboard-tui", ".env")
		godotenv.Load(configEnv) // Load from config dir (overrides CWD if exists)
	}
}

func main() {
	logFile, err := setupLogging()
	if err != nil {
		fmt.Println("Error setting up logging:", err)
		os.Exit(1)
	}
	defer logFile.Close()

	loadEnv() // Load config from .env files

	serverURL := os.Getenv("SERVER_WS_URL")
	apiKey := os.Getenv("CLIPBOARD_API_KEY")
	if serverURL == "" || apiKey == "" {
		log.Fatal("Error: SERVER_WS_URL or CLIPBOARD_API_KEY not set in environment or .env file")
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "UnknownHost"
		log.Println("Warning: Could not get hostname:", err)
	}

	initialModel := NewModel(serverURL, apiKey, hostname)

	p := tea.NewProgram(initialModel, tea.WithAltScreen(), tea.WithMouseCellMotion()) // Enable mouse for viewport scrolling
	initialModel.programRef = p // Give model reference to program for sending messages

	if _, err := p.Run(); err != nil {
		log.Fatalf("Error running Bubbletea program: %v", err)
		fmt.Fprintf(os.Stderr, "Error running program: %v\n", err)
		os.Exit(1)
	}
}
