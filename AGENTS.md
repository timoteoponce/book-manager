# AGENTS.md

Guidance for AI agents and new contributors working on Book Manager.

Read this before changing code. Several of the constraints below are non-obvious and
breaking them causes silent data corruption or build failures.

## What this project is

A single self-contained Go binary that catalogues an existing bookshelf in place.
There is no installer, no daemon, and no Electron/Tauri shell. The UI is one
embedded HTML file served from `http://127.0.0.1:<random-port>` and opened in the
user's default browser.

Design rules that the project must not violate:

- **The filesystem is the source of truth.** SQLite is a rebuildable index.
- **Never move, rename, or delete a book without explicit user confirmation.**
- **Never silently overwrite user metadata.** Online metadata is review-first.
- **Never make network calls implicitly.** Only the two explicit fetch actions.
- **Stay dependency-light.** Two direct dependencies, both pure Go.

## Layout

```
main.go              all backend: HTTP routes, sync, metadata, cleanup, trash
main_test.go         tests (17 test functions)
web/index.html       the entire frontend: CSS + JS inline, embedded via //go:embed
mise.toml            pins Go 1.24
```

There is **no frontend build step**. Editing `web/index.html` and rebuilding the
binary is the entire frontend workflow. Do not introduce a bundler, npm, or a
framework without asking first.

## Build and test

Go is pinned via mise. Always scope commands to the pinned toolchain:

```sh
mise exec go@1.24 -- go test ./...
CGO_ENABLED=0 mise exec go@1.24 -- go build -o book-manager ./...
mise exec go@1.24 -- gofmt -l main.go main_test.go
```

`CGO_ENABLED=0` is mandatory for the portable binary. Without it you get a
dynamically linked build that will not run on other machines.

### Toolchain traps

1. **`mise exec --` does not work.** The global `~/.config/mise/config.toml` on the
   maintainer's machine pins an unavailable Java version, so bare `mise exec` fails
   before Go starts. Always name the tool: `mise exec go@1.24 -- <cmd>`.

2. **`github.com/pdfcpu/pdfcpu` must stay at `v0.11.1`.** v0.12.0 and newer require
   Go >= 1.25, v0.15.0+ requires 1.25, and v0.16.x requires 1.26. Bumping it forces a
   Go upgrade and a toolchain download. pdfcpu is used only for writing PDF
   document properties; it roughly doubles the binary size (16 MB -> 27 MB).

3. After any dependency change run `GOTOOLCHAIN=local mise exec go@1.24 -- go mod tidy`.
   Without `GOTOOLCHAIN=local`, Go will silently rewrite `go.mod` to a newer language
   version to satisfy a dependency, which then fails to build under Go 1.24.

4. `go mod tidy` may print missing `go.sum` entries first. Re-run it, then build.

## Architecture

### Sync

`syncUnlocked` walks the bookshelf, skipping `appDataDirName` (`.book-manager`).

The skip is **load-bearing**, not an optimisation. `.book-manager/trash/` holds
deleted books; indexing it caused deleted books to reappear in the catalogue,
created nested `trash/RUN_A/.book-manager/trash/RUN_B/...` paths, and made every
scan re-hash ~12 GB. After committing the scan, rows whose `path` starts with
`.book-manager/` are deleted defensively in case an older catalogue still has them.

Per-file change detection uses `(size, modified_at)`. Only changed files are
re-hashed (SHA-256) and re-parsed. Fingerprints enable rename detection and exact
duplicate detection.

`syncMu` serialises all scanning and metadata jobs. `stateMu` guards the
progress/status fields exposed by `/api/sync/status` and `/api/metadata/status`.

### Metadata extraction

`extractMetadata(path, format)` handles `epub` (full OPF parse) and `pdf` (regex over
the Info dictionary). Other formats return empty, and callers fall back to
filename-derived values.

PDF metadata strings are escaped and often UTF-16BE with a `FEFF` BOM. `decodePDFString`
handles octal escapes, `\n`/`\r`/`\t`, and both UTF-16 byte orders. A naive
unescape produces garbage like `\376\377\000W\000h...` in the UI.

Series comes from three sources, in priority order:

1. `calibre:series` / `calibre:series_index` OPF meta tags.
2. EPUB 3 `belongs-to-collection` / `collection-type` / `group-position`.
3. Filename and folder heuristics in `seriesFromFilename`.

Real-world note: many Kindle/KOReader-converted EPUBs (for example the
"My Hero Academia Vigilantes" collection) carry **no series metadata at all**, so
the heuristic path is the common case, not the fallback. The heuristic tolerates
singular/plural and punctuation differences between folder and filename.

`meaningfulTitle` rejects placeholder titles such as `0 (2)` that comics archives
commonly contain, falling back to the filename.

### Writing metadata into files

`consolidateMetadata` rewrites EPUB OPF or PDF Info properties. It is invoked on
local metadata refresh, on confirmed online metadata, and after saving book
metadata. It is **not** invoked on ordinary sync for unchanged files.

All writes go through `atomicRewrite`: write to a temp file in the same directory,
then `os.Rename` over the original. Never edit a book in place.

EPUB rewriting replaces the whole zip entry-by-entry, preserving compression
method. `updateOPFMetadata` uses `ReplaceAllStringFunc` rather than
`ReplaceAllString` because the replacement text can contain `$` sequences, which
`Expand` would mangle.

### Deletion and trash

Deletions never unlink files. Files move to
`.book-manager/trash/<run-id>/<original relative path>`.

- `/api/trash` lists runs with file counts and sizes.
- `/api/trash/restore` moves files back and re-syncs. It refuses if any original
  path is occupied.
- `/api/trash/delete` permanently removes a run.

`validTrashRunID` rejects anything outside `[A-Za-z0-9._-]` and any `..`, since the
run ID becomes a path segment. Keep this validation on any new trash endpoint.

After restore, the emptied directory skeleton is removed deepest-first. Removing
`runPath` in the same pass fails, so `runPath` itself is excluded from that list.

There is **no automatic retention policy**. Nothing is deleted on a timer. If a
retention policy is ever added it must be opt-in, visible in the Trash view, and
never remove the newest run.

### Organization

`organizationProposals` builds a `Author/Title/Title.ext` layout. Apply refuses any
collision, moves files, re-syncs, and records the move in `organization_moves` for
undo. Undo applies to the most recent not-yet-undone run only.

## Frontend notes

- One file. CSS and JS are inline in `web/index.html`.
- Icons are inline SVG paths in the `icons` object, rendered by `iconButton()`.
  Add new icons there rather than introducing an icon font or CDN.
- **`pinDialogActions(selector)` prepends `.detail-actions` to a dialog's root and
  adds `.dialog-actions` (sticky top).** It is called for detail, cleanup, trash,
  online-candidate, and organization dialogs, but deliberately **not** for the
  confirmation dialog.
- `.dialog-actions` must not carry a negative top margin. Doing so caused the
  confirm dialog's buttons to overlap and slice its message text in half.
- `color-scheme: light` is set on `:root`. Without it, a dark-mode OS renders UA
  widgets against the light palette and dialogs become unreadable.
- Status polling must never redraw the card grid. `pollSyncStatus` and
  `pollMetadataStatus` only update status text and buttons; `load()` is called on
  search/filter change and when a job actually completes. An earlier version called
  `load()` on a 2 s interval, which rebuilt every card and caused visible flicker.

### Verifying UI changes

Do not eyeball CSS. Render it and look. Build a throwaway harness with the real
stylesheet extracted from `web/index.html`:

```sh
python3 - <<'PY'
import re
src = open('web/index.html', encoding='utf-8').read()
css = re.search(r'<style>(.*?)</style>', src, re.S).group(1)
open('/tmp/harness.html','w').write(f"<!doctype html><style>{css}</style><body>...</body>")
PY
firefox --headless --profile "$(mktemp -d)" --window-size=1200,800 \
  --screenshot /tmp/out.png file:///tmp/harness.html
```

Add `<script>document.getElementById('d').showModal()</script>` to test real modal
rendering; an `open` attribute renders inline and hides backdrop bugs.

This technique is how the confirmation-dialog clipping bug was found.

## Local testing against the real library

The maintainer keeps a live library at `~/bookshelf` with the binary copied in.
**Do not run destructive operations against it.** The catalogued library contains
roughly 690 books and ~13 GB.

Use a sandbox copy for anything that deletes, restores, or rewrites files:

```sh
cp -r "~/bookshelf/Manga & Comics/My Hero Academia Vigilantes" /tmp/mha/
book-manager -no-browser -bookshelf /tmp/mha -db /tmp/mha.sqlite -addr 127.0.0.1:8799
```

Because the app exits when the browser heartbeat stops, background servers started
from a tool call are killed when the call returns. Start, query, and kill within a
**single** shell invocation, and always `kill` at the end.

## Testing conventions

- Tests use `t.TempDir()` and an in-memory SQLite DB named per test, with
  `db.SetMaxOpenConns(1)` because the shared in-memory cache needs a single
  connection.
- Sync tests write dummy content (`[]byte("pdf")`) that is not a valid PDF or
  EPUB. `consolidateMetadata` logs `metadata consolidation skipped ...` warnings
  for these. **That output is expected and the tests are still passing.**
- `newTestApp(t, name)` is the shared helper for trash/metadata tests.
- `writeTestEPUB(t, opf)` builds a minimal valid EPUB for metadata tests.
- Cover the destructive paths: trash restore, purge, app-data exclusion, and
  rejection of unsafe run IDs.

## API reference

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/books` | List/filter by `q`, `format`, `status`; searches title, author, folder, filename, tags, series |
| GET | `/api/book/{id}` | Full record including series and extended metadata |
| PATCH | `/api/book/{id}` | Save status, tags, notes; then consolidates into the file |
| POST | `/api/books/delete` | Move books to trash (JSON `{ids:[...]}`) |
| POST | `/api/sync` | Start a background scan (202, or 409 if running) |
| GET | `/api/sync/status` | `syncing`, `result`, `error` |
| POST | `/api/heartbeat` | Keeps the portable process alive; 204 |
| POST | `/api/open/{id}` | Open with the OS default reader |
| POST | `/api/reveal/{id}` | Show in Finder / Explorer / parent folder |
| GET | `/api/cover/{id}` | EPUB cover image bytes |
| GET | `/api/preview/{id}` | Inline PDF bytes (endpoint retained; UI no longer embeds it) |
| GET | `/api/export` | JSON catalogue download |
| POST | `/api/metadata/refresh` | Start local metadata refresh (202) |
| POST | `/api/metadata/fetch-online` | Start Open Library lookup (202) |
| GET | `/api/metadata/status` | `running`, `operation`, `current`, `total`, `failed`, `error` |
| GET | `/api/metadata/candidates` | Stored online candidates for review |
| POST | `/api/metadata/apply-online` | Apply selected candidates, then consolidate |
| POST | `/api/metadata/search` | Per-book Open Library search (JSON `{bookId, query}`) |
| GET | `/api/cleanup/candidates` | Exact duplicates, possible duplicates, same ISBN, unavailable |
| POST | `/api/cleanup/delete` | Move selected candidates to trash |
| POST | `/api/cleanup/forget/{id}` | Drop an unavailable catalogue record |
| GET | `/api/organize/preview` | Proposed human-readable paths |
| POST | `/api/organize/apply` | Perform the moves (409 on any collision) |
| POST | `/api/organize/undo` | Revert the latest run |
| GET | `/api/trash` | List trash runs with sizes |
| POST | `/api/trash/restore` | Restore a run |
| POST | `/api/trash/delete` | Permanently delete a run |

Mutating endpoints are POST-only and return 405 otherwise.

## Behaviour worth knowing before changing it

- **Offline by default.** Only `fetch-online` and `metadata/search` touch the
  network, both user-initiated. No telemetry, no CDN, no font downloads.
- **Metadata refresh is slow on purpose.** It rewrites every EPUB/PDF, so a 690-book
  library takes tens of seconds. Progress is exposed via `/api/metadata/status`.
- **Online matches need manual selection.** Open Library title searches are fuzzy;
  applying blindly corrupts titles. Never auto-apply.
- **Renames preserve metadata** through SHA-256 fingerprint matching.
- **Unavailable books are retained** as catalogue records marked unavailable, not
  deleted, so metadata survives a temporarily missing drive.

## Known gaps

- Cross-platform packaging is untested beyond Linux; `explorer /select,` and
  `open -R` paths have not been exercised.
- CBZ/CBR and MOBI metadata extraction is not implemented; only EPUB and PDF.
- The embedded PDF preview endpoint is unused by the UI and can be removed.
- No FTS index; search uses `LIKE`, which is acceptable at this scale but will slow
  down on much larger libraries.
- Series names are not normalised across sources, so a library can legitimately show
  `My Hero Academia Vigilantes` and `My Hero Academia, Vigilantes` as separate series.