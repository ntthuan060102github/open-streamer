// Package api implements the HTTP API server.
// All routes follow the REST conventions defined in api-conventions.mdc.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/open-streamer/open-streamer/api/docs" // swag Register(SwaggerInfo)
	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/api/handler"
	"github.com/samber/do/v2"
)

// Server is the HTTP API server.
type Server struct {
	cfg     config.ServerConfig
	hlsDir  string
	dashDir string
	router  *chi.Mux
	http    *http.Server
}

// New creates a Server and registers it with the DI injector.
func New(i do.Injector) (*Server, error) {
	cfg := do.MustInvoke[*config.Config](i)
	streamHandler := do.MustInvoke[*handler.StreamHandler](i)
	recordingHandler := do.MustInvoke[*handler.RecordingHandler](i)
	hookHandler := do.MustInvoke[*handler.HookHandler](i)

	dashDir := cfg.Publisher.DASHDir
	if strings.TrimSpace(dashDir) == "" {
		dashDir = cfg.Publisher.HLSDir
	}

	s := &Server{
		cfg:     cfg.Server,
		hlsDir:  cfg.Publisher.HLSDir,
		dashDir: dashDir,
	}
	s.router = s.buildRouter(streamHandler, recordingHandler, hookHandler)
	s.http = &http.Server{
		Addr:    cfg.Server.HTTPAddr,
		Handler: s.router,
	}
	return s, nil
}

func (s *Server) buildRouter(
	stream *handler.StreamHandler,
	recording *handler.RecordingHandler,
	hook *handler.HookHandler,
) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(middleware.Timeout(120 * time.Second))

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz)

	r.Get("/swagger/doc.json", serveSwaggerJSON)
	r.Get("/swagger", http.RedirectHandler("/swagger/", http.StatusMovedPermanently).ServeHTTP)
	r.Get("/swagger/", serveSwaggerIndex)
	r.Get("/{code}/index.m3u8", s.serveManifest("index.m3u8", "application/vnd.apple.mpegurl", s.hlsDir))
	r.Get("/{code}/index.mpd", s.serveManifest("index.mpd", "application/dash+xml", s.dashDir))
	r.Get("/{code}/{asset}", s.serveStreamAsset())

	r.Route("/streams", func(r chi.Router) {
		r.Get("/", stream.List)
		r.Route("/{code}", func(r chi.Router) {
			r.Get("/", stream.Get)
			r.Put("/", stream.Put)
			r.Delete("/", stream.Delete)
			r.Post("/start", stream.Start)
			r.Post("/stop", stream.Stop)
			r.Get("/status", stream.Status)

			r.Post("/recordings/start", recording.Start)
			r.Post("/recordings/stop", recording.Stop)
			r.Get("/recordings", recording.ListByStream)
		})
	})

	r.Route("/recordings/{rid}", func(r chi.Router) {
		r.Get("/", recording.Get)
		r.Delete("/", recording.Delete)
		r.Get("/playlist.m3u8", recording.Playlist)
	})

	r.Route("/hooks", func(r chi.Router) {
		r.Get("/", hook.List)
		r.Post("/", hook.Create)
		r.Route("/{hid}", func(r chi.Router) {
			r.Get("/", hook.Get)
			r.Put("/", hook.Update)
			r.Delete("/", hook.Delete)
			r.Post("/test", hook.Test)
		})
	})

	return r
}

// Start begins accepting HTTP connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
	}()

	slog.Info("api: HTTP server listening", "addr", s.cfg.HTTPAddr)
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("api: server: %w", err)
	}
	return nil
}

// healthz liveness probe.
// @Summary Liveness
// @Description Returns 200 if the process is up.
// @Tags system
// @Success 200 {string} string "ok"
// @Router /healthz [get]
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyz readiness probe.
// @Summary Readiness
// @Description Returns 200 when the server accepts traffic (basic check).
// @Tags system
// @Success 200 {string} string "ok"
// @Router /readyz [get]
func readyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) serveManifest(filename, contentType, rootDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		if code == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		http.ServeFile(w, r, filepath.Join(rootDir, code, filename))
	}
}

func (s *Server) serveStreamAsset() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := chi.URLParam(r, "code")
		asset := chi.URLParam(r, "asset")
		if code == "" || asset == "" {
			http.NotFound(w, r)
			return
		}
		if asset != filepath.Base(asset) {
			http.NotFound(w, r)
			return
		}
		ext := strings.ToLower(filepath.Ext(asset))
		baseDir := ""
		switch ext {
		case ".ts", ".m3u8":
			baseDir = s.hlsDir
		case ".m4s", ".mpd":
			baseDir = s.dashDir
		default:
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(baseDir, code, asset))
	}
}
