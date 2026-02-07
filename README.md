# TN - Terminal Note Manager

A terminal-based note management application with a TUI (Text User Interface) for browsing, searching, and editing markdown notes. Built with Go and featuring full-text search powered by SQLite FTS5.

## Features

- **File Browser**: Navigate your notes directory with an interactive file tree
- **Full-Text Search**: Fast ranked search across all your notes using SQLite FTS5
- **Live Preview**: See rendered markdown previews as you browse
- **Template Support**: Create new notes from templates with Go template processing
- **Sprig Functions**: Templates support all [Sprig](https://masterminds.github.io/sprig/) functions for dynamic content

## Building

```sh
go build -tags sqlite_fts5
```

The `sqlite_fts5` tag is required to enable full-text search capabilities.

## Installation

1. Clone the repository
2. Build with the command above
3. Run `./tn` or move the binary to your `$PATH`

## Configuration

Configuration is stored in `~/.config/tn/config.yaml`:

```yaml
base_doc: ~/Documents/Obsidian        # Root directory for your notes
templates_dir: ~/.config/tn/templates # Directory for note templates
```

The application creates this config directory automatically on first run with sensible defaults.

### Templates

Template files (`.md` files) can be placed in the `templates/` directory or your configured `templates_dir`. The repository includes example templates:

- **`templates/hugo.md`** - Hugo-compatible frontmatter with TOML format
- **`templates/daily.md`** - Simple daily note with date
- **`templates/default.md`** - Used automatically when creating new notes with `Ctrl+N`

To use these, copy them to your config templates directory:

```sh
cp templates/*.md ~/.config/tn/templates/
```

If `default.md` exists in the templates directory, it will be used automatically when you press `Ctrl+N` to create a new note. Otherwise, a blank note is created.

Templates support Go template syntax with Sprig functions:

```markdown
---
date: {{ now | date "2006-01-02" }}
title: {{ env "USER" | title }}'s Notes
---

# {{ .Title }}

Created on {{ now | date "Monday, January 2, 2006" }}
```

Available functions include:
- Filename: `current_path`, `base` 
- Date/time: `now`, `date`, `date_modify`
- Strings: `upper`, `lower`, `title`, `trim`
- Math: `add`, `sub`, `mul`, `div`
- And many more from [Sprig](https://masterminds.github.io/sprig/)

## Usage

Run `tn` to start the application:

```sh
./tn                    # Normal mode
./tn -debug            # Enable debug logging to debug.log
```

### Keybindings

#### Global
- `Ctrl+C` - Quit application

#### File Browser
- `↑/↓` or `j/k` - Navigate files
- `Enter` or `l` - Open file/directory
- `h` or `Left` - Go to parent directory
- `Ctrl+N` - Create new note
- `Ctrl+T` - Create note from template
- `Ctrl+F` - Open search
- `d` - Delete file
- `s` - Cycle sort (alpha, time, size)
- `r` - Reverse order
- `?` - Show/hide this help
- `q` - Quit

#### Search Mode
- Type to search - Results update in real-time
- `d` - Delete file
- `↑/↓` - Navigate search results
- `Enter` - Jump to selected file
- `Esc` - Return to file browser

#### Creating Notes
1. Press `Ctrl+N` to create a new note (uses `default.md` template if available) or `Ctrl+T` to choose a template
2. Enter filename (`.md` extension is added automatically)
3. Your `$EDITOR` opens with the new file

## Data Storage

- **Database**: `~/.config/tn/tn.db` (SQLite with FTS5)
- **Config**: `~/.config/tn/config.yaml`
- **Templates**: `~/.config/tn/templates/` (copy examples from `templates/`)

The database maintains an index of all markdown files for fast searching. It automatically re-indexes files when they change.

## Dependencies

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) - TUI framework
- [Bubbles](https://github.com/charmbracelet/bubbles) - TUI components
- [Lipgloss](https://github.com/charmbracelet/lipgloss) - Styling
- [Glamour](https://github.com/charmbracelet/glamour) - Markdown rendering
- [go-sqlite3](https://github.com/mattn/go-sqlite3) - SQLite driver
- [Sprig](https://github.com/Masterminds/sprig) - Template functions

## License

MIT
