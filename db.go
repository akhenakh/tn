package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// initDB initializes the SQLite database, creates tables, and enables WAL mode.
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

// calculateHash generates a SHA256 hash of the file content to detect changes.
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

// isIndexable checks if the file extension is supported for text indexing.
func isIndexable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown" || ext == ".txt"
}

// parseMarkdown reads a file, separates headers (#) from content, and checks for binary data.
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

// indexFile is a wrapper for single file indexing (e.g., after editing).
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

// removeFileFromDB cleans up the database when a file is renamed or deleted.
func removeFileFromDB(db *sql.DB, path string) {
	if _, err := db.Exec("DELETE FROM file_tracking WHERE path = ?", path); err != nil {
		log.Printf("Error deleting from file_tracking: %v", err)
	}
	if _, err := db.Exec("DELETE FROM search_idx WHERE path = ?", path); err != nil {
		log.Printf("Error deleting from search_idx: %v", err)
	}
}
