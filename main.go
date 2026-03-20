package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

type MediaType string
type Status string

type config struct {
	dataDir             string
	maxExecutors        int
	jobQueueSize        int
	ytdlpAutoUpdate     bool
	ytdlpUpdateInterval time.Duration
	pageSize            int
	cleanupEnabled      bool
	cleanupInterval     time.Duration
}

var cfg config

func loadConfig() config {
	return config{
		dataDir:             envString("DATA_DIR", "data"),
		maxExecutors:        envInt("MAX_EXECUTORS", 1),
		jobQueueSize:        envInt("JOB_QUEUE_SIZE", 100),
		ytdlpAutoUpdate:     envBool("YTDLP_AUTO_UPDATE", true),
		ytdlpUpdateInterval: envDurationHours("YTDLP_UPDATE_INTERVAL_HOURS", 24),
		pageSize:            envInt("PAGE_SIZE", 20),
		cleanupEnabled:      envBool("CLEANUP_ENABLED", false),
		cleanupInterval:     envDurationHours("CLEANUP_INTERVAL_HOURS", 24),
	}
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s=%q, using default %d", key, v, def)
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		log.Printf("invalid %s=%q, using default %v", key, v, def)
	}
	return def
}

func envDurationHours(key string, defHours int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Hour
		}
		log.Printf("invalid %s=%q, using default %d", key, v, defHours)
	}
	return time.Duration(defHours) * time.Hour
}

const (
	MediaVideo MediaType = "video"
	MediaAudio MediaType = "audio"

	StatusQueued      Status = "queued"      // waiting for a free executor
	StatusRunning     Status = "running"     // yt-dlp process active
	StatusDone        Status = "done"        // finished successfully
	StatusError       Status = "error"       // yt-dlp exited non-zero
	StatusInterrupted Status = "interrupted" // app restarted while job was queued/running

)

type Metadata struct {
	ID          string
	Title       string
	Uploader    string
	URL         string
	Type        MediaType
	SubmittedAt time.Time
	Status      Status
	Liked       int    // 1=liked, -1=disliked, 0=neutral
	Description string // YouTube video description
}

type App struct {
	mu       sync.RWMutex
	items    map[string]*Metadata
	tmpl     *template.Template
	jobQueue chan *Metadata
}

func newApp() (*App, error) {
	funcMap := template.FuncMap{
		"thumbnail": func(m *Metadata) string {
			if m.Type == MediaVideo {
				return fmt.Sprintf("/media/%s/file/video.jpg", m.ID)
			}
			return fmt.Sprintf("/media/%s/file/audio.jpg", m.ID)
		},
	}

	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	app := &App{
		items:    make(map[string]*Metadata),
		tmpl:     tmpl,
		jobQueue: make(chan *Metadata, cfg.jobQueueSize),
	}

	if err := app.loadExisting(); err != nil {
		log.Printf("warning: loading existing data: %v", err)
	}

	for range cfg.maxExecutors {
		go app.worker()
	}

	return app, nil
}

func (a *App) worker() {
	for m := range a.jobQueue {
		a.download(m)
	}
}

// metadataPath returns the txt path for a media ID.
func metadataPath(id string) string {
	return filepath.Join(cfg.dataDir, id, "metadata.txt")
}

// writeMetadata serialises m to metadata.txt.
// Format: one "key=value" line per field; newlines in Description are escaped as \n.
func writeMetadata(m *Metadata) error {
	dir := filepath.Join(cfg.dataDir, m.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("id=" + m.ID + "\n")
	sb.WriteString("title=" + m.Title + "\n")
	sb.WriteString("uploader=" + m.Uploader + "\n")
	sb.WriteString("url=" + m.URL + "\n")
	sb.WriteString("type=" + string(m.Type) + "\n")
	sb.WriteString("submitted_at=" + m.SubmittedAt.UTC().Format(time.RFC3339) + "\n")
	sb.WriteString("status=" + string(m.Status) + "\n")
	sb.WriteString("liked=" + strconv.Itoa(m.Liked) + "\n")
	sb.WriteString("description=" + strings.ReplaceAll(m.Description, "\n", `\n`) + "\n")
	return os.WriteFile(metadataPath(m.ID), []byte(sb.String()), 0644)
}

// readMetadataTxt parses a metadata.txt file.
func readMetadataTxt(path string) (*Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := &Metadata{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key, val := line[:idx], line[idx+1:]
		switch key {
		case "id":
			m.ID = val
		case "title":
			m.Title = val
		case "uploader":
			m.Uploader = val
		case "url":
			m.URL = val
		case "type":
			m.Type = MediaType(val)
		case "submitted_at":
			m.SubmittedAt, _ = time.Parse(time.RFC3339, val)
		case "status":
			m.Status = Status(val)
		case "liked":
			m.Liked, _ = strconv.Atoi(val)
		case "description":
			m.Description = strings.ReplaceAll(val, `\n`, "\n")
		}
	}
	return m, scanner.Err()
}

// readMetadataJSON reads a legacy metadata.json file.
func readMetadataJSON(path string) (*Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		ID          string    `json:"id"`
		Title       string    `json:"title"`
		Uploader    string    `json:"uploader"`
		URL         string    `json:"url"`
		Type        MediaType `json:"type"`
		SubmittedAt time.Time `json:"submitted_at"`
		Status      Status    `json:"status"`
		Liked       int       `json:"liked"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return &Metadata{
		ID: raw.ID, Title: raw.Title, Uploader: raw.Uploader,
		URL: raw.URL, Type: raw.Type, SubmittedAt: raw.SubmittedAt,
		Status: raw.Status, Liked: raw.Liked,
	}, nil
}

func (a *App) loadExisting() error {
	entries, err := os.ReadDir(cfg.dataDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(cfg.dataDir, entry.Name())
		txtPath := filepath.Join(dir, "metadata.txt")
		jsonPath := filepath.Join(dir, "metadata.json")

		var m *Metadata
		if _, err := os.Stat(txtPath); err == nil {
			m, err = readMetadataTxt(txtPath)
			if err != nil {
				log.Printf("load %s: %v", txtPath, err)
				continue
			}
		} else if _, err := os.Stat(jsonPath); err == nil {
			// Migrate legacy JSON entry to txt format.
			m, err = readMetadataJSON(jsonPath)
			if err != nil {
				log.Printf("load %s: %v", jsonPath, err)
				continue
			}
			if err := writeMetadata(m); err != nil {
				log.Printf("migrate %s: %v", jsonPath, err)
				continue
			}
			if err := os.Remove(jsonPath); err != nil {
				log.Printf("remove legacy %s: %v", jsonPath, err)
			}
			log.Printf("migrated %s → metadata.txt", jsonPath)
		} else {
			continue
		}

		// Jobs that were in-flight when the app stopped are marked interrupted.
		// They can be retried manually via the UI.
		if m.Status == StatusRunning || m.Status == StatusQueued {
			m.Status = StatusInterrupted
			if err := writeMetadata(m); err != nil {
				log.Printf("mark interrupted %s: %v", m.ID, err)
			}
		}
		a.items[m.ID] = m
	}
	return nil
}

func (a *App) setStatus(id string, status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.items[id]; ok {
		m.Status = status
		if err := writeMetadata(m); err != nil {
			log.Printf("setStatus %s → %s: %v", id, status, err)
		}
	}
}

type indexData struct {
	Items    []*Metadata
	PageSize int
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	items := make([]*Metadata, 0, len(a.items))
	for _, m := range a.items {
		items = append(items, m)
	}
	a.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		return items[i].SubmittedAt.After(items[j].SubmittedAt)
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "list.html", indexData{Items: items, PageSize: cfg.pageSize}); err != nil {
		log.Printf("execute list.html: %v", err)
	}
}

func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	a.mu.RLock()
	m, ok := a.items[id]
	a.mu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	logPath := filepath.Join(cfg.dataDir, id, "log.txt")
	logBytes, _ := os.ReadFile(logPath)

	type PageData struct {
		Media *Metadata
		Log   string
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "media.html", PageData{Media: m, Log: string(logBytes)}); err != nil {
		log.Printf("execute media.html: %v", err)
	}
}

// submitURL fetches metadata via yt-dlp and enqueues a download.
// Returns (id, duplicate, error). On duplicate the download is NOT started again.
func (a *App) submitURL(url string, mediaType MediaType) (string, bool, error) {
	cmd := exec.Command("yt-dlp", "--dump-json", "--no-download", url)
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("yt-dlp metadata: %w", err)
	}
	var ytMeta struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Uploader    string `json:"uploader"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &ytMeta); err != nil {
		return "", false, fmt.Errorf("parse yt-dlp output: %w", err)
	}

	a.mu.Lock()
	if existing, ok := a.items[ytMeta.ID]; ok {
		a.mu.Unlock()
		return existing.ID, true, nil
	}
	m := &Metadata{
		ID:          ytMeta.ID,
		Title:       ytMeta.Title,
		Uploader:    ytMeta.Uploader,
		Description: ytMeta.Description,
		URL:         url,
		Type:        mediaType,
		SubmittedAt: time.Now(),
		Status:      StatusQueued,
	}
	a.items[m.ID] = m
	a.mu.Unlock()

	if err := writeMetadata(m); err != nil {
		a.mu.Lock()
		delete(a.items, m.ID)
		a.mu.Unlock()
		return "", false, fmt.Errorf("write metadata: %w", err)
	}
	a.jobQueue <- m
	return m.ID, false, nil
}

type submitRequest struct {
	URL  string    `json:"url"`
	Type MediaType `json:"type"`
}

func (a *App) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if req.Type != MediaVideo && req.Type != MediaAudio {
		req.Type = MediaVideo
	}

	id, _, err := a.submitURL(req.URL, req.Type)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (a *App) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	a.mu.Lock()
	m, ok := a.items[id]
	if !ok {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if m.Status != StatusError && m.Status != StatusInterrupted {
		a.mu.Unlock()
		http.Error(w, "can only retry failed or interrupted downloads", http.StatusConflict)
		return
	}
	m.Status = StatusQueued
	if err := writeMetadata(m); err != nil {
		log.Printf("retry writeMetadata %s: %v", id, err)
	}
	a.mu.Unlock()

	a.jobQueue <- m

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "queued"})
}

// miniflux webhook support

type minifluxEntry struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type minifluxPayload struct {
	EventType string          `json:"event_type"`
	Entry     *minifluxEntry  `json:"entry"`
	Entries   []minifluxEntry `json:"entries"`
}

func (a *App) handleMinifluxWebhook(mediaType MediaType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload minifluxPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		var entries []minifluxEntry
		switch payload.EventType {
		// process only save_entry events
		case "save_entry":
			if payload.Entry != nil {
				entries = []minifluxEntry{*payload.Entry}
			}
		default:
			log.Printf("miniflux webhook: unknown event_type %q, ignoring", payload.EventType)
			w.WriteHeader(http.StatusOK)
			return
		}

		type result struct {
			URL       string `json:"url"`
			ID        string `json:"id,omitempty"`
			Duplicate bool   `json:"duplicate,omitempty"`
			Error     string `json:"error,omitempty"`
		}
		results := make([]result, 0, len(entries))

		for _, entry := range entries {
			if entry.URL == "" {
				continue
			}
			id, duplicate, err := a.submitURL(entry.URL, mediaType)
			if err != nil {
				log.Printf("miniflux webhook: submit %q: %v", entry.URL, err)
				results = append(results, result{URL: entry.URL, Error: err.Error()})
				continue
			}
			if duplicate {
				log.Printf("miniflux webhook: duplicate url=%q id=%s, skipping", entry.URL, id)
			}
			results = append(results, result{URL: entry.URL, ID: id, Duplicate: duplicate})
		}

		w.WriteHeader(http.StatusOK)
	}
}

func (a *App) download(m *Metadata) {
	a.setStatus(m.ID, StatusRunning)

	dir := filepath.Join(cfg.dataDir, m.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("download mkdir %s: %v", dir, err)
		a.setStatus(m.ID, StatusError)
		return
	}

	logPath := filepath.Join(dir, "log.txt")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("open log file %s: %v", logPath, err)
		a.setStatus(m.ID, StatusError)
		return
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n--- started %s ---\n", time.Now().Format(time.RFC3339))

	var args []string
	if m.Type == MediaVideo {
		args = []string{
			// best video + best audio, merged into mp4
			"--format", "bestvideo[ext=mp4]+bestaudio[ext=m4a]/bestvideo+bestaudio/best",
			"--output", fmt.Sprintf("data/%s/video.%%(ext)s", m.ID),
			"--merge-output-format", "mp4",
			"--write-thumbnail",
			"--convert-thumbnails", "jpg",
			"--newline",
			m.URL,
		}
	} else {
		args = []string{
			// best audio quality, converted to mp3
			"--format", "bestaudio/best",
			"--output", fmt.Sprintf("data/%s/audio.%%(ext)s", m.ID),
			"--extract-audio",
			"--audio-format", "mp3",
			"--audio-quality", "0",
			"--embed-thumbnail",
			"--add-metadata",
			"--replace-in-metadata", "meta_artist", ".+", "YouTube",
			"--write-thumbnail",
			"--convert-thumbnails", "jpg",
			"--newline",
			m.URL,
		}
	}

	cmd := exec.Command("yt-dlp", args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(logFile, "stdout pipe error: %v\n", err)
		a.setStatus(m.ID, StatusError)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(logFile, "stderr pipe error: %v\n", err)
		a.setStatus(m.ID, StatusError)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logFile, "start error: %v\n", err)
		a.setStatus(m.ID, StatusError)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(logFile, os.Stdout), stdoutPipe)
	}()
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(logFile, os.Stderr), stderrPipe)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(logFile, "\nDownload error: %v\n", err)
		a.setStatus(m.ID, StatusError)
		return
	}

	a.setStatus(m.ID, StatusDone)
}

type likeRequest struct {
	Value int `json:"value"`
}

func (a *App) handleLike(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req likeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Value < -1 || req.Value > 1 {
		http.Error(w, "value must be -1, 0, or 1", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	m, ok := a.items[id]
	if ok {
		m.Liked = req.Value
		if err := writeMetadata(m); err != nil {
			log.Printf("like writeMetadata %s: %v", id, err)
		}
	}
	a.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"liked": req.Value})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	a.mu.RLock()
	m, ok := a.items[id]
	a.mu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	logPath := filepath.Join(cfg.dataDir, id, "log.txt")
	logBytes, _ := os.ReadFile(logPath)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": string(m.Status),
		"log":    string(logBytes),
	})
}

func (a *App) handleFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	filename := r.PathValue("filename")

	// Sanitize: reject anything that would escape the media directory.
	clean := filepath.Clean(filename)
	if clean != filepath.Base(clean) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	path := filepath.Join(cfg.dataDir, id, clean)
	http.ServeFile(w, r, path)
}

func updateYtDLP() {
	cmd := exec.Command("yt-dlp", "-U")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("yt-dlp -U: %v\n%s", err, out)
		return
	}
	log.Printf("yt-dlp -U: %s", strings.TrimSpace(string(out)))
}

func startYtDLPUpdater() {
	go func() {
		updateYtDLP()
		for range time.Tick(cfg.ytdlpUpdateInterval) {
			updateYtDLP()
		}
	}()
}

func (a *App) startCleanup() {
	go func() {
		for {
			time.Sleep(cfg.cleanupInterval)
			a.runCleanup()
		}
	}()
}

func (a *App) runCleanup() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for id, m := range a.items {
		if m.Liked != -1 {
			continue
		}
		dir := filepath.Join(cfg.dataDir, id)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("cleanup remove %s: %v", dir, err)
			continue
		}
		delete(a.items, id)
		log.Printf("cleanup: removed disliked media %s (%q)", id, m.Title)
	}
}

func main() {
	cfg = loadConfig()

	if err := os.MkdirAll(cfg.dataDir, 0755); err != nil {
		log.Fatalf("create data directory: %v", err)
	}

	app, err := newApp()
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	if cfg.ytdlpAutoUpdate {
		startYtDLPUpdater()
	}
	if cfg.cleanupEnabled {
		app.startCleanup()
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", app.handleIndex)
	mux.HandleFunc("GET /media/{id}", app.handleMedia)
	mux.HandleFunc("POST /api/submit", app.handleSubmit)
	mux.HandleFunc("POST /api/retry/{id}", app.handleRetry)
	mux.HandleFunc("POST /api/like/{id}", app.handleLike)
	mux.HandleFunc("GET /api/status/{id}", app.handleStatus)
	mux.HandleFunc("GET /media/{id}/file/{filename}", app.handleFile)
	mux.HandleFunc("POST /api/webhook/miniflux/video", app.handleMinifluxWebhook(MediaVideo))
	mux.HandleFunc("POST /api/webhook/miniflux/audio", app.handleMinifluxWebhook(MediaAudio))

	staticHandler := http.FileServer(http.FS(staticFS))
	mux.Handle("GET /static/", staticHandler)

	log.Printf("offtube listening on :8080")
	if err := http.ListenAndServe("0.0.0.0:8080", mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
