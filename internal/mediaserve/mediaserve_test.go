package mediaserve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setupDirs(t *testing.T) (hlsDir, dashDir string) {
	t.Helper()
	root := t.TempDir()
	hlsDir = filepath.Join(root, "hls")
	dashDir = filepath.Join(root, "dash")

	type entry struct {
		dir, code, file, body, ctype string
	}
	files := []entry{
		{hlsDir, "live", "index.m3u8", "#EXTM3U\n", "application/vnd.apple.mpegurl"},
		{hlsDir, "live", "seg0.ts", "TSDATA", "video/mp2t"},
		{dashDir, "live", "index.mpd", "<MPD/>", "application/dash+xml"},
		{dashDir, "live", "init.m4s", "M4S", "video/mp4"},
		{dashDir, "live", "init.mp4", "MP4", "video/mp4"},
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Join(f.dir, f.code), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, f.code, f.file), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return hlsDir, dashDir
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestServeHLSManifest(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/index.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("content-type=%s", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache-control=%s", got)
	}
	if got := w.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("pragma=%s", got)
	}
	if w.Body.String() != "#EXTM3U\n" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestServeDASHManifest(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/index.mpd")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/dash+xml" {
		t.Errorf("content-type=%s", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store, max-age=0, must-revalidate" {
		t.Errorf("cache-control=%s", got)
	}
}

func TestServeNestedTSSegment(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/seg0.ts")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.String() != "TSDATA" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestServeNestedM4SSegment(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/init.m4s")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.String() != "M4S" {
		t.Errorf("body=%q", w.Body.String())
	}
}

func TestServeNestedMP4Segment(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/init.mp4")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestNestedRejectsUnknownExtension(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/file.bin")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown ext, got %d", w.Code)
	}
}

func TestNestedRejectsTraversal(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/../../etc/passwd")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for traversal, got %d", w.Code)
	}
}

func TestNestedManifestSetsCorrectCacheControl(t *testing.T) {
	hls, dash := setupDirs(t)
	if err := os.WriteFile(filepath.Join(hls, "live", "alt.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dash, "live", "alt.mpd"), []byte("<MPD/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/alt.m3u8")
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("m3u8 cache-control=%s", got)
	}

	w = doGet(t, h, "/live/alt.mpd")
	if got := w.Header().Get("Cache-Control"); got != "no-store, max-age=0, must-revalidate" {
		t.Errorf("mpd cache-control=%s", got)
	}
}

func TestNestedMissingFileReturns404(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	w := doGet(t, h, "/live/nope.ts")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestNestedRejectsEmptySuffix(t *testing.T) {
	hls, dash := setupDirs(t)
	h := NewHandler(hls, dash)

	// /live/ — no nested file
	w := doGet(t, h, "/live/")
	// chi may route this as suffix="" → 404 from our handler, or no match (404)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for empty suffix, got %d", w.Code)
	}
}
