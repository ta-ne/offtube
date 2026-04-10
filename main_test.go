package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	cfg = loadConfig()
	os.Exit(m.Run())
}

// ---- metadata round-trip ----

func TestWriteReadMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// writeMetadata uses "data/<id>/" relative path, so we chdir into temp dir.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	want := &Metadata{
		ID:          "abc123",
		Title:       "Test Video",
		Uploader:    "Some Channel",
		URL:         "https://example.com/watch?v=abc123",
		Type:        MediaVideo,
		SubmittedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Status:      StatusDone,
		Liked:       1,
		Description: "Line one\nLine two\nLine three",
	}

	if err := writeMetadata(want); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}

	got, err := readMetadataTxt(filepath.Join("data", "abc123", "metadata.txt"))
	if err != nil {
		t.Fatalf("readMetadataTxt: %v", err)
	}

	if got.ID != want.ID {
		t.Errorf("ID: got %q want %q", got.ID, want.ID)
	}
	if got.Title != want.Title {
		t.Errorf("Title: got %q want %q", got.Title, want.Title)
	}
	if got.Uploader != want.Uploader {
		t.Errorf("Uploader: got %q want %q", got.Uploader, want.Uploader)
	}
	if got.URL != want.URL {
		t.Errorf("URL: got %q want %q", got.URL, want.URL)
	}
	if got.Type != want.Type {
		t.Errorf("Type: got %q want %q", got.Type, want.Type)
	}
	if !got.SubmittedAt.Equal(want.SubmittedAt) {
		t.Errorf("SubmittedAt: got %v want %v", got.SubmittedAt, want.SubmittedAt)
	}
	if got.Status != want.Status {
		t.Errorf("Status: got %q want %q", got.Status, want.Status)
	}
	if got.Liked != want.Liked {
		t.Errorf("Liked: got %d want %d", got.Liked, want.Liked)
	}
	if got.Description != want.Description {
		t.Errorf("Description: got %q want %q", got.Description, want.Description)
	}
}

func TestWriteReadMetadataAudio(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	m := &Metadata{
		ID:     "aud1",
		Type:   MediaAudio,
		Status: StatusError,
		Liked:  -1,
	}
	if err := writeMetadata(m); err != nil {
		t.Fatal(err)
	}
	got, err := readMetadataTxt(filepath.Join("data", "aud1", "metadata.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != MediaAudio {
		t.Errorf("Type: got %q want %q", got.Type, MediaAudio)
	}
	if got.Status != StatusError {
		t.Errorf("Status: got %q want %q", got.Status, StatusError)
	}
	if got.Liked != -1 {
		t.Errorf("Liked: got %d want -1", got.Liked)
	}
}

// ---- handleFile path sanitization ----

func TestHandleFileRejectsPathTraversal(t *testing.T) {
	app := &App{items: make(map[string]*Metadata), jobQueue: make(chan *Metadata, 1)}

	cases := []string{
		"../secret.txt",
		"../../etc/passwd",
		"sub/dir/file.txt",
	}

	for _, filename := range cases {
		req := httptest.NewRequest(http.MethodGet, "/media/id1/file/"+filename, nil)
		req.SetPathValue("id", "id1")
		req.SetPathValue("filename", filename)
		w := httptest.NewRecorder()
		app.handleFile(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("filename %q: expected 400, got %d", filename, w.Code)
		}
	}
}

func TestHandleFileAllowsSafeFilename(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Create data/id1/video.mp4
	os.MkdirAll(filepath.Join("data", "id1"), 0755)
	os.WriteFile(filepath.Join("data", "id1", "video.mp4"), []byte("fake"), 0644)

	app := &App{items: make(map[string]*Metadata), jobQueue: make(chan *Metadata, 1)}

	req := httptest.NewRequest(http.MethodGet, "/media/id1/file/video.mp4", nil)
	req.SetPathValue("id", "id1")
	req.SetPathValue("filename", "video.mp4")
	w := httptest.NewRecorder()
	app.handleFile(w, req)
	if w.Code == http.StatusBadRequest {
		t.Errorf("safe filename rejected with 400")
	}
}

// ---- runCleanup ----

func TestRunCleanupRemovesDisliked(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Create two items: one disliked, one neutral.
	disliked := &Metadata{ID: "disliked1", Title: "Bad", Liked: -1, Status: StatusDone}
	neutral := &Metadata{ID: "neutral1", Title: "OK", Liked: 0, Status: StatusDone}

	for _, m := range []*Metadata{disliked, neutral} {
		if err := writeMetadata(m); err != nil {
			t.Fatal(err)
		}
	}

	app := &App{
		items:    map[string]*Metadata{disliked.ID: disliked, neutral.ID: neutral},
		jobQueue: make(chan *Metadata, 1),
	}

	app.runCleanup()

	if _, ok := app.items[disliked.ID]; ok {
		t.Error("disliked item should have been removed from in-memory map")
	}
	if _, ok := app.items[neutral.ID]; !ok {
		t.Error("neutral item should still be in map")
	}
	if _, err := os.Stat(filepath.Join("data", disliked.ID)); !os.IsNotExist(err) {
		t.Error("disliked item directory should have been removed from disk")
	}
	if _, err := os.Stat(filepath.Join("data", neutral.ID)); err != nil {
		t.Error("neutral item directory should still exist on disk")
	}
}

// ---- status constants ----

func TestStatusValues(t *testing.T) {
	// Ensure status string values match what the templates expect.
	cases := map[Status]string{
		StatusQueued:      "queued",
		StatusRunning:     "running",
		StatusDone:        "done",
		StatusError:       "error",
		StatusInterrupted: "interrupted",
	}
	for s, want := range cases {
		if string(s) != want {
			t.Errorf("Status %v: got %q want %q", s, string(s), want)
		}
	}
}

// ---- metadataPath helper ----

func TestMetadataPath(t *testing.T) {
	got := metadataPath("abc")
	want := filepath.Join("data", "abc", "metadata.txt")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// ---- description newline escaping ----

func TestDescriptionNewlineEscaping(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	m := &Metadata{
		ID:          "desc1",
		Description: "first\nsecond\nthird",
	}
	if err := writeMetadata(m); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join("data", "desc1", "metadata.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The raw file must not contain literal newlines inside the description line.
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "description=") {
			if strings.Contains(line[len("description="):], "\n") {
				t.Error("description value contains literal newline in metadata.txt")
			}
			if !strings.Contains(line, `\n`) {
				t.Error("description value missing escaped newline in metadata.txt")
			}
		}
	}

	got, err := readMetadataTxt(filepath.Join("data", "desc1", "metadata.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != m.Description {
		t.Errorf("description round-trip: got %q want %q", got.Description, m.Description)
	}
}
