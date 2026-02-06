package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v3"
)

// --- Configuration ---

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

// --- Database & Indexing ---

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create Tracking Table
	queryTrack := `
	CREATE TABLE IF NOT EXISTS file_tracking (
		path TEXT PRIMARY KEY,
		hash TEXT,
		last_indexed TIMESTAMP
	);`
	if _, err := db.Exec(queryTrack); err != nil {
		return nil, err
	}

	// Create FTS5 Table
	// columns: path (unindexed identifier), filename (high weight), headers (medium), content (low)
	queryFTS := `
	CREATE VIRTUAL TABLE IF NOT EXISTS search_idx USING fts5(
		path UNINDEXED, 
		filename, 
		headers, 
		content
	);`
	if _, err := db.Exec(queryFTS); err != nil {
		return nil, err
	}

	return db, nil
}

func calculateHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func parseMarkdown(path string) (headers string, content string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var headerBuilder strings.Builder
	var contentBuilder strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			headerBuilder.WriteString(strings.TrimLeft(line, "# "))
			headerBuilder.WriteString(" ")
		} else {
			contentBuilder.WriteString(line)
			contentBuilder.WriteString("\n")
		}
	}
	return headerBuilder.String(), contentBuilder.String(), scanner.Err()
}

func indexFile(db *sql.DB, path string) error {
	hash, err := calculateHash(path)
	if err != nil {
		return err
	}

	// Check if update needed
	var storedHash string
	err = db.QueryRow("SELECT hash FROM file_tracking WHERE path = ?", path).Scan(&storedHash)
	if err == nil && storedHash == hash {
		return nil // No changes
	}

	log.Printf("Indexing file: %s", path)

	headers, content, err := parseMarkdown(path)
	if err != nil {
		return err
	}
	filename := filepath.Base(path)

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// Upsert tracking
	_, err = tx.Exec("INSERT OR REPLACE INTO file_tracking (path, hash, last_indexed) VALUES (?, ?, ?)",
		path, hash, time.Now())
	if err != nil {
		tx.Rollback()
		return err
	}

	// Delete old FTS entry if exists
	_, err = tx.Exec("DELETE FROM search_idx WHERE path = ?", path)
	if err != nil {
		tx.Rollback()
		return err
	}

	// Insert new FTS entry
	_, err = tx.Exec("INSERT INTO search_idx (path, filename, headers, content) VALUES (?, ?, ?, ?)",
		path, filename, headers, content)
	if err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

// --- Styles ---

var (
	docStyle = lipgloss.NewStyle().Margin(1, 2)

	listStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1).
			MarginRight(1)

	previewStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(0, 1)

	inputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("205")).
			Padding(1).
			Align(lipgloss.Center)

	infoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Padding(1)
)

// --- Types & Messages ---

type item struct {
	title, desc, path string
	isDir             bool
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

type appState int

const (
	stateBrowser appState = iota
	stateSearch
	stateInputName
	stateTemplateSelect
)

// Messages
type editorFinishedMsg struct{ err error }
type filesRefreshedMsg []list.Item
type searchResultMsg []list.Item
type indexingFinishedMsg struct{}

// --- Model ---

type model struct {
	config Config
	db     *sql.DB
	state  appState

	// View components
	fileList     list.Model
	searchList   list.Model
	templateList list.Model
	viewport     viewport.Model
	textInput    textinput.Model
	searchInput  textinput.Model

	// Logic state
	currentDir      string
	selectedFile    string
	pendingTemplate string
	width, height   int
	renderer        *glamour.TermRenderer
	fileToSelect    string // Track file to select after directory refresh
}

func initialModel(cfg Config, db *sql.DB) model {
	// Initialize File Browser List
	l := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Files"
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()

	// Initialize Search List
	sl := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	sl.Title = "Search Results (Ranked)"
	sl.SetShowHelp(false)
	sl.SetFilteringEnabled(false) // We do manual filtering via DB
	sl.SetShowFilter(false)
	sl.DisableQuitKeybindings()

	// Initialize Template List
	tl := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	tl.Title = "Select Template"

	// Initialize Text Input (Filename)
	ti := textinput.New()
	ti.Placeholder = "filename.md"
	ti.CharLimit = 156
	ti.Width = 40

	// Initialize Search Input
	si := textinput.New()
	si.Placeholder = "Type to search..."
	si.CharLimit = 100
	si.Width = 40

	// Markdown Renderer
	r, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(80),
	)

	return model{
		config:       cfg,
		db:           db,
		state:        stateBrowser,
		fileList:     l,
		searchList:   sl,
		templateList: tl,
		viewport:     viewport.New(0, 0),
		textInput:    ti,
		searchInput:  si,
		currentDir:   cfg.BaseDoc,
		renderer:     r,
	}
}

func (m model) Init() tea.Cmd {
	// Batch commands: start blinking cursor, refresh current dir, and start background indexing
	return tea.Batch(
		textinput.Blink,
		m.refreshFileListCmd(m.currentDir),
		m.syncDatabaseCmd(),
	)
}

// --- Commands (Async operations) ---

// Walks the current directory and returns a message with items
func (m model) refreshFileListCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("Error reading dir %s: %v", dir, err)
			return filesRefreshedMsg(nil)
		}

		var items []list.Item
		// Add ".." if not at root
		if dir != m.config.BaseDoc {
			parent := filepath.Dir(dir)
			items = append(items, item{title: "..", desc: "Go Back", path: parent, isDir: true})
		}

		for _, e := range entries {
			// Skip hidden files/dirs
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}

			info, _ := e.Info()
			desc := "File"
			if e.IsDir() {
				desc = "Directory"
			} else {
				desc = info.ModTime().Format("Jan 02 15:04")
			}

			items = append(items, item{
				title: e.Name(),
				desc:  desc,
				path:  filepath.Join(dir, e.Name()),
				isDir: e.IsDir(),
			})
		}
		log.Printf("refreshFileListCmd took %v for %d items", time.Since(start), len(items))
		return filesRefreshedMsg(items)
	}
}

// Performs FTS5 query
func (m model) searchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		if query == "" {
			return searchResultMsg(nil)
		}

		// Using prefix match on the query
		term := query + "*"

		// Weighted query: Filename (10), Headers (5), Content (1)
		q := `
		SELECT path, snippet(search_idx, 3, '>', '<', '...', 10) 
		FROM search_idx 
		WHERE search_idx MATCH ? 
		ORDER BY bm25(search_idx, 10.0, 5.0, 1.0) 
		LIMIT 20;`

		rows, err := m.db.Query(q, term)
		if err != nil {
			log.Printf("Search error: %v", err)
			return searchResultMsg(nil)
		}
		defer rows.Close()

		var items []list.Item
		for rows.Next() {
			var path, snip string
			if err := rows.Scan(&path, &snip); err != nil {
				continue
			}

			// Make relative path for display title
			rel, _ := filepath.Rel(m.config.BaseDoc, path)

			items = append(items, item{
				title: rel,
				desc:  snip, // Display snippet as description
				path:  path,
				isDir: false,
			})
		}
		return searchResultMsg(items)
	}
}

// Background sync of all files
func (m model) syncDatabaseCmd() tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		log.Println("Starting full DB sync...")

		err := filepath.WalkDir(m.config.BaseDoc, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && strings.HasPrefix(d.Name(), ".") && path != m.config.BaseDoc {
				return fs.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") && !strings.HasPrefix(d.Name(), ".") {
				if err := indexFile(m.db, path); err != nil {
					log.Printf("Failed to index %s: %v", path, err)
				}
			}
			return nil
		})

		if err != nil {
			log.Printf("Sync error: %v", err)
		}
		log.Printf("DB sync completed in %v", time.Since(start))
		return indexingFinishedMsg{}
	}
}

// Index single file (after edit)
func (m model) reindexFileCmd(path string) tea.Cmd {
	return func() tea.Msg {
		if err := indexFile(m.db, path); err != nil {
			log.Printf("Error re-indexing %s: %v", path, err)
		} else {
			log.Printf("Re-indexed %s", path)
		}
		return nil
	}
}

func (m *model) loadTemplates() {
	start := time.Now()
	entries, _ := os.ReadDir(m.config.TemplatesDir)
	var items []list.Item
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			items = append(items, item{
				title: e.Name(),
				desc:  "Template",
				path:  filepath.Join(m.config.TemplatesDir, e.Name()),
				isDir: false,
			})
		}
	}
	m.templateList.SetItems(items)
	log.Printf("loadTemplates took %v", time.Since(start))
}

func (m *model) updatePreview() {
	start := time.Now()
	sel := m.fileList.SelectedItem()
	if m.state == stateSearch {
		sel = m.searchList.SelectedItem()
	}

	if sel == nil {
		m.viewport.SetContent("")
		return
	}
	i := sel.(item)

	// Optimization: skip if file hasn't changed (checked by path)
	if i.path == m.selectedFile && m.viewport.View() != "" {
		// pass
	}

	m.selectedFile = i.path

	if i.isDir {
		m.viewport.SetContent(infoStyle.Render(fmt.Sprintf("Directory:\n%s", i.path)))
		return
	}

	if !strings.HasSuffix(strings.ToLower(i.path), ".md") {
		m.viewport.SetContent(infoStyle.Render(fmt.Sprintf("%s\n\nNot a markdown file.", filepath.Base(i.path))))
		return
	}

	content, err := os.ReadFile(i.path)
	readDuration := time.Since(start)

	if err != nil {
		log.Printf("Error reading file for preview %s: %v", i.path, err)
		m.viewport.SetContent(infoStyle.Render("Error reading file"))
		return
	}

	renderStart := time.Now()
	str, err := m.renderer.Render(string(content))
	renderDuration := time.Since(renderStart)

	if err != nil {
		log.Printf("Glamour render error: %v", err)
		m.viewport.SetContent(string(content))
	} else {
		m.viewport.SetContent(str)
	}

	totalDuration := time.Since(start)
	log.Printf("Preview Update: %s | Read: %v | Render: %v | Total: %v",
		filepath.Base(i.path), readDuration, renderDuration, totalDuration)
}

func openEditor(path string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}
	log.Printf("Opening editor %s for file %s", editor, path)
	c := exec.Command(editor, path)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{err}
	})
}

// --- Update ---

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		log.Printf("Window resize: %dx%d", msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height

		// Layout calculations
		listWidth := msg.Width / 3
		viewWidth := msg.Width - listWidth - 6 // borders/margins

		m.fileList.SetSize(listWidth, msg.Height-4)
		m.templateList.SetSize(msg.Width-4, msg.Height-4)

		// Search list needs less height because of the input box
		// approx 3 lines for input
		searchHeight := msg.Height - 4 - 3
		if searchHeight < 0 {
			searchHeight = 0
		}
		m.searchList.SetSize(msg.Width-4, searchHeight)

		m.viewport.Width = viewWidth
		m.viewport.Height = msg.Height - 4

		// Reset renderer with new width
		m.renderer, _ = glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(viewWidth))
		m.updatePreview()

	// Async Data handling
	case filesRefreshedMsg:
		m.fileList.SetItems(msg)
		// If we have a file to select (from search), find and select it
		if m.fileToSelect != "" {
			for idx, it := range msg {
				if it.(item).path == m.fileToSelect {
					m.fileList.Select(idx)
					break
				}
			}
			m.fileToSelect = "" // Clear after selection
		}
		m.updatePreview()

	case searchResultMsg:
		cmd = m.searchList.SetItems(msg)
		cmds = append(cmds, cmd)
		m.searchList.StopSpinner()
		m.updatePreview()

	case editorFinishedMsg:
		if msg.err != nil {
			log.Println("Editor finished with error:", msg.err)
		}
		// Re-read file list just in case names changed
		cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
		// Re-index the specific file that was edited
		cmds = append(cmds, m.reindexFileCmd(m.selectedFile))
		m.updatePreview()

	case indexingFinishedMsg:
		log.Println("Indexing finished notification received")

	case tea.KeyMsg:
		// Global Keys
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		}

		switch m.state {
		case stateBrowser:
			// If filtering, bubbles/list handles input
			if m.fileList.FilterState() == list.Filtering {
				break
			}

			switch msg.String() {
			case "q", "esc":
				return m, tea.Quit
			case "ctrl+n": // New File
				m.state = stateInputName
				m.textInput.Focus()
				m.textInput.SetValue("")
				m.pendingTemplate = ""
				return m, nil
			case "ctrl+t": // New from Template
				m.loadTemplates()
				m.state = stateTemplateSelect
				return m, nil
			case "ctrl+f": // Search
				m.state = stateSearch
				m.searchInput.Focus()
				m.searchInput.SetValue("")
				m.searchList.SetItems([]list.Item{})
				return m, textinput.Blink

			// Navigation
			case "enter", "right", "l":
				if i, ok := m.fileList.SelectedItem().(item); ok {
					if i.isDir {
						m.currentDir = i.path
						m.fileList.ResetSelected()
						cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
					} else {
						return m, openEditor(i.path)
					}
				}
			case "left", "h":
				if m.currentDir != m.config.BaseDoc {
					m.currentDir = filepath.Dir(m.currentDir)
					m.fileList.ResetSelected()
					cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
				}
			}

		case stateInputName:
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEnter:
				filename := m.textInput.Value()
				if !strings.HasSuffix(filename, ".md") {
					filename += ".md"
				}
				fullPath := filepath.Join(m.currentDir, filename)

				var content []byte
				if m.pendingTemplate != "" {
					content, _ = os.ReadFile(m.pendingTemplate)
				}
				_ = os.WriteFile(fullPath, content, 0644)

				m.state = stateBrowser
				// Refresh list and open editor
				cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
				return m, openEditor(fullPath)
			case tea.KeyEsc:
				m.state = stateBrowser
				m.textInput.Blur()
			}
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd

		case stateTemplateSelect:
			switch msg.String() {
			case "enter":
				if i, ok := m.templateList.SelectedItem().(item); ok {
					m.pendingTemplate = i.path
					m.state = stateInputName
					m.textInput.Focus()
					m.textInput.SetValue("")
				}
			case "esc":
				m.state = stateBrowser
			}
			m.templateList, cmd = m.templateList.Update(msg)
			return m, cmd

		case stateSearch:
			switch msg.Type {
			case tea.KeyEsc:
				m.state = stateBrowser
				m.searchInput.Blur()
				return m, nil
			case tea.KeyEnter:
				if i, ok := m.searchList.SelectedItem().(item); ok {
					m.state = stateBrowser
					m.currentDir = filepath.Dir(i.path)
					m.fileToSelect = i.path // Track file to select after refresh
					// Jump to dir, refresh, and show preview
					cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
					return m, tea.Batch(cmds...)
				}
			case tea.KeyDown, tea.KeyUp, tea.KeyPgDown, tea.KeyPgUp:
				m.searchList, cmd = m.searchList.Update(msg)
				// Update preview based on search list selection
				if m.searchList.SelectedItem() != nil {
					selPath := m.searchList.SelectedItem().(item).path
					if selPath != m.selectedFile {
						m.updatePreview()
					}
				}
				return m, cmd
			default:
				// Type in search input
				m.searchInput, cmd = m.searchInput.Update(msg)
				cmds = append(cmds, cmd)
				// Trigger search command
				cmds = append(cmds, m.searchCmd(m.searchInput.Value()))
				return m, tea.Batch(cmds...)
			}
		}
	}

	// Default updates based on state
	if m.state == stateBrowser {
		m.fileList, cmd = m.fileList.Update(msg)
		cmds = append(cmds, cmd)

		if m.fileList.SelectedItem() != nil {
			selPath := m.fileList.SelectedItem().(item).path
			if selPath != m.selectedFile {
				m.updatePreview()
			}
		}
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// --- View ---

func (m model) View() string {
	if m.width == 0 {
		return "Initializing UI..."
	}

	switch m.state {
	case stateSearch:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			docStyle.Render(m.searchInput.View()),
			docStyle.Render(m.searchList.View()),
			// Show preview below or to the side? The original requirement was preview on right.
			// But for search, we usually want full width results.
			// Let's stick to full width results + preview.
			// Wait, the original layout was Browser(Left) + Preview(Right).
			// Search replaces Browser? Or full screen?
			// The original code rendered search full screen.
			// Let's render Search List (Left) + Preview (Right) like browser?
			// Or just Search List + Input.
			// User asked "ctrl + f display Search Result Ranked"
			// Let's do Split view for Search too: Search Input+List (Left), Preview (Right)
		)
		// Actually, to keep it consistent with browser view (List Left, Preview Right):
		leftPane := lipgloss.JoinVertical(lipgloss.Left,
			inputStyle.Render(m.searchInput.View()),
			listStyle.Height(m.height-8).Render(m.searchList.View()),
		)
		return lipgloss.JoinHorizontal(
			lipgloss.Top,
			leftPane,
			previewStyle.Width(m.viewport.Width).Render(m.viewport.View()),
		)

	case stateTemplateSelect:
		return docStyle.Render(m.templateList.View())

	case stateInputName:
		title := "New Note"
		if m.pendingTemplate != "" {
			title = "New Note from Template"
		}
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			inputStyle.Render(
				fmt.Sprintf("%s\n\n%s", title, m.textInput.View()),
			),
		)

	default: // stateBrowser
		return lipgloss.JoinHorizontal(
			lipgloss.Top,
			listStyle.Render(m.fileList.View()),
			previewStyle.Width(m.viewport.Width).Render(m.viewport.View()),
		)
	}
}

// --- Main ---

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
	db, err := initDB(cfg.DbPath)
	if err != nil {
		fmt.Printf("Error initializing DB: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

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
