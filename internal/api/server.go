// Package api implements the HTTP API server.
// All routes follow the REST conventions defined in api-conventions.mdc.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/ntthuan060102github/open-streamer/api/docs" // swag Register(SwaggerInfo)
	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/api/handler"
	"github.com/ntthuan060102github/open-streamer/internal/mediaserve"
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

	dashDir := strings.TrimSpace(cfg.Publisher.DASH.Dir)
	if dashDir == "" {
		dashDir = "./dash"
	}

	s := &Server{
		cfg:     cfg.Server,
		hlsDir:  cfg.Publisher.HLS.Dir,
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
	mediaserve.Mount(r, s.hlsDir, s.dashDir)

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
		r.Get("/info", recording.Info)
		r.Get("/playlist.m3u8", recording.Playlist)
		r.Get("/timeshift.m3u8", recording.Timeshift)
		r.Get("/{file}", recording.ServeSegment)
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
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutCtx); err != nil {
			slog.Warn("api: HTTP shutdown", "err", err)
		}
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
// @Router /healthz [get].
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyz readiness probe.
// @Summary Readiness
// @Description Returns 200 when the server accepts traffic (basic check).
// @Tags system
// @Success 200 {string} string "ok"
// @Router /readyz [get].
func readyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

