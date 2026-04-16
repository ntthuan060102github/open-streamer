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
	"github.com/go-chi/cors"
	_ "github.com/ntt0601zcoder/open-streamer/api/docs" // swag Register(SwaggerInfo)
	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/api/handler"
	"github.com/ntt0601zcoder/open-streamer/internal/mediaserve"
	"github.com/samber/do/v2"
)

// Server is the HTTP API server.
type Server struct {
	hlsDir  string
	dashDir string

	// Handler references stored for router rebuilding on config change.
	streamH    *handler.StreamHandler
	recordingH *handler.RecordingHandler
	hookH      *handler.HookHandler
	configH    *handler.ConfigHandler

	router *chi.Mux
	http   *http.Server
}

// New creates a Server and registers it with the DI injector.
// The server is constructed but not started — call StartWithConfig to begin accepting connections.
func New(i do.Injector) (*Server, error) {
	pub := do.MustInvoke[config.PublisherConfig](i)

	dashDir := strings.TrimSpace(pub.DASH.Dir)
	if dashDir == "" {
		dashDir = "./dash"
	}

	s := &Server{
		hlsDir:     pub.HLS.Dir,
		dashDir:    dashDir,
		streamH:    do.MustInvoke[*handler.StreamHandler](i),
		recordingH: do.MustInvoke[*handler.RecordingHandler](i),
		hookH:      do.MustInvoke[*handler.HookHandler](i),
		configH:    do.MustInvoke[*handler.ConfigHandler](i),
	}
	return s, nil
}

func (s *Server) buildRouter(serverCfg *config.ServerConfig) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	if serverCfg.CORS.Enabled {
		r.Use(cors.Handler(corsOptions(serverCfg.CORS)))
	}
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(middleware.Timeout(120 * time.Second))

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz)
	r.Get("/config", s.configH.GetConfig)
	r.Post("/config", s.configH.UpdateConfig)

	r.Get("/swagger/doc.json", serveSwaggerJSON)
	r.Get("/swagger", http.RedirectHandler("/swagger/", http.StatusMovedPermanently).ServeHTTP)
	r.Get("/swagger/", serveSwaggerIndex)

	mediaserve.Mount(r, s.hlsDir, s.dashDir)

	r.Route("/streams", func(r chi.Router) {
		r.Get("/", s.streamH.List)
		r.Route("/{code}", func(r chi.Router) {
			r.Get("/", s.streamH.Get)
			r.Post("/", s.streamH.Put)
			r.Delete("/", s.streamH.Delete)
			r.Post("/restart", s.streamH.Restart)
			r.Post("/inputs/switch", s.streamH.SwitchInput)

			r.Get("/recordings", s.recordingH.ListByStream)
		})
	})

	r.Route("/recordings/{rid}", func(r chi.Router) {
		r.Get("/", s.recordingH.Get)
		r.Get("/info", s.recordingH.Info)
		r.Get("/playlist.m3u8", s.recordingH.Playlist)
		r.Get("/timeshift.m3u8", s.recordingH.Timeshift)
		r.Get("/{file}", s.recordingH.ServeSegment)
	})

	r.Route("/hooks", func(r chi.Router) {
		r.Get("/", s.hookH.List)
		r.Post("/", s.hookH.Create)
		r.Route("/{hid}", func(r chi.Router) {
			r.Get("/", s.hookH.Get)
			r.Put("/", s.hookH.Update)
			r.Delete("/", s.hookH.Delete)
			r.Post("/test", s.hookH.Test)
		})
	})

	return r
}

func corsOptions(cfg config.CORSConfig) cors.Options {
	origins := cfg.AllowedOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
	}
	headers := cfg.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"Accept", "Authorization", "Content-Type", "X-Request-ID"}
	}
	allowCredentials := cfg.AllowCredentials
	if allowCredentials {
		for _, o := range origins {
			if o == "*" {
				allowCredentials = false
				break
			}
		}
	}
	opts := cors.Options{
		AllowedOrigins:   origins,
		AllowedMethods:   methods,
		AllowedHeaders:   headers,
		ExposedHeaders:   cfg.ExposedHeaders,
		AllowCredentials: allowCredentials,
	}
	if cfg.MaxAge > 0 {
		opts.MaxAge = cfg.MaxAge
	}
	return opts
}

// StartWithConfig builds the router from the given ServerConfig and begins accepting
// HTTP connections. Blocks until ctx is cancelled.
// Called by the RuntimeManager each time the HTTP service is (re)started.
func (s *Server) StartWithConfig(ctx context.Context, cfg *config.ServerConfig) error {
	s.router = s.buildRouter(cfg)
	s.http = &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: s.router,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutCtx); err != nil {
			slog.Warn("api: HTTP shutdown", "err", err)
		}
	}()

	slog.Info("api: HTTP server listening", "addr", cfg.HTTPAddr)
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
func readyz(w http.ResponseWriter, r *http.Request) {
	healthz(w, r)
}
