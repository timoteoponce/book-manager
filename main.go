package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	_ "modernc.org/sqlite"
)

//go:embed web/index.html
var webFS embed.FS

var supportedFormats = map[string]bool{
	".azw": true, ".azw3": true, ".cb7": true, ".cbr": true, ".cbz": true,
	".epub": true, ".fb2": true, ".mobi": true, ".pdf": true, ".txt": true,
}

type App struct {
	db                *sql.DB
	bookshelf         string
	syncMu            sync.Mutex
	stateMu           sync.RWMutex
	syncing           bool
	lastSync          SyncResult
	lastSyncErr       string
	lastHeartbeat     time.Time
	metadataRunning   bool
	metadataOperation string
	metadataCurrent   int
	metadataTotal     int
	metadataFailed    int
	metadataError     string
}

type Book struct {
	ID           int64  `json:"id"`
	Path         string `json:"path"`
	Folder       string `json:"folder"`
	Filename     string `json:"filename"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	Format       string `json:"format"`
	Size         int64  `json:"size"`
	ModifiedAt   string `json:"modifiedAt"`
	Status       string `json:"status"`
	Tags         string `json:"tags"`
	Notes        string `json:"notes"`
	LastSyncedAt string `json:"lastSyncedAt"`
	Available    bool   `json:"available"`
	Publisher    string `json:"publisher,omitempty"`
	PublishedAt  string `json:"publishedAt,omitempty"`
	ISBN         string `json:"isbn,omitempty"`
	Language     string `json:"language,omitempty"`
	Description  string `json:"description,omitempty"`
	Series       string `json:"series,omitempty"`
	SeriesIndex  string `json:"seriesIndex,omitempty"`
}

type SyncResult struct {
	Indexed     int `json:"indexed"`
	Renamed     int `json:"renamed"`
	Unavailable int `json:"unavailable"`
}

type CleanupCandidate struct {
	ID          int64  `json:"id"`
	Reason      string `json:"reason"`
	Confidence  string `json:"confidence"`
	Path        string `json:"path"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Format      string `json:"format"`
	Size        int64  `json:"size"`
	ModifiedAt  string `json:"modifiedAt"`
	DuplicateOf string `json:"duplicateOf,omitempty"`
	Available   bool   `json:"available"`
	ISBN        string `json:"isbn,omitempty"`
}

type OrganizationProposal struct {
	ID        int64  `json:"id"`
	Current   string `json:"current"`
	Proposed  string `json:"proposed"`
	Collision bool   `json:"collision"`
}

type organizationMove struct {
	Original string
	New      string
}

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Metadata struct {
		Title       string           `xml:"title"`
		Creator     string           `xml:"creator"`
		Publisher   string           `xml:"publisher"`
		Date        string           `xml:"date"`
		Language    string           `xml:"language"`
		Description string           `xml:"description"`
		Identifiers []epubIdentifier `xml:"identifier"`
		Items       []epubMeta       `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []epubManifestItem `xml:"item"`
	} `xml:"manifest"`
}

type epubManifestItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type epubMeta struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	ID       string `xml:"id,attr"`
	Property string `xml:"property,attr"`
	Refines  string `xml:"refines,attr"`
	Value    string `xml:",chardata"`
}

type extractedMetadata struct {
	Title       string
	Author      string
	Publisher   string
	PublishedAt string
	ISBN        string
	Language    string
	Description string
	Series      string
	SeriesIndex string
}

type OnlineMetadataCandidate struct {
	ID          int64  `json:"id"`
	BookID      int64  `json:"bookId"`
	BookTitle   string `json:"bookTitle"`
	Provider    string `json:"provider"`
	ExternalID  string `json:"externalId"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Publisher   string `json:"publisher"`
	PublishedAt string `json:"publishedAt"`
	ISBN        string `json:"isbn"`
	Language    string `json:"language"`
	Description string `json:"description"`
	Series      string `json:"series"`
	SeriesIndex string `json:"seriesIndex"`
	CoverURL    string `json:"coverUrl"`
	FetchedAt   string `json:"fetchedAt"`
}

type openLibrarySearch struct {
	Docs []struct {
		Key              string   `json:"key"`
		Title            string   `json:"title"`
		AuthorName       []string `json:"author_name"`
		Publisher        []string `json:"publisher"`
		PublishDate      []string `json:"publish_date"`
		FirstPublishYear int      `json:"first_publish_year"`
		ISBN             []string `json:"isbn"`
		Language         []string `json:"language"`
		Series           []string `json:"series"`
		CoverID          int      `json:"cover_i"`
	} `json:"docs"`
}

type epubIdentifier struct {
	Value  string `xml:",chardata"`
	Scheme string `xml:"scheme,attr"`
}

func main() {
	executableDir := mustExecutableDir()
	bookshelf := flag.String("bookshelf", "", "folder to catalogue (defaults to the executable directory when packaged)")
	dbPath := flag.String("db", "", "SQLite catalogue path (defaults beside the executable when packaged)")
	addr := flag.String("addr", "127.0.0.1:0", "HTTP listen address; port 0 selects an available port")
	noBrowser := flag.Bool("no-browser", false, "do not open the application in the default browser")
	flag.Parse()
	if *bookshelf == "" {
		if isDevelopmentExecutable(executableDir) {
			*bookshelf = filepath.Join(mustUserHome(), "bookshelf")
		} else {
			*bookshelf = executableDir
		}
	}
	if *dbPath == "" {
		if isDevelopmentExecutable(executableDir) {
			*dbPath = filepath.Join(mustConfigDir(), "book-manager", "catalog.sqlite")
		} else {
			*dbPath = filepath.Join(executableDir, ".book-manager", "catalog.sqlite")
		}
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := &App{db: db, bookshelf: filepath.Clean(*bookshelf)}
	if err := app.initDB(context.Background()); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.index)
	mux.HandleFunc("/api/books", app.books)
	mux.HandleFunc("/api/book/", app.book)
	mux.HandleFunc("/api/export", app.export)
	mux.HandleFunc("/api/organize/preview", app.organizationPreview)
	mux.HandleFunc("/api/organize/apply", app.organizationApply)
	mux.HandleFunc("/api/organize/undo", app.organizationUndo)
	mux.HandleFunc("/api/sync", app.syncHandler)
	mux.HandleFunc("/api/sync/status", app.syncStatus)
	mux.HandleFunc("/api/heartbeat", app.heartbeat)
	mux.HandleFunc("/api/open/", app.open)
	mux.HandleFunc("/api/preview/", app.preview)
	mux.HandleFunc("/api/cover/", app.cover)
	mux.HandleFunc("/api/cleanup/candidates", app.cleanupCandidatesHandler)
	mux.HandleFunc("/api/cleanup/forget/", app.forgetCleanupRecord)
	mux.HandleFunc("/api/cleanup/delete", app.deleteCleanupCandidates)
	mux.HandleFunc("/api/books/delete", app.deleteBooks)
	mux.HandleFunc("/api/metadata/refresh", app.refreshMetadata)
	mux.HandleFunc("/api/metadata/fetch-online", app.fetchOnlineMetadata)
	mux.HandleFunc("/api/metadata/search", app.searchBookMetadata)
	mux.HandleFunc("/api/metadata/candidates", app.onlineMetadataCandidates)
	mux.HandleFunc("/api/metadata/apply-online", app.applyOnlineMetadata)
	mux.HandleFunc("/api/metadata/status", app.metadataStatus)
	log.Printf("book manager: %s", app.bookshelf)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	applicationURL := "http://" + listener.Addr().String()
	log.Printf("open %s", applicationURL)
	browserOpened := false
	if !*noBrowser {
		if err := openBrowser(applicationURL); err != nil {
			log.Printf("could not open browser: %v", err)
		} else {
			browserOpened = true
		}
	}
	app.stateMu.Lock()
	app.lastHeartbeat = time.Now()
	app.stateMu.Unlock()
	app.setSyncing(true)
	go func() {
		if _, err := app.syncInBackground(context.Background()); err != nil {
			log.Printf("initial sync failed: %v", err)
		}
	}()
	server := &http.Server{Handler: mux}
	if browserOpened {
		go app.monitorBrowser(server)
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (a *App) initDB(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		PRAGMA journal_mode = WAL;
		CREATE TABLE IF NOT EXISTS books (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			folder TEXT NOT NULL,
			filename TEXT NOT NULL,
			title TEXT NOT NULL,
			author TEXT NOT NULL DEFAULT '',
			format TEXT NOT NULL,
			size INTEGER NOT NULL,
			modified_at TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '',
			notes TEXT NOT NULL DEFAULT '',
			last_synced_at TEXT NOT NULL,
			fingerprint TEXT NOT NULL DEFAULT '',
			available INTEGER NOT NULL DEFAULT 1
		);
		CREATE INDEX IF NOT EXISTS books_title_idx ON books(title COLLATE NOCASE);
		CREATE INDEX IF NOT EXISTS books_folder_idx ON books(folder COLLATE NOCASE);
		CREATE TABLE IF NOT EXISTS organization_moves (
			run_id TEXT NOT NULL,
			original_path TEXT NOT NULL,
			new_path TEXT NOT NULL,
			created_at TEXT NOT NULL,
			undone_at TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS organization_moves_run_idx ON organization_moves(run_id, created_at);
		CREATE TABLE IF NOT EXISTS book_metadata (
			book_id INTEGER PRIMARY KEY,
			publisher TEXT NOT NULL DEFAULT '',
			published_at TEXT NOT NULL DEFAULT '',
			isbn TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			series TEXT NOT NULL DEFAULT '',
			series_index TEXT NOT NULL DEFAULT '',
			refreshed_at TEXT NOT NULL,
			FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS metadata_candidates (
			id INTEGER PRIMARY KEY,
			book_id INTEGER NOT NULL,
			provider TEXT NOT NULL,
			external_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '',
			publisher TEXT NOT NULL DEFAULT '',
			published_at TEXT NOT NULL DEFAULT '',
			isbn TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			series TEXT NOT NULL DEFAULT '',
			series_index TEXT NOT NULL DEFAULT '',
			cover_url TEXT NOT NULL DEFAULT '',
			fetched_at TEXT NOT NULL,
			UNIQUE(book_id, provider, external_id),
			FOREIGN KEY(book_id) REFERENCES books(id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS metadata_candidates_book_idx ON metadata_candidates(book_id);
	`); err != nil {
		return err
	}
	for name, definition := range map[string]string{
		"series":       "TEXT NOT NULL DEFAULT ''",
		"series_index": "TEXT NOT NULL DEFAULT ''",
	} {
		var found int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('book_metadata') WHERE name = ?`, name).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := a.db.ExecContext(ctx, `ALTER TABLE book_metadata ADD COLUMN `+name+` `+definition); err != nil {
				return err
			}
		}
	}
	for name, definition := range map[string]string{
		"series":       "TEXT NOT NULL DEFAULT ''",
		"series_index": "TEXT NOT NULL DEFAULT ''",
	} {
		var found int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('metadata_candidates') WHERE name = ?`, name).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := a.db.ExecContext(ctx, `ALTER TABLE metadata_candidates ADD COLUMN `+name+` `+definition); err != nil {
				return err
			}
		}
	}
	// These columns make upgrades safe for catalogues created by earlier builds.
	for name, definition := range map[string]string{
		"fingerprint": "TEXT NOT NULL DEFAULT ''",
		"available":   "INTEGER NOT NULL DEFAULT 1",
	} {
		var found int
		err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('books') WHERE name = ?`, name).Scan(&found)
		if err != nil {
			return err
		}
		if found == 0 {
			if _, err := a.db.ExecContext(ctx, `ALTER TABLE books ADD COLUMN `+name+` `+definition); err != nil {
				return err
			}
		}
	}
	_, err := a.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS books_fingerprint_idx ON books(fingerprint)`)
	return err
}

func (a *App) sync(ctx context.Context) (SyncResult, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.setSyncing(true)
	result, err := a.syncUnlocked(ctx)
	a.finishSync(result, err)
	return result, err
}

func (a *App) syncInBackground(ctx context.Context) (SyncResult, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	result, err := a.syncUnlocked(ctx)
	a.finishSync(result, err)
	return result, err
}

func (a *App) syncUnlocked(ctx context.Context) (SyncResult, error) {
	type foundBook struct {
		path, folder, filename, title, author, format, modified, fingerprint string
		size                                                                 int64
	}
	var found []foundBook
	err := filepath.WalkDir(a.bookshelf, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !supportedFormats[ext] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(a.bookshelf, path)
		if err != nil {
			return err
		}
		modified := info.ModTime().UTC().Format(time.RFC3339Nano)
		var cachedTitle, cachedAuthor, cachedFingerprint, cachedModified string
		var cachedSize int64
		cacheErr := a.db.QueryRowContext(ctx, `SELECT title, author, fingerprint, size, modified_at FROM books WHERE path = ?`, filepath.ToSlash(rel)).Scan(&cachedTitle, &cachedAuthor, &cachedFingerprint, &cachedSize, &cachedModified)
		title, author := displayMetadata(entry.Name())
		fingerprint := ""
		if cacheErr == nil && cachedFingerprint != "" && cachedSize == info.Size() && cachedModified == modified && metadataCacheUsable(cachedTitle, cachedAuthor) {
			title, author, fingerprint = cachedTitle, cachedAuthor, cachedFingerprint
		} else {
			fingerprint, err = fileFingerprint(path)
			if err != nil {
				return err
			}
			if ext == ".epub" {
				if epubTitle, epubAuthor := epubMetadata(path); epubTitle != "" {
					title, author = epubTitle, epubAuthor
				}
			} else if ext == ".pdf" {
				if pdfTitle, pdfAuthor := pdfMetadata(path); pdfTitle != "" || pdfAuthor != "" {
					if pdfTitle != "" {
						title = pdfTitle
					}
					if pdfAuthor != "" {
						author = pdfAuthor
					}
				}
			}
			if err := consolidateMetadata(path, strings.TrimPrefix(ext, "."), extractedMetadata{Title: title, Author: author}); err != nil {
				log.Printf("metadata consolidation skipped for %s: %v", path, err)
			}
		}
		found = append(found, foundBook{
			path: filepath.ToSlash(rel), folder: filepath.ToSlash(filepath.Dir(rel)),
			filename: entry.Name(), title: title, author: author, format: strings.TrimPrefix(ext, "."),
			size: info.Size(), modified: modified, fingerprint: fingerprint,
		})
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}
	sort.Slice(found, func(i, j int) bool { return strings.ToLower(found[i].title) < strings.ToLower(found[j].title) })

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return SyncResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE books SET available = 0`); err != nil {
		_ = tx.Rollback()
		return SyncResult{}, err
	}
	result := SyncResult{Indexed: len(found)}
	for _, book := range found {
		var id int64
		pathErr := tx.QueryRowContext(ctx, `SELECT id FROM books WHERE path = ?`, book.path).Scan(&id)
		if errors.Is(pathErr, sql.ErrNoRows) {
			pathErr = tx.QueryRowContext(ctx, `SELECT id FROM books WHERE fingerprint = ? AND available = 0 LIMIT 1`, book.fingerprint).Scan(&id)
			if pathErr == nil {
				_, err = tx.ExecContext(ctx, `UPDATE books SET path=?, folder=?, filename=?, title=?, author=?, format=?, size=?, modified_at=?, last_synced_at=?, fingerprint=?, available=1 WHERE id=?`, book.path, book.folder, book.filename, book.title, book.author, book.format, book.size, book.modified, now, book.fingerprint, id)
				result.Renamed++
			} else if errors.Is(pathErr, sql.ErrNoRows) {
				_, err = tx.ExecContext(ctx, `INSERT INTO books (path, folder, filename, title, author, format, size, modified_at, last_synced_at, fingerprint, available) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`, book.path, book.folder, book.filename, book.title, book.author, book.format, book.size, book.modified, now, book.fingerprint)
			} else {
				err = pathErr
			}
		} else if pathErr == nil {
			_, err = tx.ExecContext(ctx, `UPDATE books SET folder=?, filename=?, title=?, author=?, format=?, size=?, modified_at=?, last_synced_at=?, fingerprint=?, available=1 WHERE id=?`, book.folder, book.filename, book.title, book.author, book.format, book.size, book.modified, now, book.fingerprint, id)
		} else {
			err = pathErr
		}
		if err != nil {
			_ = tx.Rollback()
			return SyncResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return SyncResult{}, err
	}
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books WHERE available = 0`).Scan(&result.Unavailable)
	return result, nil
}

func (a *App) setSyncing(running bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.syncing = running
	if running {
		a.lastSyncErr = ""
	}
}

func (a *App) finishSync(result SyncResult, err error) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.syncing = false
	a.lastSync = result
	a.lastSyncErr = ""
	if err != nil {
		a.lastSyncErr = err.Error()
	}
}

func (a *App) syncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stateMu.RLock()
	response := map[string]any{
		"syncing": a.syncing,
		"result":  a.lastSync,
		"error":   a.lastSyncErr,
	}
	a.stateMu.RUnlock()
	writeJSON(w, response)
}

func (a *App) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stateMu.Lock()
	a.lastHeartbeat = time.Now()
	a.stateMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) startMetadataJob(operation string) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.metadataRunning || a.syncing {
		return false
	}
	a.metadataRunning = true
	a.metadataOperation = operation
	a.metadataCurrent = 0
	a.metadataTotal = 0
	a.metadataFailed = 0
	a.metadataError = ""
	return true
}

func (a *App) setMetadataTotal(total int) {
	a.stateMu.Lock()
	a.metadataTotal = total
	a.stateMu.Unlock()
}

func (a *App) setMetadataProgress(current, failed int) {
	a.stateMu.Lock()
	a.metadataCurrent = current
	a.metadataFailed = failed
	a.stateMu.Unlock()
}

func (a *App) finishMetadataJob(current, failed int, err error) {
	a.stateMu.Lock()
	a.metadataRunning = false
	a.metadataCurrent = current
	a.metadataFailed = failed
	a.metadataError = ""
	if err != nil {
		a.metadataError = err.Error()
	}
	a.stateMu.Unlock()
}

func (a *App) metadataStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stateMu.RLock()
	response := map[string]any{
		"running":   a.metadataRunning,
		"operation": a.metadataOperation,
		"current":   a.metadataCurrent,
		"total":     a.metadataTotal,
		"failed":    a.metadataFailed,
		"error":     a.metadataError,
	}
	a.stateMu.RUnlock()
	writeJSON(w, response)
}

func (a *App) monitorBrowser(server *http.Server) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		a.stateMu.RLock()
		lastHeartbeat := a.lastHeartbeat
		a.stateMu.RUnlock()
		if now.Sub(lastHeartbeat) < 20*time.Second {
			continue
		}
		log.Printf("browser heartbeat stopped; shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = server.Shutdown(ctx)
		cancel()
		return
	}
}

func (a *App) cleanupCandidatesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	candidates, err := a.cleanupCandidates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"candidates": candidates, "count": len(candidates)})
}

func (a *App) cleanupCandidates(ctx context.Context) ([]CleanupCandidate, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT b.id, b.path, b.title, b.author, b.format, b.size, b.modified_at, b.fingerprint, COALESCE(m.isbn, '') FROM books b LEFT JOIN book_metadata m ON m.book_id = b.id WHERE b.available = 1 AND b.fingerprint != '' ORDER BY b.fingerprint, b.modified_at, b.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type duplicateRecord struct {
		candidate   CleanupCandidate
		fingerprint string
	}
	duplicates := map[string][]duplicateRecord{}
	allRecords := []duplicateRecord{}
	for rows.Next() {
		var record duplicateRecord
		if err := rows.Scan(&record.candidate.ID, &record.candidate.Path, &record.candidate.Title, &record.candidate.Author, &record.candidate.Format, &record.candidate.Size, &record.candidate.ModifiedAt, &record.fingerprint, &record.candidate.ISBN); err != nil {
			return nil, err
		}
		record.candidate.Reason = "exact_duplicate"
		record.candidate.Confidence = "high"
		record.candidate.Available = true
		duplicates[record.fingerprint] = append(duplicates[record.fingerprint], record)
		allRecords = append(allRecords, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	candidates := []CleanupCandidate{}
	flagged := map[int64]bool{}
	for _, records := range duplicates {
		if len(records) < 2 {
			continue
		}
		keeper := records[0].candidate.Path
		for _, record := range records[1:] {
			record.candidate.DuplicateOf = keeper
			candidates = append(candidates, record.candidate)
			flagged[record.candidate.ID] = true
		}
	}
	byISBN := map[string][]duplicateRecord{}
	for _, record := range allRecords {
		if record.candidate.ISBN != "" {
			byISBN[record.candidate.ISBN] = append(byISBN[record.candidate.ISBN], record)
		}
	}
	for _, records := range byISBN {
		if len(records) < 2 {
			continue
		}
		keeper := records[0].candidate.Path
		for _, record := range records[1:] {
			if flagged[record.candidate.ID] {
				continue
			}
			record.candidate.Reason = "same_isbn"
			record.candidate.Confidence = "high"
			record.candidate.DuplicateOf = keeper
			candidates = append(candidates, record.candidate)
			flagged[record.candidate.ID] = true
		}
	}
	byTitleAuthor := map[string][]duplicateRecord{}
	for _, record := range allRecords {
		key := cleanupKey(record.candidate.Title, record.candidate.Author)
		if key != "" {
			byTitleAuthor[key] = append(byTitleAuthor[key], record)
		}
	}
	for _, records := range byTitleAuthor {
		if len(records) < 2 {
			continue
		}
		keeper := records[0].candidate.Path
		for _, record := range records[1:] {
			if flagged[record.candidate.ID] {
				continue
			}
			record.candidate.Reason = "possible_duplicate"
			record.candidate.Confidence = "medium"
			record.candidate.DuplicateOf = keeper
			candidates = append(candidates, record.candidate)
		}
	}
	missingRows, err := a.db.QueryContext(ctx, `SELECT id, path, title, author, format, size, modified_at FROM books WHERE available = 0 ORDER BY title COLLATE NOCASE, path`)
	if err != nil {
		return nil, err
	}
	defer missingRows.Close()
	for missingRows.Next() {
		var candidate CleanupCandidate
		if err := missingRows.Scan(&candidate.ID, &candidate.Path, &candidate.Title, &candidate.Author, &candidate.Format, &candidate.Size, &candidate.ModifiedAt); err != nil {
			return nil, err
		}
		candidate.Reason = "unavailable_record"
		candidate.Confidence = "high"
		candidates = append(candidates, candidate)
	}
	return candidates, missingRows.Err()
}

func cleanupKey(title, author string) string {
	title = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(title))), " ")
	author = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(author))), " ")
	if title == "" {
		return ""
	}
	return title + "\x00" + author
}

func (a *App) forgetCleanupRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := bookID(r.URL.Path, "/api/cleanup/forget/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM books WHERE id = ? AND available = 0`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		http.Error(w, "only unavailable catalogue records can be forgotten", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"forgotten": true})
}

func (a *App) deleteCleanupCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&request); err != nil || len(request.IDs) == 0 {
		http.Error(w, "select at least one cleanup candidate", http.StatusBadRequest)
		return
	}
	candidates, err := a.cleanupCandidates(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byID := make(map[int64]CleanupCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	moves := []organizationMove{}
	deleteIDs := []int64{}
	seen := map[int64]bool{}
	for _, id := range request.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		candidate, ok := byID[id]
		if !ok {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, "cleanup candidate is stale; refresh the list", http.StatusConflict)
			return
		}
		if candidate.Available {
			source := filepath.Join(a.bookshelf, filepath.FromSlash(candidate.Path))
			if _, err := os.Stat(source); err != nil {
				a.rollbackOrganizationMoves(moves)
				http.Error(w, "candidate file is no longer available; sync first", http.StatusConflict)
				return
			}
			trashRelative := filepath.ToSlash(filepath.Join(".book-manager", "trash", runID, filepath.FromSlash(candidate.Path)))
			trashPath := filepath.Join(a.bookshelf, filepath.FromSlash(trashRelative))
			if _, err := os.Lstat(trashPath); err == nil {
				a.rollbackOrganizationMoves(moves)
				http.Error(w, "trash destination already exists; try again", http.StatusConflict)
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				a.rollbackOrganizationMoves(moves)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.MkdirAll(filepath.Dir(trashPath), 0o755); err != nil {
				a.rollbackOrganizationMoves(moves)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(source, trashPath); err != nil {
				a.rollbackOrganizationMoves(moves)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			moves = append(moves, organizationMove{Original: candidate.Path, New: trashRelative})
		}
		deleteIDs = append(deleteIDs, id)
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err == nil {
		for _, id := range deleteIDs {
			if _, err = tx.ExecContext(r.Context(), `DELETE FROM books WHERE id = ?`, id); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		a.rollbackOrganizationMoves(moves)
		http.Error(w, "cleanup rolled back: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": len(moves), "forgotten": len(deleteIDs) - len(moves), "trashRun": runID})
}

func (a *App) deleteBooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&request); err != nil || len(request.IDs) == 0 {
		http.Error(w, "select at least one book", http.StatusBadRequest)
		return
	}
	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	moves := []organizationMove{}
	seen := map[int64]bool{}
	for _, id := range request.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		var relative string
		var available int
		if err := a.db.QueryRowContext(r.Context(), `SELECT path, available FROM books WHERE id = ?`, id).Scan(&relative, &available); err != nil || available != 1 {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, "book selection is stale; refresh the catalogue", http.StatusConflict)
			return
		}
		source := filepath.Join(a.bookshelf, filepath.FromSlash(relative))
		trashRelative := filepath.ToSlash(filepath.Join(".book-manager", "trash", runID, filepath.FromSlash(relative)))
		trashPath := filepath.Join(a.bookshelf, filepath.FromSlash(trashRelative))
		if _, err := os.Stat(source); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, "book file is unavailable; sync first", http.StatusConflict)
			return
		}
		if err := os.MkdirAll(filepath.Dir(trashPath), 0o755); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(source, trashPath); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		moves = append(moves, organizationMove{Original: relative, New: trashRelative})
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err == nil {
		for id := range seen {
			if _, err = tx.ExecContext(r.Context(), `DELETE FROM books WHERE id = ?`, id); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		a.rollbackOrganizationMoves(moves)
		http.Error(w, "delete rolled back: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": len(moves), "trashRun": runID})
}

func (a *App) books(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	query := `SELECT b.id, b.path, b.folder, b.filename, b.title, b.author, b.format, b.size, b.modified_at, b.status, b.tags, b.notes, b.last_synced_at, b.available, COALESCE(m.series, ''), COALESCE(m.series_index, '') FROM books b LEFT JOIN book_metadata m ON m.book_id = b.id`
	args := []any{}
	conditions := []string{}
	if q != "" {
		conditions = append(conditions, `(b.title LIKE ? OR b.author LIKE ? OR b.folder LIKE ? OR b.filename LIKE ? OR b.tags LIKE ? OR m.series LIKE ? OR m.series_index LIKE ?)`)
		term := "%" + q + "%"
		args = append(args, term, term, term, term, term, term, term)
	}
	if format != "" {
		conditions = append(conditions, `b.format = ?`)
		args = append(args, format)
	}
	if status != "" {
		if status == "__none__" {
			conditions = append(conditions, `b.status = ''`)
		} else {
			conditions = append(conditions, `b.status = ?`)
			args = append(args, status)
		}
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY b.title COLLATE NOCASE, b.author COLLATE NOCASE"
	rows, err := a.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	result := []Book{}
	for rows.Next() {
		var book Book
		var available int
		if err := rows.Scan(&book.ID, &book.Path, &book.Folder, &book.Filename, &book.Title, &book.Author, &book.Format, &book.Size, &book.ModifiedAt, &book.Status, &book.Tags, &book.Notes, &book.LastSyncedAt, &available, &book.Series, &book.SeriesIndex); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		book.Available = available == 1
		result = append(result, book)
	}
	writeJSON(w, map[string]any{"books": result, "count": len(result)})
}

func (a *App) syncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.stateMu.Lock()
	if a.syncing {
		a.stateMu.Unlock()
		http.Error(w, "sync already in progress", http.StatusConflict)
		return
	}
	a.syncing = true
	a.lastSyncErr = ""
	a.stateMu.Unlock()
	go func() {
		if _, err := a.syncInBackground(context.Background()); err != nil {
			log.Printf("sync failed: %v", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (a *App) book(w http.ResponseWriter, r *http.Request) {
	id, err := bookID(r.URL.Path, "/api/book/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodPatch {
		a.updateBook(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var book Book
	var available int
	err = a.db.QueryRowContext(r.Context(), `SELECT id, path, folder, filename, title, author, format, size, modified_at, status, tags, notes, last_synced_at, available FROM books WHERE id = ?`, id).Scan(
		&book.ID, &book.Path, &book.Folder, &book.Filename, &book.Title, &book.Author, &book.Format, &book.Size, &book.ModifiedAt, &book.Status, &book.Tags, &book.Notes, &book.LastSyncedAt, &available)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	book.Available = available == 1
	if err := a.loadBookMetadata(r.Context(), &book); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, book)
}

func (a *App) loadBookMetadata(ctx context.Context, book *Book) error {
	err := a.db.QueryRowContext(ctx, `SELECT publisher, published_at, isbn, language, description, series, series_index FROM book_metadata WHERE book_id = ?`, book.ID).Scan(&book.Publisher, &book.PublishedAt, &book.ISBN, &book.Language, &book.Description, &book.Series, &book.SeriesIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (a *App) updateBook(w http.ResponseWriter, r *http.Request, id int64) {
	var input struct {
		Status string `json:"status"`
		Tags   string `json:"tags"`
		Notes  string `json:"notes"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid metadata JSON", http.StatusBadRequest)
		return
	}
	input.Status = strings.TrimSpace(input.Status)
	if input.Status != "" && input.Status != "unread" && input.Status != "reading" && input.Status != "finished" && input.Status != "paused" {
		http.Error(w, "invalid reading status", http.StatusBadRequest)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE books SET status = ?, tags = ?, notes = ? WHERE id = ?`, input.Status, strings.TrimSpace(input.Tags), strings.TrimSpace(input.Notes), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := consolidateBookMetadata(r.Context(), id, a.db, a.bookshelf); err != nil {
		http.Error(w, "metadata saved locally but could not be copied into the book file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "saved"})
}

func (a *App) export(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, path, folder, filename, title, author, format, size, modified_at, status, tags, notes, last_synced_at, available FROM books ORDER BY title COLLATE NOCASE`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	books := []Book{}
	for rows.Next() {
		var book Book
		var available int
		if err := rows.Scan(&book.ID, &book.Path, &book.Folder, &book.Filename, &book.Title, &book.Author, &book.Format, &book.Size, &book.ModifiedAt, &book.Status, &book.Tags, &book.Notes, &book.LastSyncedAt, &available); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		book.Available = available == 1
		books = append(books, book)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="bookshelf-catalogue.json"`)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":    1,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"bookshelf":  a.bookshelf,
		"books":      books,
	})
}

func (a *App) organizationPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proposals, err := a.organizationProposals(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"proposals": proposals, "count": len(proposals), "mutated": false})
}

func (a *App) organizationProposals(ctx context.Context) ([]OrganizationProposal, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, path, folder, filename, title, author, format, available FROM books WHERE available = 1 ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	proposals := []OrganizationProposal{}
	used := map[string]bool{}
	for rows.Next() {
		var book Book
		var available int
		if err := rows.Scan(&book.ID, &book.Path, &book.Folder, &book.Filename, &book.Title, &book.Author, &book.Format, &available); err != nil {
			return nil, err
		}
		book.Available = available == 1
		target := organizationTarget(book)
		key := strings.ToLower(filepath.ToSlash(target))
		collision := used[key]
		if !collision {
			if _, err := os.Stat(filepath.Join(a.bookshelf, filepath.FromSlash(target))); err == nil && filepath.ToSlash(book.Path) != target {
				collision = true
			}
		}
		used[key] = true
		proposals = append(proposals, OrganizationProposal{ID: book.ID, Current: book.Path, Proposed: target, Collision: collision})
	}
	return proposals, rows.Err()
}

func (a *App) organizationApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proposals, err := a.organizationProposals(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, proposal := range proposals {
		if proposal.Collision {
			http.Error(w, "organization stopped: resolve all collisions in the preview first", http.StatusConflict)
			return
		}
	}
	moves := []organizationMove{}
	for _, proposal := range proposals {
		if filepath.ToSlash(proposal.Current) == proposal.Proposed {
			continue
		}
		original := filepath.Join(a.bookshelf, filepath.FromSlash(proposal.Current))
		newPath := filepath.Join(a.bookshelf, filepath.FromSlash(proposal.Proposed))
		if _, err := os.Stat(original); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, "organization stopped: a source file is no longer available; sync first", http.StatusConflict)
			return
		}
		if _, err := os.Lstat(newPath); err == nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, "organization stopped: a target appeared after preview", http.StatusConflict)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(original, newPath); err != nil {
			a.rollbackOrganizationMoves(moves)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		moves = append(moves, organizationMove{Original: proposal.Current, New: proposal.Proposed})
	}
	if len(moves) == 0 {
		writeJSON(w, map[string]any{"moved": 0, "message": "All books are already organized"})
		return
	}
	if _, err := a.sync(r.Context()); err != nil {
		a.rollbackOrganizationMoves(moves)
		http.Error(w, "organization rolled back because sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err == nil {
		for _, move := range moves {
			if _, err = tx.ExecContext(r.Context(), `INSERT INTO organization_moves (run_id, original_path, new_path, created_at) VALUES (?, ?, ?, ?)`, runID, move.Original, move.New, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				break
			}
		}
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		a.rollbackOrganizationMoves(moves)
		_, _ = a.sync(r.Context())
		http.Error(w, "organization rolled back because its undo record could not be saved: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		a.rollbackOrganizationMoves(moves)
		_, _ = a.sync(r.Context())
		http.Error(w, "organization rolled back because its undo record could not be committed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"moved": len(moves), "runID": runID})
}

func (a *App) organizationUndo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var runID string
	if err := a.db.QueryRowContext(r.Context(), `SELECT run_id FROM organization_moves WHERE undone_at = '' ORDER BY created_at DESC LIMIT 1`).Scan(&runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no organization run to undo", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT original_path, new_path FROM organization_moves WHERE run_id = ? ORDER BY rowid DESC`, runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	moves := []organizationMove{}
	for rows.Next() {
		var move organizationMove
		if err := rows.Scan(&move.Original, &move.New); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		moves = append(moves, move)
	}
	for _, move := range moves {
		original := filepath.Join(a.bookshelf, filepath.FromSlash(move.Original))
		newPath := filepath.Join(a.bookshelf, filepath.FromSlash(move.New))
		if _, err := os.Stat(newPath); err != nil {
			http.Error(w, "undo stopped: an organized file is missing", http.StatusConflict)
			return
		}
		if _, err := os.Lstat(original); err == nil {
			http.Error(w, "undo stopped: an original path is occupied", http.StatusConflict)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	undone := []organizationMove{}
	for _, move := range moves {
		original := filepath.Join(a.bookshelf, filepath.FromSlash(move.Original))
		newPath := filepath.Join(a.bookshelf, filepath.FromSlash(move.New))
		if err := os.MkdirAll(filepath.Dir(original), 0o755); err != nil {
			a.rollbackOrganizationMoves(undone)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(newPath, original); err != nil {
			a.rollbackOrganizationMoves(undone)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		undone = append(undone, organizationMove{Original: move.New, New: move.Original})
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE organization_moves SET undone_at = ? WHERE run_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
		for _, move := range undone {
			_ = os.Rename(filepath.Join(a.bookshelf, filepath.FromSlash(move.New)), filepath.Join(a.bookshelf, filepath.FromSlash(move.Original)))
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := a.sync(r.Context()); err != nil {
		http.Error(w, "undo completed but sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"undone": len(moves), "runID": runID})
}

func (a *App) rollbackOrganizationMoves(moves []organizationMove) {
	for index := len(moves) - 1; index >= 0; index-- {
		move := moves[index]
		_ = os.Rename(filepath.Join(a.bookshelf, filepath.FromSlash(move.New)), filepath.Join(a.bookshelf, filepath.FromSlash(move.Original)))
	}
}

func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := bookID(r.URL.Path, "/api/preview/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var relative string
	if err := a.db.QueryRowContext(r.Context(), `SELECT path FROM books WHERE id = ? AND available = 1`, id).Scan(&relative); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	path := filepath.Join(a.bookshelf, filepath.FromSlash(relative))
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}

func (a *App) cover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := bookID(r.URL.Path, "/api/cover/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var relative, format string
	if err := a.db.QueryRowContext(r.Context(), `SELECT path, format FROM books WHERE id = ? AND available = 1`, id).Scan(&relative, &format); err != nil {
		http.NotFound(w, r)
		return
	}
	if format != "epub" {
		http.NotFound(w, r)
		return
	}
	cover, mediaType, err := epubCover(filepath.Join(a.bookshelf, filepath.FromSlash(relative)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer cover.Close()
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if _, err := io.Copy(w, cover); err != nil {
		log.Printf("cover response failed: %v", err)
	}
}

func (a *App) open(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := bookID(r.URL.Path, "/api/open/")
	if err != nil {
		http.Error(w, "invalid book id", http.StatusBadRequest)
		return
	}
	var relative string
	if err := a.db.QueryRowContext(r.Context(), `SELECT path FROM books WHERE id = ?`, id).Scan(&relative); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	path := filepath.Join(a.bookshelf, filepath.FromSlash(relative))
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "book is no longer at its indexed path; sync first", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if err := openWithOS(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "opened"})
}

func bookID(path, prefix string) (int64, error) {
	value := strings.TrimPrefix(path, prefix)
	var id int64
	if value == "" || strings.ContainsAny(value, "/?&") {
		return 0, errors.New("invalid book id")
	}
	if _, err := fmt.Sscanf(value, "%d", &id); err != nil || id < 1 {
		return 0, errors.New("invalid book id")
	}
	return id, nil
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, webFS, "web/index.html")
}

func fileFingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func displayMetadata(filename string) (string, string) {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	parts := strings.SplitN(base, " - ", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1]), strings.TrimSpace(parts[0])
	}
	return base, ""
}

func metadataCacheUsable(title, author string) bool {
	// Older versions stored PDF escape sequences as literal text. Reparse those
	// records once so the corrected decoder can repair the catalogue in place.
	return !strings.Contains(title, "\\") && !strings.Contains(author, "\\")
}

func organizationTarget(book Book) string {
	author := book.Author
	if author == "" {
		author = "Unknown Author"
	}
	title := book.Title
	if title == "" {
		title = strings.TrimSuffix(book.Filename, filepath.Ext(book.Filename))
	}
	ext := strings.ToLower(filepath.Ext(book.Filename))
	if ext == "" && book.Format != "" {
		ext = "." + strings.ToLower(book.Format)
	}
	cleanAuthor := sanitizePathPart(author)
	cleanTitle := sanitizePathPart(title)
	return filepath.ToSlash(filepath.Join(cleanAuthor, cleanTitle, cleanTitle+ext))
}

func sanitizePathPart(value string) string {
	var builder strings.Builder
	for _, character := range strings.TrimSpace(value) {
		if character < 32 || strings.ContainsRune(`<>:"/\\|?*`, character) {
			builder.WriteRune('_')
		} else {
			builder.WriteRune(character)
		}
	}
	value = strings.Trim(builder.String(), " .")
	if value == "" {
		return "Untitled"
	}
	for _, reserved := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"} {
		if strings.EqualFold(value, reserved) {
			return "_" + value
		}
	}
	return value
}

var pdfInfoString = regexp.MustCompile(`/((?:Title)|(?:Author))\s*\(([^()]*)\)`)

func pdfMetadata(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var title, author string
	for _, match := range pdfInfoString.FindAllStringSubmatch(string(data), -1) {
		value := decodePDFString(match[2])
		switch match[1] {
		case "Title":
			title = value
		case "Author":
			author = value
		}
	}
	return title, author
}

func decodePDFString(value string) string {
	decoded := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			decoded = append(decoded, value[index])
			continue
		}
		index++
		switch value[index] {
		case 'n':
			decoded = append(decoded, '\n')
		case 'r':
			decoded = append(decoded, '\r')
		case 't':
			decoded = append(decoded, '\t')
		case 'b':
			decoded = append(decoded, '\b')
		case 'f':
			decoded = append(decoded, '\f')
		case '(', ')', '\\':
			decoded = append(decoded, value[index])
		default:
			if value[index] < '0' || value[index] > '7' {
				decoded = append(decoded, value[index])
				continue
			}
			octal := int(value[index] - '0')
			for digits := 1; digits < 3 && index+1 < len(value) && value[index+1] >= '0' && value[index+1] <= '7'; digits++ {
				index++
				octal = octal*8 + int(value[index]-'0')
			}
			decoded = append(decoded, byte(octal))
		}
	}
	if bytes.HasPrefix(decoded, []byte{0xfe, 0xff}) {
		return strings.TrimSpace(decodeUTF16(decoded[2:], binary.BigEndian))
	}
	if bytes.HasPrefix(decoded, []byte{0xff, 0xfe}) {
		return strings.TrimSpace(decodeUTF16(decoded[2:], binary.LittleEndian))
	}
	return strings.TrimSpace(string(decoded))
}

func decodeUTF16(data []byte, order binary.ByteOrder) string {
	units := make([]uint16, 0, len(data)/2)
	for index := 0; index+1 < len(data); index += 2 {
		units = append(units, order.Uint16(data[index:index+2]))
	}
	return string(utf16.Decode(units))
}

func epubMetadata(path string) (string, string) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", ""
	}
	defer z.Close()
	pkg, _, err := epubPackage(z)
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(pkg.Metadata.Title), strings.TrimSpace(pkg.Metadata.Creator)
}

func extractMetadata(path, format string) (extractedMetadata, error) {
	switch format {
	case "epub":
		z, err := zip.OpenReader(path)
		if err != nil {
			return extractedMetadata{}, err
		}
		defer z.Close()
		pkg, _, err := epubPackage(z)
		if err != nil {
			return extractedMetadata{}, err
		}
		metadata := extractedMetadata{
			Title: pkg.Metadata.Title, Author: pkg.Metadata.Creator, Publisher: pkg.Metadata.Publisher,
			PublishedAt: pkg.Metadata.Date, Language: pkg.Metadata.Language, Description: pkg.Metadata.Description,
		}
		metadata.Series, metadata.SeriesIndex = epubSeriesMetadata(pkg)
		if metadata.Series == "" {
			metadata.Series, metadata.SeriesIndex = seriesFromFilename(path)
		}
		for _, identifier := range pkg.Metadata.Identifiers {
			if isbn := normalizeISBN(identifier.Value); isbn != "" {
				metadata.ISBN = isbn
				break
			}
		}
		return metadata, nil
	case "pdf":
		title, author := pdfMetadata(path)
		return extractedMetadata{Title: title, Author: author}, nil
	default:
		return extractedMetadata{}, nil
	}
}

func seriesFromFilename(path string) (string, string) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	parent := filepath.Base(filepath.Dir(path))
	index := ""
	name := ""
	if match := volumePattern.FindStringSubmatch(base); len(match) == 3 {
		name = strings.TrimSpace(match[1])
		index = match[2]
	}
	candidate := name
	if candidate == "" {
		candidate = base
	}
	if _, after, found := strings.Cut(candidate, " - "); found && strings.TrimSpace(after) != "" {
		candidate = after
	}
	if len(parent) > 3 && folderMatchesFilename(parent, candidate) {
		return parent, index
	}
	if index != "" {
		return name, index
	}
	return "", ""
}

var volumePattern = regexp.MustCompile(`(?i)^(.*?)(?:,?\s*vol(?:ume)?\.?\s*|\s+v)(\d+)$`)

func folderMatchesFilename(folder, filename string) bool {
	left, right := normalizeName(folder), normalizeName(filename)
	if left == "" || right == "" {
		return false
	}
	return strings.HasPrefix(right, left) || strings.HasPrefix(left, right) ||
		strings.HasPrefix(right+"s", left) || strings.HasPrefix(left+"s", right)
}

func meaningfulTitle(title string) bool {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return false
	}
	alphanumeric := 0
	for _, character := range trimmed {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			alphanumeric++
		}
	}
	// Comic archives frequently carry placeholder titles such as "0 (2)".
	return alphanumeric >= 4
}

func normalizeName(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func epubSeriesMetadata(pkg *opfPackage) (string, string) {
	series, index := "", ""
	collections := map[string]string{}
	seriesCollections := map[string]bool{}
	for _, item := range pkg.Metadata.Items {
		name := strings.ToLower(item.Name)
		if name == "calibre:series" {
			series = strings.TrimSpace(item.Content)
		} else if name == "calibre:series_index" {
			index = strings.TrimSpace(item.Content)
		}
		property := strings.ToLower(item.Property)
		if property == "belongs-to-collection" {
			collections[item.ID] = strings.TrimSpace(item.Value)
		} else if property == "collection-type" && strings.EqualFold(strings.TrimSpace(item.Value), "series") {
			seriesCollections[strings.TrimPrefix(item.Refines, "#")] = true
		} else if property == "group-position" {
			if series == "" {
				index = strings.TrimSpace(item.Value)
			}
		}
	}
	if series == "" {
		for id, value := range collections {
			if seriesCollections[id] || len(seriesCollections) == 0 {
				series = value
				break
			}
		}
	}
	return series, index
}

func normalizeISBN(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToUpper(value) {
		if character >= '0' && character <= '9' || character == 'X' {
			builder.WriteRune(character)
		}
	}
	isbn := builder.String()
	if len(isbn) == 10 || len(isbn) == 13 {
		return isbn
	}
	return ""
}

func consolidateMetadata(path, format string, metadata extractedMetadata) error {
	if metadata.Title == "" && metadata.Author == "" && metadata.Publisher == "" && metadata.PublishedAt == "" && metadata.ISBN == "" && metadata.Language == "" && metadata.Description == "" {
		return nil
	}
	switch format {
	case "epub":
		return consolidateEPUB(path, metadata)
	case "pdf":
		return consolidatePDF(path, metadata)
	default:
		return nil
	}
}

func consolidateBookMetadata(ctx context.Context, id int64, db *sql.DB, bookshelf string) error {
	var path, format, title, author string
	if err := db.QueryRowContext(ctx, `SELECT path, format, title, author FROM books WHERE id = ? AND available = 1`, id).Scan(&path, &format, &title, &author); err != nil {
		return err
	}
	metadata := extractedMetadata{Title: title, Author: author}
	var publisher, publishedAt, isbn, language, description string
	var series, seriesIndex string
	if err := db.QueryRowContext(ctx, `SELECT publisher, published_at, isbn, language, description, series, series_index FROM book_metadata WHERE book_id = ?`, id).Scan(&publisher, &publishedAt, &isbn, &language, &description, &series, &seriesIndex); err == nil {
		metadata.Publisher, metadata.PublishedAt, metadata.ISBN, metadata.Language, metadata.Description, metadata.Series, metadata.SeriesIndex = publisher, publishedAt, isbn, language, description, series, seriesIndex
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return consolidateMetadata(filepath.Join(bookshelf, filepath.FromSlash(path)), format, metadata)
}

func consolidatePDF(path string, metadata extractedMetadata) error {
	properties := map[string]string{}
	if metadata.Title != "" {
		properties["Title"] = metadata.Title
	}
	if metadata.Author != "" {
		properties["Author"] = metadata.Author
	}
	if metadata.Description != "" {
		properties["Subject"] = metadata.Description
	}
	if len(properties) == 0 {
		return nil
	}
	currentTitle, currentAuthor := pdfMetadata(path)
	if currentTitle == metadata.Title && currentAuthor == metadata.Author {
		return nil
	}
	return atomicRewrite(path, func(tempPath string) error {
		return pdfapi.AddPropertiesFile(path, tempPath, properties, pdfmodel.NewDefaultConfiguration())
	})
}

func consolidateEPUB(path string, metadata extractedMetadata) error {
	current, err := extractMetadata(path, "epub")
	if err != nil {
		return err
	}
	if !metadataDiffers(current, metadata) {
		return nil
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	_, opfPath, err := epubPackage(archive)
	if err != nil {
		return err
	}
	return atomicRewrite(path, func(tempPath string) error {
		output, err := os.Create(tempPath)
		if err != nil {
			return err
		}
		writer := zip.NewWriter(output)
		for _, entry := range archive.File {
			reader, err := entry.Open()
			if err != nil {
				_ = writer.Close()
				_ = output.Close()
				return err
			}
			data, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				_ = writer.Close()
				_ = output.Close()
				return err
			}
			if filepath.ToSlash(entry.Name) == opfPath {
				data = updateOPFMetadata(data, metadata)
			}
			header := entry.FileHeader
			header.Name = entry.Name
			header.Method = entry.Method
			destination, err := writer.CreateHeader(&header)
			if err != nil {
				_ = writer.Close()
				_ = output.Close()
				return err
			}
			if _, err := destination.Write(data); err != nil {
				_ = writer.Close()
				_ = output.Close()
				return err
			}
		}
		if err := writer.Close(); err != nil {
			_ = output.Close()
			return err
		}
		return output.Close()
	})
}

func metadataDiffers(current, desired extractedMetadata) bool {
	return (desired.Title != "" && current.Title != desired.Title) ||
		(desired.Author != "" && current.Author != desired.Author) ||
		(desired.Publisher != "" && current.Publisher != desired.Publisher) ||
		(desired.PublishedAt != "" && current.PublishedAt != desired.PublishedAt) ||
		(desired.ISBN != "" && current.ISBN != desired.ISBN) ||
		(desired.Language != "" && current.Language != desired.Language) ||
		(desired.Description != "" && current.Description != desired.Description) ||
		(desired.Series != "" && current.Series != desired.Series) ||
		(desired.SeriesIndex != "" && current.SeriesIndex != desired.SeriesIndex)
}

func updateOPFMetadata(data []byte, metadata extractedMetadata) []byte {
	text := string(data)
	values := map[string]string{"title": metadata.Title, "creator": metadata.Author, "publisher": metadata.Publisher, "date": metadata.PublishedAt, "language": metadata.Language, "description": metadata.Description, "identifier": metadata.ISBN}
	for tag, value := range values {
		if value == "" {
			continue
		}
		escaped := string(xmlEscape(value))
		pattern := regexp.MustCompile(`(?s)(<(?:(?:[A-Za-z0-9_-]+:)?` + tag + `)\b[^>]*>).*?(</(?:(?:[A-Za-z0-9_-]+:)?` + tag + `)\s*>)`)
		if pattern.MatchString(text) {
			text = pattern.ReplaceAllStringFunc(text, func(match string) string {
				parts := pattern.FindStringSubmatch(match)
				return parts[1] + escaped + parts[2]
			})
		} else if index := strings.Index(strings.ToLower(text), "</metadata>"); index >= 0 {
			element := "<" + tag + ">" + escaped + "</" + tag + ">"
			text = text[:index] + element + text[index:]
		}
	}
	return []byte(text)
}

func xmlEscape(value string) []byte {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.Bytes()
}

func atomicRewrite(path string, rewrite func(string) error) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".book-manager-metadata-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	defer os.Remove(tempPath)
	if err := rewrite(tempPath); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (a *App) refreshMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.startMetadataJob("local") {
		http.Error(w, "metadata operation already in progress", http.StatusConflict)
		return
	}
	go func() {
		refreshed, failed, err := a.refreshMetadataJob(context.Background())
		a.finishMetadataJob(refreshed, failed, err)
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (a *App) fetchOnlineMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.startMetadataJob("online") {
		http.Error(w, "metadata operation already in progress", http.StatusConflict)
		return
	}
	go func() {
		books, candidates, failed, err := a.fetchOnlineMetadataJob(context.Background())
		a.finishMetadataJob(books, failed, err)
		_ = candidates
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (a *App) searchBookMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		BookID int64  `json:"bookId"`
		Query  string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&request); err != nil {
		http.Error(w, "invalid search request", http.StatusBadRequest)
		return
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.BookID < 1 || request.Query == "" || len(request.Query) > 300 {
		http.Error(w, "book and search text are required", http.StatusBadRequest)
		return
	}
	var bookTitle string
	if err := a.db.QueryRowContext(r.Context(), `SELECT title FROM books WHERE id = ? AND available = 1`, request.BookID).Scan(&bookTitle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	matches, err := openLibraryLookup(r.Context(), &http.Client{Timeout: 15 * time.Second}, request.Query, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM metadata_candidates WHERE book_id = ?`, request.BookID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, match := range matches {
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO metadata_candidates (book_id, provider, external_id, title, author, publisher, published_at, isbn, language, description, series, series_index, cover_url, fetched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(book_id, provider, external_id) DO UPDATE SET title=excluded.title, author=excluded.author, publisher=excluded.publisher, published_at=excluded.published_at, isbn=excluded.isbn, language=excluded.language, description=excluded.description, series=excluded.series, series_index=excluded.series_index, cover_url=excluded.cover_url, fetched_at=excluded.fetched_at`, request.BookID, match.Provider, match.ExternalID, match.Title, match.Author, match.Publisher, match.PublishedAt, match.ISBN, match.Language, match.Description, match.Series, match.SeriesIndex, match.CoverURL, match.FetchedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"bookId": request.BookID, "bookTitle": bookTitle, "candidates": len(matches)})
}

func (a *App) refreshMetadataJob(ctx context.Context) (int, int, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	rows, err := a.db.QueryContext(ctx, `SELECT id, path, format FROM books WHERE available = 1 ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	type bookFile struct {
		id           int64
		path, format string
	}
	files := []bookFile{}
	for rows.Next() {
		var file bookFile
		if err := rows.Scan(&file.id, &file.path, &file.format); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, err
	}
	_ = rows.Close()
	a.setMetadataTotal(len(files))
	refreshed, failed := 0, 0
	for index, file := range files {
		fullPath := filepath.Join(a.bookshelf, filepath.FromSlash(file.path))
		metadata, err := extractMetadata(fullPath, file.format)
		if err != nil {
			failed++
			a.setMetadataProgress(index+1, failed)
			continue
		}
		if !meaningfulTitle(metadata.Title) {
			if fallbackTitle, fallbackAuthor := displayMetadata(filepath.Base(fullPath)); meaningfulTitle(fallbackTitle) {
				metadata.Title = fallbackTitle
				if metadata.Author == "" {
					metadata.Author = fallbackAuthor
				}
			}
		}
		if metadata.Series == "" {
			metadata.Series, metadata.SeriesIndex = seriesFromFilename(fullPath)
		}
		if _, err = a.db.ExecContext(ctx, `UPDATE books SET title = CASE WHEN ? != '' THEN ? ELSE title END, author = CASE WHEN ? != '' THEN ? ELSE author END WHERE id = ?`, metadata.Title, metadata.Title, metadata.Author, metadata.Author, file.id); err != nil {
			return refreshed, failed, err
		}
		if _, err = a.db.ExecContext(ctx, `INSERT INTO book_metadata (book_id, publisher, published_at, isbn, language, description, series, series_index, refreshed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(book_id) DO UPDATE SET publisher=CASE WHEN excluded.publisher != '' THEN excluded.publisher ELSE book_metadata.publisher END, published_at=CASE WHEN excluded.published_at != '' THEN excluded.published_at ELSE book_metadata.published_at END, isbn=CASE WHEN excluded.isbn != '' THEN excluded.isbn ELSE book_metadata.isbn END, language=CASE WHEN excluded.language != '' THEN excluded.language ELSE book_metadata.language END, description=CASE WHEN excluded.description != '' THEN excluded.description ELSE book_metadata.description END, series=CASE WHEN excluded.series != '' THEN excluded.series ELSE book_metadata.series END, series_index=CASE WHEN excluded.series_index != '' THEN excluded.series_index ELSE book_metadata.series_index END, refreshed_at=excluded.refreshed_at`, file.id, metadata.Publisher, metadata.PublishedAt, metadata.ISBN, metadata.Language, metadata.Description, metadata.Series, metadata.SeriesIndex, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return refreshed, failed, err
		}
		if err := consolidateMetadata(fullPath, file.format, metadata); err != nil {
			return refreshed, failed, err
		}
		refreshed++
		a.setMetadataProgress(index+1, failed)
	}
	return refreshed, failed, nil
}

func (a *App) fetchOnlineMetadataJob(ctx context.Context) (int, int, int, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	rows, err := a.db.QueryContext(ctx, `SELECT b.id, b.title, b.author, COALESCE(m.isbn, '') FROM books b LEFT JOIN book_metadata m ON m.book_id = b.id WHERE b.available = 1 ORDER BY b.title COLLATE NOCASE`)
	if err != nil {
		return 0, 0, 0, err
	}
	type lookup struct {
		id                  int64
		title, author, isbn string
	}
	lookups := []lookup{}
	for rows.Next() {
		var item lookup
		if err := rows.Scan(&item.id, &item.title, &item.author, &item.isbn); err != nil {
			_ = rows.Close()
			return 0, 0, 0, err
		}
		lookups = append(lookups, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, 0, err
	}
	_ = rows.Close()
	a.setMetadataTotal(len(lookups))
	client := &http.Client{Timeout: 15 * time.Second}
	fetched, failed := 0, 0
	for index, item := range lookups {
		matches, err := openLibraryLookup(ctx, client, item.title, item.author, item.isbn)
		if err != nil {
			failed++
			a.setMetadataProgress(index+1, failed)
			continue
		}
		if _, err := a.db.ExecContext(ctx, `DELETE FROM metadata_candidates WHERE book_id = ?`, item.id); err != nil {
			return len(lookups), fetched, failed, err
		}
		for _, match := range matches {
			_, err := a.db.ExecContext(ctx, `INSERT INTO metadata_candidates (book_id, provider, external_id, title, author, publisher, published_at, isbn, language, description, series, series_index, cover_url, fetched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(book_id, provider, external_id) DO UPDATE SET title=excluded.title, author=excluded.author, publisher=excluded.publisher, published_at=excluded.published_at, isbn=excluded.isbn, language=excluded.language, description=excluded.description, series=excluded.series, series_index=excluded.series_index, cover_url=excluded.cover_url, fetched_at=excluded.fetched_at`, item.id, match.Provider, match.ExternalID, match.Title, match.Author, match.Publisher, match.PublishedAt, match.ISBN, match.Language, match.Description, match.Series, match.SeriesIndex, match.CoverURL, match.FetchedAt)
			if err != nil {
				return len(lookups), fetched, failed, err
			}
			fetched++
		}
		a.setMetadataProgress(index+1, failed)
	}
	return len(lookups), fetched, failed, nil
}

func openLibraryLookup(ctx context.Context, client *http.Client, title, author, isbn string) ([]OnlineMetadataCandidate, error) {
	query := url.Values{}
	query.Set("limit", "5")
	query.Set("fields", "key,title,author_name,publisher,publish_date,first_publish_year,isbn,language,cover_i")
	if isbn != "" {
		query.Set("isbn", isbn)
	} else {
		query.Set("title", title)
		if author != "" {
			query.Set("author", author)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openlibrary.org/search.json?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "book-manager/1.0 (local personal library)")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("open library returned %s", response.Status)
	}
	var result openLibrarySearch
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&result); err != nil {
		return nil, err
	}
	fetchedAt := time.Now().UTC().Format(time.RFC3339)
	candidates := make([]OnlineMetadataCandidate, 0, len(result.Docs))
	for _, doc := range result.Docs {
		published := firstString(doc.PublishDate)
		if published == "" && doc.FirstPublishYear != 0 {
			published = fmt.Sprint(doc.FirstPublishYear)
		}
		candidate := OnlineMetadataCandidate{
			Provider: "openlibrary", ExternalID: doc.Key, Title: doc.Title, Author: strings.Join(doc.AuthorName, ", "),
			Publisher: firstString(doc.Publisher), PublishedAt: published, ISBN: firstISBN(doc.ISBN), Language: firstString(doc.Language), Series: firstString(doc.Series), FetchedAt: fetchedAt,
		}
		if doc.CoverID != 0 {
			candidate.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-L.jpg", doc.CoverID)
		}
		if candidate.ExternalID == "" {
			candidate.ExternalID = candidate.ISBN + ":" + candidate.Title
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func firstISBN(values []string) string {
	for _, value := range values {
		if isbn := normalizeISBN(value); isbn != "" {
			return isbn
		}
	}
	return ""
}

func (a *App) onlineMetadataCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT c.id, c.book_id, b.title, c.provider, c.external_id, c.title, c.author, c.publisher, c.published_at, c.isbn, c.language, c.description, c.series, c.series_index, c.cover_url, c.fetched_at FROM metadata_candidates c JOIN books b ON b.id = c.book_id ORDER BY c.book_id, c.id`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	candidates := []OnlineMetadataCandidate{}
	for rows.Next() {
		var candidate OnlineMetadataCandidate
		if err := rows.Scan(&candidate.ID, &candidate.BookID, &candidate.BookTitle, &candidate.Provider, &candidate.ExternalID, &candidate.Title, &candidate.Author, &candidate.Publisher, &candidate.PublishedAt, &candidate.ISBN, &candidate.Language, &candidate.Description, &candidate.Series, &candidate.SeriesIndex, &candidate.CoverURL, &candidate.FetchedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		candidates = append(candidates, candidate)
	}
	writeJSON(w, map[string]any{"candidates": candidates, "count": len(candidates)})
}

func (a *App) applyOnlineMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		CandidateIDs []int64 `json:"candidateIds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&request); err != nil || len(request.CandidateIDs) == 0 {
		http.Error(w, "select at least one metadata candidate", http.StatusBadRequest)
		return
	}
	seenBooks := map[int64]bool{}
	appliedBookIDs := []int64{}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, candidateID := range request.CandidateIDs {
		var candidate OnlineMetadataCandidate
		if err := tx.QueryRowContext(r.Context(), `SELECT id, book_id, title, author, publisher, published_at, isbn, language, description, series, series_index FROM metadata_candidates WHERE id = ?`, candidateID).Scan(&candidate.ID, &candidate.BookID, &candidate.Title, &candidate.Author, &candidate.Publisher, &candidate.PublishedAt, &candidate.ISBN, &candidate.Language, &candidate.Description, &candidate.Series, &candidate.SeriesIndex); err != nil {
			_ = tx.Rollback()
			http.Error(w, "metadata candidate is stale; fetch again", http.StatusConflict)
			return
		}
		if seenBooks[candidate.BookID] {
			_ = tx.Rollback()
			http.Error(w, "select only one candidate per book", http.StatusBadRequest)
			return
		}
		seenBooks[candidate.BookID] = true
		appliedBookIDs = append(appliedBookIDs, candidate.BookID)
		if _, err := tx.ExecContext(r.Context(), `UPDATE books SET title = CASE WHEN ? != '' THEN ? ELSE title END, author = CASE WHEN ? != '' THEN ? ELSE author END WHERE id = ?`, candidate.Title, candidate.Title, candidate.Author, candidate.Author, candidate.BookID); err != nil {
			_ = tx.Rollback()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO book_metadata (book_id, publisher, published_at, isbn, language, description, series, series_index, refreshed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(book_id) DO UPDATE SET publisher=CASE WHEN excluded.publisher != '' THEN excluded.publisher ELSE book_metadata.publisher END, published_at=CASE WHEN excluded.published_at != '' THEN excluded.published_at ELSE book_metadata.published_at END, isbn=CASE WHEN excluded.isbn != '' THEN excluded.isbn ELSE book_metadata.isbn END, language=CASE WHEN excluded.language != '' THEN excluded.language ELSE book_metadata.language END, description=CASE WHEN excluded.description != '' THEN excluded.description ELSE book_metadata.description END, series=CASE WHEN excluded.series != '' THEN excluded.series ELSE book_metadata.series END, series_index=CASE WHEN excluded.series_index != '' THEN excluded.series_index ELSE book_metadata.series_index END, refreshed_at=excluded.refreshed_at`, candidate.BookID, candidate.Publisher, candidate.PublishedAt, candidate.ISBN, candidate.Language, candidate.Description, candidate.Series, candidate.SeriesIndex, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM metadata_candidates WHERE book_id = ?`, candidate.BookID); err != nil {
			_ = tx.Rollback()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, bookID := range appliedBookIDs {
		if err := consolidateBookMetadata(r.Context(), bookID, a.db, a.bookshelf); err != nil {
			http.Error(w, "metadata applied but could not be copied into the book file: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]int{"applied": len(seenBooks)})
}

func epubPackage(z *zip.ReadCloser) (*opfPackage, string, error) {
	var container epubContainer
	for _, file := range z.File {
		if file.Name != "META-INF/container.xml" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, "", err
		}
		err = xml.NewDecoder(reader).Decode(&container)
		_ = reader.Close()
		if err != nil || len(container.Rootfiles) == 0 {
			return nil, "", errors.New("invalid epub container")
		}
		break
	}
	if len(container.Rootfiles) == 0 {
		return nil, "", errors.New("epub package not found")
	}
	rootfile := filepath.ToSlash(container.Rootfiles[0].FullPath)
	for _, file := range z.File {
		if filepath.ToSlash(file.Name) != rootfile {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, "", err
		}
		var pkg opfPackage
		err = xml.NewDecoder(reader).Decode(&pkg)
		_ = reader.Close()
		if err != nil {
			return nil, "", err
		}
		return &pkg, rootfile, nil
	}
	return nil, "", errors.New("opf package not found")
}

func epubCover(path string) (io.ReadCloser, string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, "", err
	}
	pkg, opfPath, err := epubPackage(z)
	if err != nil {
		_ = z.Close()
		return nil, "", err
	}
	coverID := ""
	for _, item := range pkg.Metadata.Items {
		if strings.EqualFold(item.Name, "cover") {
			coverID = item.Content
			break
		}
	}
	var selected *epubManifestItem
	for i := range pkg.Manifest.Items {
		item := &pkg.Manifest.Items[i]
		if (coverID != "" && item.ID == coverID) || hasWord(item.Properties, "cover-image") {
			selected = item
			break
		}
	}
	if selected == nil {
		for i := range pkg.Manifest.Items {
			item := &pkg.Manifest.Items[i]
			candidate := strings.ToLower(item.ID + " " + item.Href)
			if strings.HasPrefix(item.MediaType, "image/") && strings.Contains(candidate, "cover") {
				selected = item
				break
			}
		}
	}
	if selected == nil {
		_ = z.Close()
		return nil, "", errors.New("cover image not found")
	}
	href, err := url.PathUnescape(selected.Href)
	if err != nil {
		_ = z.Close()
		return nil, "", err
	}
	coverPath := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(opfPath), href))
	for _, file := range z.File {
		if pathpkg.Clean(file.Name) == coverPath {
			reader, err := file.Open()
			if err != nil {
				_ = z.Close()
				return nil, "", err
			}
			return &zipCoverReader{ReadCloser: reader, archive: z}, selected.MediaType, nil
		}
	}
	_ = z.Close()
	return nil, "", errors.New("cover file not found")
}

func hasWord(value, wanted string) bool {
	for _, word := range strings.Fields(value) {
		if word == wanted {
			return true
		}
	}
	return false
}

type zipCoverReader struct {
	io.ReadCloser
	archive *zip.ReadCloser
}

func (r *zipCoverReader) Close() error {
	readerErr := r.ReadCloser.Close()
	archiveErr := r.archive.Close()
	if readerErr != nil {
		return readerErr
	}
	return archiveErr
}

func openWithOS(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", path)
	case "windows":
		command = exec.Command("cmd", "/c", "start", "", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func mustUserHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return home
}

func mustExecutableDir() string {
	executable, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Dir(executable)
}

func isDevelopmentExecutable(directory string) bool {
	normalized := filepath.ToSlash(directory)
	return strings.Contains(normalized, "/go-build")
}

func openBrowser(applicationURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", applicationURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", applicationURL)
	default:
		command = exec.Command("xdg-open", applicationURL)
	}
	return command.Start()
}

func mustConfigDir() string {
	config, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(mustUserHome(), ".config")
	}
	return config
}
