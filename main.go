package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web/index.html
var webFS embed.FS

var supportedFormats = map[string]bool{
	".azw": true, ".azw3": true, ".cb7": true, ".cbr": true, ".cbz": true,
	".epub": true, ".fb2": true, ".mobi": true, ".pdf": true, ".txt": true,
}

type App struct {
	db        *sql.DB
	bookshelf string
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

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Metadata struct {
		Title   string `xml:"title"`
		Creator string `xml:"creator"`
	} `xml:"metadata"`
}

func main() {
	defaultBookshelf := filepath.Join(mustUserHome(), "bookshelf")
	defaultDB := filepath.Join(mustConfigDir(), "book-manager", "catalog.sqlite")
	bookshelf := flag.String("bookshelf", defaultBookshelf, "folder to catalogue")
	dbPath := flag.String("db", defaultDB, "SQLite catalogue path")
	addr := flag.String("addr", "127.0.0.1:8765", "HTTP listen address")
	flag.Parse()

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
	if _, err := app.sync(context.Background()); err != nil {
		log.Printf("initial sync failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.index)
	mux.HandleFunc("/api/books", app.books)
	mux.HandleFunc("/api/sync", app.syncHandler)
	mux.HandleFunc("/api/open/", app.open)
	log.Printf("book manager: %s", app.bookshelf)
	log.Printf("open http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
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
		fingerprint, err := fileFingerprint(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(a.bookshelf, path)
		if err != nil {
			return err
		}
		title, author := displayMetadata(entry.Name())
		if ext == ".epub" {
			if epubTitle, epubAuthor := epubMetadata(path); epubTitle != "" {
				title, author = epubTitle, epubAuthor
			}
		}
		found = append(found, foundBook{
			path: filepath.ToSlash(rel), folder: filepath.ToSlash(filepath.Dir(rel)),
			filename: entry.Name(), title: title, author: author, format: strings.TrimPrefix(ext, "."),
			size: info.Size(), modified: info.ModTime().UTC().Format(time.RFC3339Nano), fingerprint: fingerprint,
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

func (a *App) books(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
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
	result, err := a.sync(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

func (a *App) open(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var id int64
	if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/open/"), "%d", &id); err != nil {
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

func epubMetadata(path string) (string, string) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", ""
	}
	defer z.Close()
	var container epubContainer
	for _, file := range z.File {
		if file.Name != "META-INF/container.xml" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", ""
		}
		err = xml.NewDecoder(reader).Decode(&container)
		_ = reader.Close()
		if err != nil || len(container.Rootfiles) == 0 {
			return "", ""
		}
		break
	}
	for _, file := range z.File {
		if filepath.ToSlash(file.Name) != filepath.ToSlash(container.Rootfiles[0].FullPath) {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", ""
		}
		var pkg opfPackage
		err = xml.NewDecoder(reader).Decode(&pkg)
		_ = reader.Close()
		if err != nil {
			return "", ""
		}
		return strings.TrimSpace(pkg.Metadata.Title), strings.TrimSpace(pkg.Metadata.Creator)
	}
	return "", ""
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

func mustConfigDir() string {
	config, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(mustUserHome(), ".config")
	}
	return config
}
