# Book Manager

A local, non-destructive bookshelf catalogue. It uses a normal browser for the UI and SQLite for the index; it does not embed a browser runtime or reorganize files.

## Run

Go is pinned through mise in `mise.toml`.

```sh
mise install
mise exec go@1.24 -- go run .
```

If mise is activated in your shell, `go run .` is sufficient inside this directory.

When a built binary is double-clicked, it uses the binary's directory as the bookshelf, stores its catalogue in `.book-manager/catalog.sqlite`, selects an available localhost port, and opens the default browser automatically. The browser opens before a large initial sync finishes and shows a loading state. Use `-no-browser` for terminal use.

Then open <http://127.0.0.1:8765>. The default bookshelf is `~/bookshelf` and the catalogue is stored in the platform user configuration directory.

Options:

```sh
mise exec go@1.24 -- go run . -bookshelf /path/to/books -addr 127.0.0.1:8765 -db /path/to/catalog.sqlite
```

The sync indexes PDF, EPUB, CBZ, CBR, MOBI, AZW/AZW3, FB2, and TXT files. It preserves tags, notes, and reading status. Organization is optional and starts with a preview; approved moves use the human-readable `Author/Title/Title.ext` layout, refuse collisions, and can undo the latest run.
