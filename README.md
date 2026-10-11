# Book Manager

A lightweight, local-first catalogue for an existing bookshelf. Book Manager indexes files in place, stores searchable metadata in SQLite, and opens books with the operating system's configured reader. It is designed to remain useful from Finder, Explorer, or a Linux file manager without creating a Calibre-style opaque library.

The application is a single Go binary with an embedded web interface. It does not require Electron, Tauri, a database server, or an application runtime.

## Current Status

Usable personal-library prototype. The core catalogue, metadata review, cleanup, and organization workflows are implemented, but packaging and cross-platform smoke testing still need to be completed before calling it a stable release.

Implemented:

- Recursive local indexing of PDF, EPUB, CBZ, CBR, MOBI, AZW, AZW3, FB2, and TXT files.
- Rebuildable SQLite catalogue with relative filesystem paths.
- SHA-256 file fingerprints for rename detection and exact duplicate detection.
- Non-destructive sync that preserves unavailable records and user metadata.
- Search by title, author, folder, filename, tags, format, and reading status.
- EPUB covers and locally extracted EPUB/PDF metadata.
- Series and series index from EPUB metadata (Calibre and EPUB 3 collections) with filename/folder fallback.
- Rejection of placeholder titles such as `0 (2)` in favour of the filename-derived title.
- PDF metadata decoding including escaped UTF-16 and octal strings.
- Editable tags, notes, and reading status.
- Metadata consolidation into supported EPUB and PDF files using atomic replacement.
- Single-book and bulk deletion through recoverable `.book-manager/trash/` moves.
- JSON catalogue export.
- Optional Open Library metadata lookup with per-book candidate selection.
- Human-readable organization preview and collision-safe moves:

  ```text
  Author/Title/Title.ext
  ```

- Organization undo support.
- Cleanup candidates for exact duplicates, possible duplicates, same ISBN, and unavailable records.
- Recoverable cleanup moves under `.book-manager/trash/`.
- Portable executable mode with executable-relative storage.
- Automatic browser launch, background initial sync, progress state, and idle shutdown after the browser closes.

## Safety Model

- Normal sync never moves, renames, or deletes book files.
- Online metadata fetching is opt-in and never changes metadata automatically.
- Metadata changes from online sources require selecting a candidate first.
- Organization requires a preview and refuses collisions.
- Cleanup does not permanently delete book files; selected files move to recoverable trash.
- The SQLite catalogue can be rebuilt by scanning the bookshelf again.
- The application binds to localhost only.
- Embedded metadata writes that cannot be completed safely are reported instead of replacing the original file.

## Run From Source

Go is pinned through mise in `mise.toml`.

```sh
mise install
mise exec go@1.24 -- go run .
```

For a development run, the default bookshelf is `~/bookshelf` and the catalogue is stored in the platform configuration directory.

Useful options:

```sh
mise exec go@1.24 -- go run . \
  -bookshelf /path/to/books \
  -db /path/to/catalog.sqlite \
  -addr 127.0.0.1:8765 \
  -no-browser
```

The server normally chooses an available localhost port. `-no-browser` is useful for testing or when opening the URL manually.

## Portable Binary

When a built binary is double-clicked, its directory becomes the bookshelf:

```text
BookManager/
  book-manager
  book.epub
  another-book.pdf
  .book-manager/
    catalog.sqlite
```

The binary selects an available localhost port and opens the default browser. The browser opens before a large initial sync finishes and displays progress while the catalogue is built. When the browser tab is closed, the binary exits after a short heartbeat timeout.

Build the current Linux binary:

```sh
CGO_ENABLED=0 mise exec go@1.24 -- go build -o book-manager ./...
```

Cross-platform release builds need platform-specific smoke testing. Example targets:

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 mise exec go@1.24 -- go build -o book-manager.exe ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 mise exec go@1.24 -- go build -o book-manager-macos ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 mise exec go@1.24 -- go build -o book-manager-linux ./...
```

## Online Metadata

Normal operation is offline. **Fetch online metadata** is a separate explicit action that queries Open Library using an ISBN when available, or title and author otherwise.

Results are stored as candidates. The user reviews candidates per book and chooses which one to apply. Existing local metadata is not overwritten during the fetch step.

## Development

Format, test, and build:

```sh
mise exec go@1.24 -- gofmt -w main.go main_test.go
mise exec go@1.24 -- go test ./...
CGO_ENABLED=0 mise exec go@1.24 -- go build ./...
```

The test suite covers sync preservation, rename detection, cache reuse, EPUB covers and metadata, PDF metadata decoding, organization moves and undo, and cleanup behavior.

## Roadmap

- Cross-platform packaging and smoke tests on Windows and macOS.
- Better progress reporting for very large scans.
- More format-specific metadata and preview support.
- Optional additional metadata providers.
- Restore tooling for the recoverable cleanup trash.
- More UI tests and accessibility polish.

## Privacy

The application is local by default. No network requests occur during normal scanning, previewing, searching, or organization. Network access occurs only when the user explicitly starts online metadata fetching.
