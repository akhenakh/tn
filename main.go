package main

import (
	"bufio"
	"bytes"
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
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	_ "github.com/mattn/go-sqlite3"
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

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Enable WAL mode for better concurrency (Reader doesn't block Writer)
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		log.Printf("Failed to enable WAL mode: %v", err)
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

// isIndexable checks if the file extension is supported for text indexing
func isIndexable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown" || ext == ".txt"
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

	// Read first 512 bytes to ensure it's not binary content even if extension matches
	// simple heuristic: check for null bytes
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if n > 0 && bytes.Contains(buf[:n], []byte{0}) {
		return "", "", fmt.Errorf("binary content detected")
	}
	// Reset file pointer
	f.Seek(0, 0)
	scanner = bufio.NewScanner(f)

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

// indexFileWithTx indexes a file using an existing transaction.
// This is used for bulk indexing to avoid transaction overhead per file.
func indexFileWithTx(tx *sql.Tx, path string) error {
	// Strictly check extension before processing
	if !isIndexable(path) {
		return nil
	}

	hash, err := calculateHash(path)
	if err != nil {
		return err
	}

	// Check if update needed within the transaction
	var storedHash string
	err = tx.QueryRow("SELECT hash FROM file_tracking WHERE path = ?", path).Scan(&storedHash)
	if err == nil && storedHash == hash {
		return nil // No changes
	}

	log.Printf("Indexing file: %s", path)

	headers, content, err := parseMarkdown(path)
	if err != nil {
		return err // Maybe binary or read error, skip indexing
	}
	filename := filepath.Base(path)

	// Upsert tracking
	_, err = tx.Exec("INSERT OR REPLACE INTO file_tracking (path, hash, last_indexed) VALUES (?, ?, ?)",
		path, hash, time.Now())
	if err != nil {
		return err
	}

	// Delete old FTS entry if exists
	_, err = tx.Exec("DELETE FROM search_idx WHERE path = ?", path)
	if err != nil {
		return err
	}

	// Insert new FTS entry
	_, err = tx.Exec("INSERT INTO search_idx (path, filename, headers, content) VALUES (?, ?, ?, ?)",
		path, filename, headers, content)
	if err != nil {
		return err
	}

	return nil
}

// indexFile is a wrapper for single file indexing (e.g., after editing)
func indexFile(db *sql.DB, path string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := indexFileWithTx(tx, path); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// removeFileFromDB cleans up the database when a file is renamed or deleted
func removeFileFromDB(db *sql.DB, path string) {
	if _, err := db.Exec("DELETE FROM file_tracking WHERE path = ?", path); err != nil {
		log.Printf("Error deleting from file_tracking: %v", err)
	}
	if _, err := db.Exec("DELETE FROM search_idx WHERE path = ?", path); err != nil {
		log.Printf("Error deleting from search_idx: %v", err)
	}
}

// Helper to sanitize snippets for display
func sanitizeSnippet(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	re := regexp.MustCompile(`[\x00-\x1f\x7f]+`)
	s = re.ReplaceAllString(s, " ")
	reSpace := regexp.MustCompile(`\s+`)
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// Helper to truncate text to a specific width to prevent wrapping
func truncateText(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) > width {
		// Use ellipsis if we have space, otherwise just cut
		if width > 3 {
			return s[:width-3] + "..."
		}
		return s[:width]
	}
	return s
}

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

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Padding(1, 2)

	helpContent = `
TN - Terminal Note Manager

Key Bindings:

Navigation:
  ↑/↓, j/k       Navigate file list
  ←/h            Go up one directory
  →/l, Enter     Open file or enter directory
  esc, q         Quit application

Actions:
  ctrl+n         Create new note
  ctrl+t         Create note from template
  ctrl+f         Search notes
  c              Create directory
  r              Rename file/directory
  d              Delete file
  ?              Show/hide this help

Search Mode:
  d              Delete file
  esc            Exit search mode
  Enter          Open selected search result
  ↑/↓, j/k       Navigate search results

Template Selection:
  Enter          Select template
  esc            Cancel template selection

Input Modes:
  Enter          Confirm action
  esc            Cancel action

Delete Confirmation:
  Type 'yes' to confirm deletion
  esc            Cancel deletion
`
)

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
	stateHelp
	stateConfirmDelete
	stateCreateDir
	stateRename
)

// Messages
type editorFinishedMsg struct{ err error }
type filesRefreshedMsg []list.Item
type searchResultMsg []list.Item
type toggleHelpMsg struct{}
type deleteConfirmedMsg bool

// Background Scan Messages
type scanCompleteMsg struct{}
type scanTickMsg time.Time

// previewRenderMsg carries async preview rendering result
type previewRenderMsg struct {
	content string
	path    string
	id      int // To track if this is the most recent render request
}

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
	deleteInput  textinput.Model

	// Logic state
	currentDir       string
	selectedFile     string
	pendingTemplate  string
	fileToDelete     string // Track file to be deleted
	fileToRename     string // Track file to be renamed
	width, height    int
	listWidth        int    // Calculated inner width for list
	viewWidth        int    // Calculated inner width for viewport
	inputWidth       int    // Calculated inner width for input
	searchListHeight int    // Calculated height for search list content
	fileToSelect     string // Track file to select after directory refresh
	previewLoading   bool   // Track if preview is being rendered
	renderID         int    // Incremental ID to track current render request
	showHelp         bool   // Track if help is being shown
	searchQuery      string // Current search query for highlighting
}

func (m *model) updateFileListTitle() {
	if m.currentDir == m.config.BaseDoc {
		m.fileList.Title = "TOP"
	} else {
		m.fileList.Title = filepath.Base(m.currentDir)
	}
}

func initialModel(cfg Config, db *sql.DB) model {
	// Initialize File Browser List
	l := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	l.Title = "TOP" // Will be updated to current directory name
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()

	// Initialize Search List
	// Use a delegate with strict height settings to prevent wrapping issues
	searchDelegate := list.NewDefaultDelegate()
	searchDelegate.SetHeight(2)  // Title + 1 line description
	searchDelegate.SetSpacing(1) // Restore spacing for readability

	sl := list.New([]list.Item{}, searchDelegate, 0, 0)
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

	// Initialize Delete Confirmation Input
	di := textinput.New()
	di.Placeholder = "Type 'yes' to confirm deletion"
	di.CharLimit = 10
	di.Width = 30

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
		deleteInput:  di,
		currentDir:   cfg.BaseDoc,
	}
}

func (m model) Init() tea.Cmd {
	// Start with just the essential operations
	return tea.Batch(
		textinput.Blink,
		m.refreshFileListCmd(m.currentDir),
		m.startBackgroundScan(), // Start the first scan immediately
	)
}

// startBackgroundScan creates a command that walks the directory and indexes files using a batch transaction.
func (m model) startBackgroundScan() tea.Cmd {
	return func() tea.Msg {
		log.Println("Starting background database scan...")
		start := time.Now()

		tx, err := m.db.Begin()
		if err != nil {
			log.Printf("Failed to begin transaction for scan: %v", err)
			return scanCompleteMsg{}
		}

		// Rollback on panic or error (if not committed)
		defer func() {
			_ = tx.Rollback()
		}()

		count := 0
		err = filepath.WalkDir(m.config.BaseDoc, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors accessing paths
			}
			if d.IsDir() && strings.HasPrefix(d.Name(), ".") && path != m.config.BaseDoc {
				return fs.SkipDir
			}
			// Only index supported file types
			if !d.IsDir() && isIndexable(d.Name()) && !strings.HasPrefix(d.Name(), ".") {
				if err := indexFileWithTx(tx, path); err != nil {
					log.Printf("Failed to index %s: %v", path, err)
				} else {
					count++
				}
			}
			return nil
		})

		if err != nil {
			log.Printf("Background scan walk error: %v", err)
		} else {
			if err := tx.Commit(); err != nil {
				log.Printf("Failed to commit scan transaction: %v", err)
			} else {
				log.Printf("Background scan completed in %v. Processed %d files.", time.Since(start), count)
			}
		}

		return scanCompleteMsg{}
	}
}

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
		// Add ".." if not at root and not already at base directory
		if dir != m.config.BaseDoc {
			parent := filepath.Dir(dir)
			// Only allow navigation up if parent is still within or equal to base directory
			if parent == m.config.BaseDoc || strings.HasPrefix(parent, m.config.BaseDoc+string(filepath.Separator)) {
				items = append(items, item{title: "..", desc: "Go Back", path: parent, isDir: true})
			}
		}

		// Separate directories and files
		var dirs []list.Item
		var files []list.Item

		for _, e := range entries {
			// Skip hidden files/dirs
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}

			info, _ := e.Info()
			var desc string
			if e.IsDir() {
				desc = "Directory"
			} else {
				desc = info.ModTime().Format("Jan 02 15:04")
			}

			newItem := item{
				title: e.Name(),
				desc:  desc,
				path:  filepath.Join(dir, e.Name()),
				isDir: e.IsDir(),
			}

			if e.IsDir() {
				dirs = append(dirs, newItem)
			} else {
				files = append(files, newItem)
			}
		}

		// Sort directories and files alphabetically
		sort.Slice(dirs, func(i, j int) bool {
			return dirs[i].(item).title < dirs[j].(item).title
		})
		sort.Slice(files, func(i, j int) bool {
			return files[i].(item).title < files[j].(item).title
		})

		// Combine: directories first, then files
		items = append(items, dirs...)
		items = append(items, files...)
		log.Printf("refreshFileListCmd took %v for %d items", time.Since(start), len(items))
		return filesRefreshedMsg(items)
	}
}

// Performs FTS5 query
// Accepts width to enforce strict truncation
func (m model) searchCmd(query string, width int) tea.Cmd {
	return func() tea.Msg {
		if query == "" {
			return searchResultMsg(nil)
		}

		// Using prefix match on the query
		term := query + "*"

		// Escape single quotes in query to prevent SQL injection
		// FTS5 MATCH doesn't support parameter binding, so we must construct the query carefully
		escapedTerm := strings.ReplaceAll(term, "'", "''")

		// Weighted query: Filename (10), Headers (5), Content (1)
		// Reduced snippet context to 5 tokens to keep results compact
		q := fmt.Sprintf(`
		SELECT path, snippet(search_idx, 3, '>', '<', '...', 5) 
		FROM search_idx 
		WHERE search_idx MATCH '%s' 
		ORDER BY bm25(search_idx, 10.0, 5.0, 1.0) 
		LIMIT 20;`, escapedTerm)

		rows, err := m.db.Query(q)
		if err != nil {
			return searchResultMsg(nil)
		}
		defer rows.Close()

		// Calculate strict text limits based on width to prevent wrapping
		// We account for list padding/borders (approx 6 chars safety)
		safeWidth := width - 6
		if safeWidth < 10 {
			safeWidth = 10
		}

		var items []list.Item
		for rows.Next() {
			var path, snip string
			if err := rows.Scan(&path, &snip); err != nil {
				continue
			}

			// Clean snippet to avoid UI bugs
			snip = sanitizeSnippet(snip)

			// Hard truncate snippet (Description)
			snip = truncateText(snip, safeWidth)

			// Make relative path for display title and truncate it (Title)
			rel, _ := filepath.Rel(m.config.BaseDoc, path)
			rel = truncateText(rel, safeWidth)

			items = append(items, item{
				title: rel,
				desc:  snip,
				path:  path,
				isDir: false,
			})
		}
		return searchResultMsg(items)
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
		if !e.IsDir() && isIndexable(e.Name()) && !strings.HasPrefix(e.Name(), ".") {
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

func (m *model) updatePreview() tea.Cmd {
	sel := m.fileList.SelectedItem()
	if m.state == stateSearch {
		sel = m.searchList.SelectedItem()
	}

	if sel == nil {
		m.viewport.SetContent("")
		return nil
	}
	i := sel.(item)

	// Skip if file hasn't changed and not currently loading and no search query
	// For search mode, be less restrictive to ensure navigation works properly
	if i.path == m.selectedFile && !m.previewLoading && m.viewport.View() != "" && m.searchQuery == "" {
		// In search mode, always allow preview updates when navigating
		if m.state == stateSearch {
			// Force update if we're navigating in search mode
			m.viewport.SetContent(infoStyle.Render("Loading preview..."))
			m.previewLoading = true
			m.renderID++
			currentID := m.renderID
			return m.renderPreviewCmd(i.path, m.viewWidth, currentID)
		}
		return nil
	}

	m.selectedFile = i.path

	if i.isDir {
		m.viewport.SetContent(infoStyle.Render(fmt.Sprintf("Directory:\n%s", i.path)))
		return nil
	}

	if !isIndexable(i.path) {
		m.viewport.SetContent(infoStyle.Render(fmt.Sprintf("%s\n\nNot a supported file type for preview.", filepath.Base(i.path))))
		return nil
	}

	// Show loading state and render asynchronously
	m.previewLoading = true
	m.viewport.SetContent(infoStyle.Render("Loading preview..."))

	// Increment render ID to track this request
	m.renderID++
	currentID := m.renderID

	return m.renderPreviewCmd(i.path, m.viewWidth, currentID)
}

// renderPreviewCmd renders markdown content asynchronously using a fresh renderer
// to ensure concurrency safety and correct width handling.
func (m model) renderPreviewCmd(path string, width int, id int) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()

		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Error reading file for preview %s: %v", path, err)
			return previewRenderMsg{content: infoStyle.Render("Error reading file"), path: path, id: id}
		}
		readDuration := time.Since(start)

		renderStart := time.Now()

		// Create a local renderer to avoid race conditions with shared state.
		// Set word wrap to the specific viewport width to fix table/layout panics.
		// Safely handle width being too small.
		safeWidth := width - 4 // Account for padding/borders
		if safeWidth < 10 {
			safeWidth = 80 // Fallback default
		}

		renderer, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle("dark"), // Explicitly set style to ensure rendering
			glamour.WithWordWrap(safeWidth),
			glamour.WithPreservedNewLines(),
		)

		var str string
		if err == nil {
			str, err = renderer.Render(string(content))
		}

		renderDuration := time.Since(renderStart)

		if err != nil {
			log.Printf("Glamour render error: %v", err)
			// Fallback to raw text highlighting
			highlightedContent := highlightMatches(string(content), m.searchQuery)
			return previewRenderMsg{content: highlightedContent, path: path, id: id}
		}

		// Highlight search matches in the rendered content
		highlightedContent := highlightMatches(str, m.searchQuery)

		log.Printf("Preview Render: %s | Read: %v | Render: %v | Total: %v",
			filepath.Base(path), readDuration, renderDuration, time.Since(start))

		return previewRenderMsg{content: highlightedContent, path: path, id: id}
	}
}

func processTemplate(templatePath string) ([]byte, error) {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("template").Funcs(sprig.FuncMap()).Parse(string(content))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func highlightMatches(text, query string) string {
	if query == "" {
		return text
	}

	// Escape special regex characters
	escapedQuery := regexp.QuoteMeta(query)

	// Create case-insensitive regex pattern
	pattern := "(?i)(" + escapedQuery + ")"
	re := regexp.MustCompile(pattern)

	// Highlight matches with ANSI escape codes for red background
	// Red background (41) with white text (37)
	return re.ReplaceAllString(text, "\x1b[37;41m$1\x1b[0m")
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		log.Printf("Window resize: %dx%d", msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height

		// Calculate exact widths
		// 1. List is strictly 30% of total width
		listOuterWidth := int(float64(msg.Width) * 0.30)
		if listOuterWidth < 20 {
			listOuterWidth = 20
		}

		// 2. Preview takes the remaining width
		previewOuterWidth := msg.Width - listOuterWidth

		// 3. Calculate inner widths by subtracting styling overhead
		// listStyle has Border(2) + Padding(2) + MarginRight(1) = 5
		m.listWidth = listOuterWidth - 5

		// previewStyle has Border(2) + Padding(2) = 4
		m.viewWidth = previewOuterWidth - 4

		// 4. Calculate input width for search mode
		// Input style has Border(2) + Padding(2) = 4
		// To align with the list column, we want Input Outer to equal List Outer - Margin(1)
		// InputInner = (ListOuter - 1) - InputOverhead(4) = ListOuter - 5
		// This happens to be the same as m.listWidth!
		m.inputWidth = m.listWidth

		// Safety checks
		if m.listWidth < 10 {
			m.listWidth = 10
		}
		if m.viewWidth < 10 {
			m.viewWidth = 10
		}
		if m.inputWidth < 10 {
			m.inputWidth = 10
		}

		m.fileList.SetSize(m.listWidth, msg.Height-4)
		m.templateList.SetSize(msg.Width-4, msg.Height-4)

		// Search list needs less height because of the input box
		// We calculate this dynamically to ensure the input box is never pushed off screen.
		// Render a dummy input box to measure its true vertical height with current style.
		// We use empty string to get minimum height of the input widget style
		renderedInput := inputStyle.Width(m.inputWidth).Render(m.searchInput.View())
		inputBoxHeight := lipgloss.Height(renderedInput)

		// Get the vertical frame size (borders/padding) of the list container.
		listFrameHeight := listStyle.GetVerticalFrameSize()

		// Calculate remaining height for the list component content.
		// Total Window Height - Input Box Height - List Container Borders
		// We subtract 1 line to ensure we don't hit the absolute edge of the terminal, causing scroll.
		searchHeight := msg.Height - inputBoxHeight - listFrameHeight - 1

		if searchHeight < 0 {
			searchHeight = 0
		}
		m.searchListHeight = searchHeight
		m.searchList.SetSize(m.listWidth, m.searchListHeight)

		m.viewport.Width = m.viewWidth
		m.viewport.Height = msg.Height - 4

		// Trigger preview update with new dimensions
		cmd = m.updatePreview()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

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
		cmd = m.updatePreview()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case searchResultMsg:
		cmd = m.searchList.SetItems(msg)
		cmds = append(cmds, cmd)
		m.searchList.StopSpinner()
		cmd = m.updatePreview()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case editorFinishedMsg:
		if msg.err != nil {
			log.Println("Editor finished with error:", msg.err)
		}
		// Re-read file list just in case names changed
		cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
		// Re-index the specific file that was edited
		cmds = append(cmds, m.reindexFileCmd(m.selectedFile))
		cmd = m.updatePreview()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case scanCompleteMsg:
		// When scan is done, wait 30 seconds before next scan
		return m, tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
			return scanTickMsg(t)
		})

	case scanTickMsg:
		// Trigger the next background scan
		return m, m.startBackgroundScan()

	case previewRenderMsg:
		// Only update if this is the most recent render request
		if msg.id == m.renderID {
			m.viewport.SetContent(msg.content)
			m.previewLoading = false
		}
	// Otherwise, this is an outdated render result, ignore it

	case tea.KeyMsg:
		// Global Keys
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
			return m, nil
		}

		switch m.state {
		case stateBrowser:
			// If filtering, bubbles/list handles input
			if m.fileList.FilterState() == list.Filtering {
				// Let the list component handle all input when filtering
				m.fileList, cmd = m.fileList.Update(msg)
				cmds = append(cmds, cmd)

				if m.fileList.SelectedItem() != nil {
					selPath := m.fileList.SelectedItem().(item).path
					if selPath != m.selectedFile {
						previewCmd := m.updatePreview()
						if previewCmd != nil {
							cmds = append(cmds, previewCmd)
						}
					}
				}
				m.viewport, cmd = m.viewport.Update(msg)
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			}

			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "esc":
				if m.showHelp {
					m.showHelp = false
					return m, nil
				}
				return m, tea.Quit
			case "ctrl+n": // New File
				m.state = stateInputName
				m.textInput.Focus()
				m.textInput.SetValue("")
				m.pendingTemplate = ""
				return m, nil
			case "c": // Create Directory
				m.state = stateCreateDir
				m.textInput.Focus()
				m.textInput.SetValue("")
				m.textInput.Placeholder = "New directory name"
				return m, nil
			case "r": // Rename
				if i, ok := m.fileList.SelectedItem().(item); ok {
					m.fileToRename = i.path
					m.state = stateRename
					m.textInput.Focus()
					m.textInput.SetValue(i.title)
					m.textInput.Placeholder = "New name"
					return m, nil
				}
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
			case "d": // Delete file
				if i, ok := m.fileList.SelectedItem().(item); ok && !i.isDir {
					m.fileToDelete = i.path
					m.state = stateConfirmDelete
					m.deleteInput.Focus()
					m.deleteInput.SetValue("")
					return m, textinput.Blink
				}

			// Navigation
			case "enter":
				if i, ok := m.fileList.SelectedItem().(item); ok {
					if i.isDir {
						// Normalize paths for comparison
						cleanItemPath := filepath.Clean(i.path)
						cleanBase := filepath.Clean(m.config.BaseDoc)
						// Only navigate into directory if it's within base directory
						if cleanItemPath == cleanBase || strings.HasPrefix(cleanItemPath, cleanBase) {
							m.currentDir = i.path
							m.updateFileListTitle()
							m.fileList.ResetSelected()
							m.fileList.ResetFilter()
							cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
							return m, tea.Batch(cmds...)
						}
					} else {
						m.fileList.ResetFilter()
						return m, openEditor(i.path)
					}
				}
			case "l", "right":
				if i, ok := m.fileList.SelectedItem().(item); ok {
					if i.isDir {
						// Normalize paths for comparison
						cleanItemPath := filepath.Clean(i.path)
						cleanBase := filepath.Clean(m.config.BaseDoc)
						// Only navigate into directory if it's within base directory
						if cleanItemPath == cleanBase || strings.HasPrefix(cleanItemPath, cleanBase) {
							m.currentDir = i.path
							m.updateFileListTitle()
							m.fileList.ResetSelected()
							m.fileList.ResetFilter()
							cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
							return m, tea.Batch(cmds...)
						}
					} else {
						m.fileList.ResetFilter()
						return m, openEditor(i.path)
					}
				}
			case "left", "h":
				if m.currentDir != m.config.BaseDoc {
					parent := filepath.Dir(m.currentDir)
					// Normalize paths for comparison by cleaning them
					cleanParent := filepath.Clean(parent)
					cleanBase := filepath.Clean(m.config.BaseDoc)
					// Only navigate up if parent is still within or equal to base directory
					if cleanParent == cleanBase || strings.HasPrefix(cleanParent, cleanBase) {
						m.currentDir = parent
						m.updateFileListTitle()
						m.fileList.ResetSelected()
						cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
						return m, tea.Batch(cmds...)
					}
				}
			}

		case stateCreateDir:
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEnter:
				dirName := m.textInput.Value()
				if dirName != "" {
					newPath := filepath.Join(m.currentDir, dirName)
					if err := os.Mkdir(newPath, 0755); err != nil {
						log.Printf("Error creating directory: %v", err)
					}
				}
				m.state = stateBrowser
				m.textInput.SetValue("")
				return m, m.refreshFileListCmd(m.currentDir)
			case tea.KeyEsc:
				m.state = stateBrowser
				m.textInput.Blur()
				m.textInput.SetValue("")
			}
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd

		case stateRename:
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEnter:
				newName := m.textInput.Value()
				if newName != "" {
					newPath := filepath.Join(filepath.Dir(m.fileToRename), newName)
					if err := os.Rename(m.fileToRename, newPath); err != nil {
						log.Printf("Error renaming: %v", err)
					} else {
						// Clean up DB for the old file so it doesn't show up in search
						removeFileFromDB(m.db, m.fileToRename)
					}
				}
				m.state = stateBrowser
				m.textInput.SetValue("")
				return m, m.refreshFileListCmd(m.currentDir)
			case tea.KeyEsc:
				m.state = stateBrowser
				m.textInput.Blur()
				m.textInput.SetValue("")
			}
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd

		case stateInputName:
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEnter:
				filename := m.textInput.Value()
				if !strings.HasSuffix(filename, ".md") && !strings.HasSuffix(filename, ".markdown") && !strings.HasSuffix(filename, ".txt") {
					filename += ".md"
				}
				fullPath := filepath.Join(m.currentDir, filename)

				// Check if file already exists
				if _, err := os.Stat(fullPath); err == nil {
					// File exists, show error message and don't create
					m.textInput.SetValue("")
					m.textInput.Placeholder = "File already exists! Press Esc to cancel"
					return m, nil
				} else if !os.IsNotExist(err) {
					// Other error occurred
					log.Printf("Error checking file existence: %v", err)
				}

				var content []byte
				if m.pendingTemplate != "" {
					content, _ = processTemplate(m.pendingTemplate)
				}
				_ = os.WriteFile(fullPath, content, 0644)

				m.state = stateBrowser
				// Refresh list and open editor
				return m, tea.Batch(m.refreshFileListCmd(m.currentDir), openEditor(fullPath))
			case tea.KeyEsc:
				m.state = stateBrowser
				m.textInput.Blur()
			}
			m.textInput, cmd = m.textInput.Update(msg)
			return m, cmd

		case stateConfirmDelete:
			switch msg.Type {
			case tea.KeyEnter:
				if strings.EqualFold(m.deleteInput.Value(), "yes") {
					err := os.Remove(m.fileToDelete)
					if err != nil {
						log.Printf("Error deleting file %s: %v", m.fileToDelete, err)
					} else {
						log.Printf("Deleted file: %s", m.fileToDelete)
						removeFileFromDB(m.db, m.fileToDelete)
					}
					m.fileToDelete = ""
					m.state = stateBrowser
					return m, tea.Batch(m.refreshFileListCmd(m.currentDir))
				} else {
					m.deleteInput.SetValue("")
					m.state = stateBrowser
					return m, nil
				}
			case tea.KeyEsc:
				m.fileToDelete = ""
				m.state = stateBrowser
				m.deleteInput.Blur()
			case tea.KeyCtrlC:
				return m, tea.Quit
			}
			m.deleteInput, cmd = m.deleteInput.Update(msg)
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
			switch msg.String() {
			case "esc":
				m.state = stateBrowser
				m.searchInput.Blur()
				m.searchQuery = ""
				return m, nil
			case "enter":
				if m.searchInput.Focused() && m.searchList.SelectedItem() == nil {
					// If strictly focused with no selection, maybe do something else
					// But we want to open the item if one is selected in the list
				}
				if i, ok := m.searchList.SelectedItem().(item); ok {
					m.state = stateBrowser
					m.currentDir = filepath.Dir(i.path)
					m.updateFileListTitle()
					m.fileToSelect = i.path
					cmds = append(cmds, m.refreshFileListCmd(m.currentDir))
					return m, tea.Batch(cmds...)
				}
			case "d":
				if m.searchInput.Focused() {
					m.searchInput, cmd = m.searchInput.Update(msg)
					cmds = append(cmds, cmd)
					return m, tea.Batch(cmds...)
				}
				if i, ok := m.searchList.SelectedItem().(item); ok && !i.isDir {
					m.fileToDelete = i.path
					m.state = stateConfirmDelete
					m.deleteInput.Focus()
					m.deleteInput.SetValue("")
					return m, textinput.Blink
				}
			case "up", "k", "down", "j":
				// Handle navigation in the list even if input is focused
				m.searchList, cmd = m.searchList.Update(msg)
				cmds = append(cmds, cmd)

				// Trigger preview update immediately upon navigation
				previewCmd := m.updatePreview()
				if previewCmd != nil {
					cmds = append(cmds, previewCmd)
				}
				return m, tea.Batch(cmds...)

			default:
				m.searchInput, cmd = m.searchInput.Update(msg)
				cmds = append(cmds, cmd)
				m.searchQuery = m.searchInput.Value()
				// Pass the current calculated list width to the search command to enforce truncation
				cmds = append(cmds, m.searchCmd(m.searchInput.Value(), m.listWidth))
				previewCmd := m.updatePreview()
				if previewCmd != nil {
					cmds = append(cmds, previewCmd)
				}
				return m, tea.Batch(cmds...)
			}
		}
	}

	// Default updates based on state
	if m.state == stateBrowser {
		// Only update the list if this is not a navigation key we've already handled
		if keyMsg, isKey := msg.(tea.KeyMsg); !isKey || (keyMsg.String() != "enter" && keyMsg.String() != "l" && keyMsg.String() != "right" && keyMsg.String() != "left" && keyMsg.String() != "h") {
			m.fileList, cmd = m.fileList.Update(msg)
			cmds = append(cmds, cmd)
		}

		if m.fileList.SelectedItem() != nil {
			selPath := m.fileList.SelectedItem().(item).path
			if selPath != m.selectedFile {
				previewCmd := m.updatePreview()
				if previewCmd != nil {
					cmds = append(cmds, previewCmd)
				}
			}
		}
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if m.width == 0 {
		return "Initializing UI..."
	}

	switch m.state {
	case stateSearch:
		// Split view for Search: Search Input+List (Left), Preview (Right)
		// We strictly control heights to prevent the input from being pushed off-screen.
		// The list style height must match the content height we calculated in Update.
		inputView := inputStyle.Width(m.inputWidth).Render(m.searchInput.View())
		listView := listStyle.Width(m.listWidth).Render(m.searchList.View())

		leftPane := lipgloss.JoinVertical(lipgloss.Left,
			inputView,
			listView,
		)
		return lipgloss.JoinHorizontal(
			lipgloss.Top,
			leftPane,
			previewStyle.Width(m.viewWidth).Render(m.viewport.View()),
		)

	case stateTemplateSelect:
		return docStyle.Render(m.templateList.View())

	case stateCreateDir:
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			inputStyle.Render(
				fmt.Sprintf("Create Directory\n\n%s", m.textInput.View()),
			),
		)

	case stateRename:
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			inputStyle.Render(
				fmt.Sprintf("Rename\n\n%s", m.textInput.View()),
			),
		)

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

	case stateConfirmDelete:
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			inputStyle.Render(
				fmt.Sprintf("Delete file?\n\n%s\n\n%s", filepath.Base(m.fileToDelete), m.deleteInput.View()),
			),
		)

	default: // stateBrowser
		if m.showHelp {
			return lipgloss.JoinHorizontal(
				lipgloss.Top,
				listStyle.Width(m.listWidth).Render(m.fileList.View()),
				previewStyle.Width(m.viewWidth).Render(helpStyle.Render(helpContent)),
			)
		}
		return lipgloss.JoinHorizontal(
			lipgloss.Top,
			listStyle.Width(m.listWidth).Render(m.fileList.View()),
			previewStyle.Width(m.viewWidth).Render(m.viewport.View()),
		)
	}
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
