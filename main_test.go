package main

import (
	"context"
	"database/sql"
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
