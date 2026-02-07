package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

type Config struct {
	BaseDoc      string `yaml:"base_doc"`
	TemplatesDir string `yaml:"templates_dir"`
	DbPath       string
}

func loadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}

	configDir := filepath.Join(home, ".config", "tn")
	configFile := filepath.Join(configDir, "config.yaml")
	dbPath := filepath.Join(configDir, "tn.db")

	// Default config
	cfg := Config{
		BaseDoc:      filepath.Join(home, "Documents", "Obsidian"),
		TemplatesDir: filepath.Join(configDir, "templates"),
		DbPath:       dbPath,
	}

	log.Printf("Loading config from: %s", configFile)

	data, err := os.ReadFile(configFile)
	if os.IsNotExist(err) {
		log.Println("Config file not found, creating defaults.")
		_ = os.MkdirAll(cfg.BaseDoc, 0755)
		_ = os.MkdirAll(cfg.TemplatesDir, 0755)
		return cfg, nil
	} else if err != nil {
		return cfg, err
	}

	err = yaml.Unmarshal(data, &cfg)
	cfg.DbPath = dbPath // Ensure DB path is set regardless of yaml
	log.Printf("Config loaded. BaseDoc: %s", cfg.BaseDoc)
	return cfg, err
}

func main() {
	debug := flag.Bool("debug", false, "enable debug logging to debug.log")
	flag.Parse()

	if *debug {
		f, err := tea.LogToFile("debug.log", "debug")
		if err != nil {
			fmt.Println("fatal:", err)
			os.Exit(1)
		}
		defer f.Close()
		log.Println("--- Starting TN Debug Session ---")
	} else {
		log.SetOutput(io.Discard)
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Initialize DB
	// initDB is defined in db.go
	db, err := initDB(cfg.DbPath)
	if err != nil {
		fmt.Printf("Error initializing DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// initialModel is defined in ui.go
	p := tea.NewProgram(
		initialModel(cfg, db),
		tea.WithAltScreen(),       // Use full screen
		tea.WithMouseCellMotion(), // Allow scrolling in viewport
	)

	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}
