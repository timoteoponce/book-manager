# Book Manager

A local, non-destructive bookshelf catalogue. It uses a normal browser for the UI and SQLite for the index; it does not embed a browser runtime or reorganize files.

## Run

Go is pinned through mise in `mise.toml`.

```sh
mise install
mise exec go@1.24 -- go run .
```

If mise is activated in your shell, `go run .` is sufficient inside this directory.

Then open <http://127.0.0.1:8765>. The default bookshelf is `~/bookshelf` and the catalogue is stored in the platform user configuration directory.

Options:

```sh
mise exec go@1.24 -- go run . -bookshelf /path/to/books -addr 127.0.0.1:8765 -db /path/to/catalog.sqlite
```

The sync indexes PDF, EPUB, CBZ, CBR, MOBI, AZW/AZW3, FB2, and TXT files. It preserves tags, notes, and reading status for future versions, but organization and metadata editing are intentionally not implemented yet.
