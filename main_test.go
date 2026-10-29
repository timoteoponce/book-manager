package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
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

func TestSyncRefreshesStalePDFMetadata(t *testing.T) {
	bookshelf := t.TempDir()
	source := writeTestPDF(t, "<< /Title (Correct PDF Title) /Author (Jane Doe) >>")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bookshelf, "book.pdf"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:pdf-metadata-version-test?mode=memory&cache=shared")
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
	if _, err := db.Exec(`UPDATE books SET title = 'Cover', author = 'Wrong', metadata_version = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var title, author string
	if err := db.QueryRow(`SELECT title, author FROM books`).Scan(&title, &author); err != nil {
		t.Fatal(err)
	}
	if title != "Correct PDF Title" || author != "Jane Doe" {
		t.Fatalf("metadata = %q, %q", title, author)
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

func TestEPUBMetadataExtractsIdentifierAndPublicationData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.epub")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?><container><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`,
		"content.opf":            `<package><metadata><title>A Book</title><creator>Jane Doe</creator><publisher>Example Press</publisher><date>2024-01-02</date><language>en</language><identifier scheme="ISBN">9780306406157</identifier></metadata></package>`,
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
	metadata, err := extractMetadata(path, "epub")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Title != "A Book" || metadata.Author != "Jane Doe" || metadata.Publisher != "Example Press" || metadata.PublishedAt != "2024-01-02" || metadata.ISBN != "9780306406157" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if err := consolidateMetadata(path, "epub", extractedMetadata{Title: "Updated Book", Author: "New Author", Publisher: "New Press", PublishedAt: "2025", ISBN: "9783161484100", Language: "es"}); err != nil {
		t.Fatal(err)
	}
	updated, err := extractMetadata(path, "epub")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Updated Book" || updated.Author != "New Author" || updated.Publisher != "New Press" || updated.PublishedAt != "2025" || updated.ISBN != "9783161484100" || updated.Language != "es" {
		t.Fatalf("updated metadata = %#v", updated)
	}
}

func TestEPUBMetadataExtractsCalibreSeries(t *testing.T) {
	path := writeTestEPUB(t, `<package><metadata><title>My Hero Academia Vigilantes, Vol 3</title><creator>Hideyuki Furuhashi</creator><meta name="calibre:series" content="My Hero Academia: Vigilantes"/><meta name="calibre:series_index" content="3"/></metadata></package>`)
	metadata, err := extractMetadata(path, "epub")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Series != "My Hero Academia: Vigilantes" || metadata.SeriesIndex != "3" {
		t.Fatalf("series = %q index = %q", metadata.Series, metadata.SeriesIndex)
	}
}

func TestEPUBMetadataExtractsEPUB3SeriesCollection(t *testing.T) {
	path := writeTestEPUB(t, `<package><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><meta property="belongs-to-collection" id="c01">My Hero Academia: Vigilantes</meta><meta property="collection-type" refines="#c01">series</meta><meta property="group-position" refines="#c01">3</meta></metadata></package>`)
	metadata, err := extractMetadata(path, "epub")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Series != "My Hero Academia: Vigilantes" || metadata.SeriesIndex != "3" {
		t.Fatalf("series = %q index = %q", metadata.Series, metadata.SeriesIndex)
	}
}

func TestSeriesFallsBackToFilenameAndFolder(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "My Hero Academia Vigilantes")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if !meaningfulTitle("My Hero Academia Vigilantes") || meaningfulTitle("0 (2)") || meaningfulTitle("") {
		t.Fatal("meaningfulTitle rejected or accepted the wrong values")
	}
	series, index := seriesFromFilename(filepath.Join(directory, "Kohei Horikoshi - My Hero Academia Vigilantes, Vol 7.epub"))
	if series != "My Hero Academia Vigilantes" || index != "7" {
		t.Fatalf("folder series = %q index = %q", series, index)
	}
	series, index = seriesFromFilename(filepath.Join(directory, "My Hero Academia Vigilantes Vol 1.epub"))
	if series != "My Hero Academia Vigilantes" || index != "1" {
		t.Fatalf("bare folder series = %q index = %q", series, index)
	}
	series, index = seriesFromFilename(filepath.Join(directory, "Kohei Horikoshi - My Hero Academia, Vigilante, Vol 6.cbz"))
	if series != "My Hero Academia Vigilantes" || index != "6" {
		t.Fatalf("singular folder series = %q index = %q", series, index)
	}
	series, index = seriesFromFilename(filepath.Join(t.TempDir(), "Some Book, Volume 12.epub"))
	if series != "Some Book" || index != "12" {
		t.Fatalf("filename series = %q index = %q", series, index)
	}
}

func TestSearchMatchesSeriesMetadata(t *testing.T) {
	bookshelf := t.TempDir()
	if err := os.WriteFile(filepath.Join(bookshelf, "Author - Book.epub"), []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:series-search-test?mode=memory&cache=shared")
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
	if _, err := db.Exec(`INSERT INTO book_metadata (book_id, publisher, published_at, isbn, language, description, series, series_index, refreshed_at) SELECT id, '', '', '', '', '', 'My Hero Academia Vigilantes', '7', '' FROM books`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/books?q=vigilantes", nil)
	response := httptest.NewRecorder()
	app.books(response, request)
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte("My Hero Academia Vigilantes")) {
		t.Fatalf("series search status = %d body = %s", response.Code, response.Body.String())
	}
}

func writeTestPDF(t *testing.T, info string) string {
	t.Helper()
	return writeTestPDFObjects(t, []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>",
		info,
	}, 1, 4)
}

func writeTestPDFObjects(t *testing.T, objects []string, root, info int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.pdf")
	var buffer bytes.Buffer
	buffer.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, body := range objects {
		offsets[index+1] = buffer.Len()
		fmt.Fprintf(&buffer, "%d 0 obj\n%s\nendobj\n", index+1, body)
	}
	xref := buffer.Len()
	fmt.Fprintf(&buffer, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for index := 1; index <= len(objects); index++ {
		fmt.Fprintf(&buffer, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&buffer, "trailer\n<< /Size %d /Root %d 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, root, info, xref)
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestEPUB(t *testing.T, opf string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.epub")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?><container><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`,
		"content.opf":            opf,
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
	return path
}

func TestPDFMetadataReadsInfoDictionary(t *testing.T) {
	path := writeTestPDF(t, "<< /Title (A PDF Title) /Author (Jane Doe) >>")
	title, author := pdfMetadata(path)
	if title != "A PDF Title" || author != "Jane Doe" {
		t.Fatalf("metadata = %q, %q", title, author)
	}
}

func TestPDFMetadataDecodesUTF16Title(t *testing.T) {
	expected := "What you need to know about dates and times in computing"
	var encoded []byte
	encoded = append(encoded, 0xFE, 0xFF)
	for _, character := range expected {
		encoded = binary.BigEndian.AppendUint16(encoded, uint16(character))
	}
	path := writeTestPDF(t, fmt.Sprintf("<< /Title <%X> >>", encoded))
	title, _ := pdfMetadata(path)
	if title != expected {
		t.Fatalf("decoded title = %q, want %q", title, expected)
	}
}

func TestPDFCoverExtractsFirstPageImage(t *testing.T) {
	directory := t.TempDir()
	imagePath := filepath.Join(directory, "cover.jpg")
	imageFile, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	picture := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			picture.Set(x, y, color.RGBA{R: 200, G: 80, B: 40, A: 255})
		}
	}
	if err := jpeg.Encode(imageFile, picture, nil); err != nil {
		t.Fatal(err)
	}
	if err := imageFile.Close(); err != nil {
		t.Fatal(err)
	}
	pdfPath := filepath.Join(directory, "book.pdf")
	if err := pdfapi.ImportImagesFile([]string{imagePath}, pdfPath, nil, pdfmodel.NewDefaultConfiguration()); err != nil {
		t.Fatal(err)
	}
	cover, err := embeddedPDFCover(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(cover, []byte{0xFF, 0xD8}) {
		t.Fatalf("cover is not a JPEG: %X", cover[:min(len(cover), 8)])
	}
}

func TestPDFMetadataIgnoresBookmarkTitles(t *testing.T) {
	path := writeTestPDFObjects(t, []string{
		"<< /Type /Catalog /Pages 2 0 R /Outlines 5 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>",
		"<< /Title (Correct Title) /Author (Jane Doe) >>",
		"<< /Type /Outlines /First 6 0 R /Last 6 0 R /Count 1 >>",
		"<< /Title (Garbled Bookmark) /Parent 5 0 R >>",
	}, 1, 4)
	title, author := pdfMetadata(path)
	if title != "Correct Title" || author != "Jane Doe" {
		t.Fatalf("metadata = %q, %q", title, author)
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

func TestDeleteCleanupCandidateMovesFileToTrash(t *testing.T) {
	bookshelf := t.TempDir()
	first := filepath.Join(bookshelf, "Author - First.pdf")
	second := filepath.Join(bookshelf, "Author - Second.pdf")
	if err := os.WriteFile(first, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:cleanup-delete-test?mode=memory&cache=shared")
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
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, %v", candidates, err)
	}
	request := httptest.NewRequest("POST", "/api/cleanup/delete", bytes.NewBufferString(fmt.Sprintf(`{"ids":[%d]}`, candidates[0].ID)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.deleteCleanupCandidates(response, request)
	if response.Code != 200 {
		t.Fatalf("delete status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("candidate file still at source: %v", err)
	}
	trashMatches, err := filepath.Glob(filepath.Join(bookshelf, ".book-manager", "trash", "*", "Author - Second.pdf"))
	if err != nil || len(trashMatches) != 1 {
		t.Fatalf("trash matches = %v, %v", trashMatches, err)
	}
}

func newTestApp(t *testing.T, name string) *App {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	app := &App{db: db, bookshelf: t.TempDir()}
	if err := app.initDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestSyncSkipsApplicationDataDirectory(t *testing.T) {
	bookshelf := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bookshelf, appDataDirName, "trash", "run1", "Nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bookshelf, "Real Book.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bookshelf, appDataDirName, "trash", "run1", "Nested", "Ghost.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:skip-data-dir?mode=memory&cache=shared")
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
	if err != nil || result.Indexed != 1 {
		t.Fatalf("sync = %#v, %v", result, err)
	}
	var path string
	if err := db.QueryRow(`SELECT path FROM books`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "Real Book.pdf" {
		t.Fatalf("indexed path = %q", path)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM books WHERE path LIKE ?`, appDataDirName+"/%").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("catalogue still contains %d app data rows", total)
	}
}

func TestTrashListRestoreAndDelete(t *testing.T) {
	app := newTestApp(t, "trash-restore")
	if err := os.MkdirAll(filepath.Join(app.bookshelf, "Series"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(app.bookshelf, "Keep.pdf")
	trashMe := filepath.Join(app.bookshelf, "Series", "Trash Me.pdf")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trashMe, []byte("trash"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := app.db.QueryRow(`SELECT id FROM books WHERE path = ?`, "Series/Trash Me.pdf").Scan(&id); err != nil {
		t.Fatal(err)
	}
	deleted := httptest.NewRecorder()
	app.deleteBooks(deleted, httptest.NewRequest("POST", "/api/books/delete", bytes.NewBufferString(fmt.Sprintf(`{"ids":[%d]}`, id))))
	if deleted.Code != 200 {
		t.Fatalf("delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	if _, err := os.Stat(trashMe); !os.IsNotExist(err) {
		t.Fatalf("file still present after delete: %v", err)
	}

	listed := httptest.NewRecorder()
	app.trashRuns(listed, httptest.NewRequest("GET", "/api/trash", nil))
	if listed.Code != 200 {
		t.Fatalf("trash list status = %d", listed.Code)
	}
	var payload struct {
		Runs []TrashRun `json:"runs"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Runs) != 1 || payload.Runs[0].Files != 1 {
		t.Fatalf("trash runs = %#v", payload.Runs)
	}

	restored := httptest.NewRecorder()
	app.trashRestore(restored, httptest.NewRequest("POST", "/api/trash/restore", bytes.NewBufferString(fmt.Sprintf(`{"runId":%q}`, payload.Runs[0].ID))))
	if restored.Code != 200 {
		t.Fatalf("restore status = %d: %s", restored.Code, restored.Body.String())
	}
	if _, err := os.Stat(trashMe); err != nil {
		t.Fatalf("file not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.bookshelf, appDataDirName, "trash", payload.Runs[0].ID)); !os.IsNotExist(err) {
		t.Fatalf("trash run directory still present: %v", err)
	}
	if _, err := app.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	var restoredPath string
	if err := app.db.QueryRow(`SELECT path FROM books WHERE path = ?`, "Series/Trash Me.pdf").Scan(&restoredPath); err != nil {
		t.Fatalf("restored book not re-indexed: %v", err)
	}

	// Delete another book, then purge the run permanently.
	var secondID int64
	if err := app.db.QueryRow(`SELECT id FROM books WHERE path = ?`, "Keep.pdf").Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	deleted = httptest.NewRecorder()
	app.deleteBooks(deleted, httptest.NewRequest("POST", "/api/books/delete", bytes.NewBufferString(fmt.Sprintf(`{"ids":[%d]}`, secondID))))
	if deleted.Code != 200 {
		t.Fatalf("second delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	listed = httptest.NewRecorder()
	app.trashRuns(listed, httptest.NewRequest("GET", "/api/trash", nil))
	if err := json.NewDecoder(listed.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	runID := payload.Runs[0].ID
	purged := httptest.NewRecorder()
	app.trashDelete(purged, httptest.NewRequest("POST", "/api/trash/delete", bytes.NewBufferString(fmt.Sprintf(`{"runId":%q}`, runID))))
	if purged.Code != 200 {
		t.Fatalf("purge status = %d: %s", purged.Code, purged.Body.String())
	}
	if _, err := os.Stat(filepath.Join(app.bookshelf, appDataDirName, "trash", runID)); !os.IsNotExist(err) {
		t.Fatalf("run not purged: %v", err)
	}
}

func TestTrashRejectsUnsafeRunID(t *testing.T) {
	app := newTestApp(t, "trash-unsafe")
	for _, runID := range []string{"../../etc", "a/b", "", "..", "run;rm -rf /"} {
		request := httptest.NewRequest("POST", "/api/trash/delete", bytes.NewBufferString(fmt.Sprintf(`{"runId":%q}`, runID)))
		response := httptest.NewRecorder()
		app.trashDelete(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("run id %q accepted with status %d", runID, response.Code)
		}
	}
}
