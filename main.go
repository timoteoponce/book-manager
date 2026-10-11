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
	"unicode/utf16"

	_ "modernc.org/sqlite"
)

//go:embed web/index.html
var webFS embed.FS

var supportedFormats = map[string]bool{
	".azw": true, ".azw3": true, ".cb7": true, ".cbr": true, ".cbz": true,
	".epub": true, ".fb2": true, ".mobi": true, ".pdf": true, ".txt": true,
}

type App struct {
	db          *sql.DB
	bookshelf   string
	syncMu      sync.Mutex
	stateMu     sync.RWMutex
	syncing     bool
	lastSync    SyncResult
	lastSyncErr string
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
		Title   string     `xml:"title"`
		Creator string     `xml:"creator"`
		Items   []epubMeta `xml:"meta"`
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
	Name    string `xml:"name,attr"`
	Content string `xml:"content,attr"`
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
	mux.HandleFunc("/api/open/", app.open)
	mux.HandleFunc("/api/preview/", app.preview)
	mux.HandleFunc("/api/cover/", app.cover)
	mux.HandleFunc("/api/cleanup/candidates", app.cleanupCandidatesHandler)
	mux.HandleFunc("/api/cleanup/forget/", app.forgetCleanupRecord)
	log.Printf("book manager: %s", app.bookshelf)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	applicationURL := "http://" + listener.Addr().String()
	log.Printf("open %s", applicationURL)
	if !*noBrowser {
		if err := openBrowser(applicationURL); err != nil {
			log.Printf("could not open browser: %v", err)
		}
	}
	app.setSyncing(true)
	go func() {
		if _, err := app.syncInBackground(context.Background()); err != nil {
			log.Printf("initial sync failed: %v", err)
		}
	}()
	log.Fatal(http.Serve(listener, mux))
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
	`); err != nil {
		return err
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
		if cacheErr == nil && cachedFingerprint != "" && cachedSize == info.Size() && cachedModified == modified {
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
	rows, err := a.db.QueryContext(ctx, `SELECT id, path, title, author, format, size, modified_at, fingerprint FROM books WHERE available = 1 AND fingerprint != '' ORDER BY fingerprint, modified_at, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type duplicateRecord struct {
		candidate   CleanupCandidate
		fingerprint string
	}
	duplicates := map[string][]duplicateRecord{}
	for rows.Next() {
		var record duplicateRecord
		if err := rows.Scan(&record.candidate.ID, &record.candidate.Path, &record.candidate.Title, &record.candidate.Author, &record.candidate.Format, &record.candidate.Size, &record.candidate.ModifiedAt, &record.fingerprint); err != nil {
			return nil, err
		}
		record.candidate.Reason = "exact_duplicate"
		record.candidate.Confidence = "high"
		record.candidate.Available = true
		duplicates[record.fingerprint] = append(duplicates[record.fingerprint], record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	candidates := []CleanupCandidate{}
	for _, records := range duplicates {
		if len(records) < 2 {
			continue
		}
		keeper := records[0].candidate.Path
		for _, record := range records[1:] {
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

func (a *App) books(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	query := `SELECT id, path, folder, filename, title, author, format, size, modified_at, status, tags, notes, last_synced_at, available FROM books`
	args := []any{}
	conditions := []string{}
	if q != "" {
		conditions = append(conditions, `(title LIKE ? OR author LIKE ? OR folder LIKE ? OR filename LIKE ? OR tags LIKE ?)`)
		term := "%" + q + "%"
		args = append(args, term, term, term, term, term)
	}
	if format != "" {
		conditions = append(conditions, `format = ?`)
		args = append(args, format)
	}
	if status != "" {
		if status == "__none__" {
			conditions = append(conditions, `status = ''`)
		} else {
			conditions = append(conditions, `status = ?`)
			args = append(args, status)
		}
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY title COLLATE NOCASE, author COLLATE NOCASE"
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
		if err := rows.Scan(&book.ID, &book.Path, &book.Folder, &book.Filename, &book.Title, &book.Author, &book.Format, &book.Size, &book.ModifiedAt, &book.Status, &book.Tags, &book.Notes, &book.LastSyncedAt, &available); err != nil {
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
	writeJSON(w, book)
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
