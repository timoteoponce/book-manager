package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSyncPreservesMetadataAcrossRenameAndRemoval(t *testing.T) {
	bookshelf := t.TempDir()
	original := filepath.Join(bookshelf, "Author - A Book.epub")
	if err := os.WriteFile(original, []byte("book contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:sync-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, bookshelf: bookshelf}
	if err := app.initDB(context.Background()); err != nil {
		t.Fatal(err)
	}

	result, err := app.sync(context.Background())
	if err != nil || result.Indexed != 1 || result.Unavailable != 0 {
		t.Fatalf("initial sync = %#v, %v", result, err)
	}
	if _, err := db.Exec(`UPDATE books SET tags = 'favorite', notes = 'keep this'`); err != nil {
		t.Fatal(err)
	}

	renamed := filepath.Join(bookshelf, "Author - Renamed Book.epub")
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	result, err = app.sync(context.Background())
	if err != nil || result.Renamed != 1 {
		t.Fatalf("rename sync = %#v, %v", result, err)
	}
	var path, tags, notes string
	if err := db.QueryRow(`SELECT path, tags, notes FROM books`).Scan(&path, &tags, &notes); err != nil {
		t.Fatal(err)
	}
	if path != "Author - Renamed Book.epub" || tags != "favorite" || notes != "keep this" {
		t.Fatalf("metadata after rename = %q, %q, %q", path, tags, notes)
	}

	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	result, err = app.sync(context.Background())
	if err != nil || result.Unavailable != 1 {
		t.Fatalf("removal sync = %#v, %v", result, err)
	}
	var available int
	if err := db.QueryRow(`SELECT available FROM books`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 0 {
		t.Fatalf("removed book availability = %d", available)
	}
}

func TestSyncReusesUnchangedFileCache(t *testing.T) {
	bookshelf := t.TempDir()
	filePath := filepath.Join(bookshelf, "Author - Cached Book.pdf")
	if err := os.WriteFile(filePath, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:cache-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, bookshelf: bookshelf}
	if err := app.initDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE books SET title = 'Cached Metadata', author = 'Cached Author', fingerprint = 'cached:fingerprint'`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var title, author, fingerprint string
	if err := db.QueryRow(`SELECT title, author, fingerprint FROM books`).Scan(&title, &author, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if title != "Cached Metadata" || author != "Cached Author" || fingerprint != "cached:fingerprint" {
		t.Fatalf("cached metadata = %q, %q, %q", title, author, fingerprint)
	}
}

func TestEPUBCoverSupportsEPUB2Metadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cover.epub")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?><container><rootfiles><rootfile full-path="OEBPS/content.opf"/></rootfiles></container>`,
		"OEBPS/content.opf":      `<package><metadata><meta name="cover" content="cover-image"/></metadata><manifest><item id="cover-image" href="images/cover.jpg" media-type="image/jpeg"/></manifest></package>`,
		"OEBPS/images/cover.jpg": "cover bytes",
	}
	for name, content := range entries {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	cover, mediaType, err := epubCover(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cover.Close()
	data, err := io.ReadAll(cover)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cover bytes" || mediaType != "image/jpeg" {
		t.Fatalf("cover = %q (%s)", data, mediaType)
	}
}

func TestPDFMetadataReadsInfoDictionary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(path, []byte("7 0 obj << /Title (A PDF Title) /Author (Jane Doe) >> endobj"), 0o644); err != nil {
		t.Fatal(err)
	}
	title, author := pdfMetadata(path)
	if title != "A PDF Title" || author != "Jane Doe" {
		t.Fatalf("metadata = %q, %q", title, author)
	}
}

func TestPDFMetadataDecodesEscapedUTF16Title(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utf16-title.pdf")
	encodedTitle := `\376\377\000W\000h\000a\000t\000\040\000y\000o\000u\000\040\000n\000e\000e\000d\000\040\000t\000o\000\040\000k\000n\000o\000w\000\040\000a\000b\000o\000u\000t\000\040\000d\000a\000t\000e\000s\000\040\000a\000n\000d\000\040\000t\000i\000m\000e\000s\000\040\000i\000n\000\040\000c\000o\000m\000p\000u\000t\000i\000n\000g`
	if err := os.WriteFile(path, []byte("7 0 obj << /Title ("+encodedTitle+") >> endobj"), 0o644); err != nil {
		t.Fatal(err)
	}
	title, _ := pdfMetadata(path)
	expected := "What you need to know about dates and times in computing"
	if title != expected {
		t.Fatalf("decoded title = %q, want %q", title, expected)
	}
}

func TestOrganizationTargetIsHumanReadableAndSafe(t *testing.T) {
	target := organizationTarget(Book{
		Folder:   "Software Development/Legacy",
		Filename: "Original.pdf",
		Title:    `A <Practical> Book`,
		Author:   "Jane/Doe",
		Format:   "pdf",
	})
	if target != "Jane_Doe/A _Practical_ Book/A _Practical_ Book.pdf" {
		t.Fatalf("organization target = %q", target)
	}
	if sanitizePathPart("CON") != "_CON" {
		t.Fatalf("reserved path part = %q", sanitizePathPart("CON"))
	}
}

func TestOrganizationApplyAndUndo(t *testing.T) {
	bookshelf := t.TempDir()
	original := filepath.Join(bookshelf, "Author - A Book.pdf")
	if err := os.WriteFile(original, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:organize-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, bookshelf: bookshelf}
	if err := app.initDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	applyResponse := httptest.NewRecorder()
	app.organizationApply(applyResponse, httptest.NewRequest("POST", "/api/organize/apply", nil))
	if applyResponse.Code != 200 {
		t.Fatalf("apply status = %d: %s", applyResponse.Code, applyResponse.Body.String())
	}
	organized := filepath.Join(bookshelf, "Author", "A Book", "A Book.pdf")
	if _, err := os.Stat(organized); err != nil {
		t.Fatalf("organized file missing: %v", err)
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Fatalf("original file still exists: %v", err)
	}

	undoResponse := httptest.NewRecorder()
	app.organizationUndo(undoResponse, httptest.NewRequest("POST", "/api/organize/undo", nil))
	if undoResponse.Code != 200 {
		t.Fatalf("undo status = %d: %s", undoResponse.Code, undoResponse.Body.String())
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("original file not restored: %v", err)
	}
	if _, err := os.Stat(organized); !os.IsNotExist(err) {
		t.Fatalf("organized file still exists: %v", err)
	}
}

func TestCleanupCandidatesFindDuplicatesAndMissingRecords(t *testing.T) {
	bookshelf := t.TempDir()
	first := filepath.Join(bookshelf, "Author - First.pdf")
	second := filepath.Join(bookshelf, "Author - Second.pdf")
	if err := os.WriteFile(first, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:cleanup-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{db: db, bookshelf: bookshelf}
	if err := app.initDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidates, err := app.cleanupCandidates(context.Background())
	if err != nil || len(candidates) != 1 || candidates[0].Reason != "exact_duplicate" {
		t.Fatalf("duplicate candidates = %#v, %v", candidates, err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidates, err = app.cleanupCandidates(context.Background())
	if err != nil || len(candidates) != 1 || candidates[0].Reason != "unavailable_record" {
		t.Fatalf("missing candidates = %#v, %v", candidates, err)
	}
	response := httptest.NewRecorder()
	app.forgetCleanupRecord(response, httptest.NewRequest("POST", "/api/cleanup/forget/"+fmt.Sprint(candidates[0].ID), nil))
	if response.Code != 200 {
		t.Fatalf("forget status = %d: %s", response.Code, response.Body.String())
	}
}
