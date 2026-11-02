package app

import (
	"log/slog"
	"net/http"

	"github.com/autohome/auto-agent/internal/auth"
	"github.com/autohome/auto-agent/internal/config"
	"github.com/autohome/auto-agent/internal/platform"
	"github.com/autohome/auto-agent/internal/session"
	"github.com/autohome/auto-agent/internal/space"
	"github.com/autohome/auto-agent/internal/storage"
	"github.com/autohome/auto-agent/internal/store"
)

// App 组装路由与服务。
type App struct {
	cfg        *config.Config
	log        *slog.Logger
	store      store.Store
	auth       *auth.Handler
	spaceSvc   *space.Service
	spaceH     *space.Handler
	sessionSvc *session.Service
	sessionH   *session.Handler
}

// New 构造应用。
func New(cfg *config.Config, st store.Store, fs storage.Storage, log *slog.Logger) (*App, error) {
	authHandler := auth.NewHandler(auth.NewService(st, cfg, log), cfg, log)
	spaceSvc := space.NewService(st, fs, log)
	sessionSvc := session.NewService(st, spaceSvc, log)

	return &App{
		cfg:        cfg,
		log:        log,
		store:      st,
		auth:       authHandler,
		spaceSvc:   spaceSvc,
		spaceH:     space.NewHandler(spaceSvc, authHandler.CurrentUser, log),
		sessionSvc: sessionSvc,
		sessionH:   session.NewHandler(sessionSvc, authHandler.CurrentUser, log),
	}, nil
}

// Handler 返回带中间件的根 handler。前端由 CDN/Nginx 托管并经同源反向代理访问本服务，无需 CORS。
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	a.auth.Register(mux)
	a.spaceH.Register(mux)
	a.sessionH.Register(mux)

	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /{$}", a.index)

	var h http.Handler = mux
	h = platform.AccessLog(a.log)(h)
	h = platform.Recover(a.log)(h)
	h = platform.RequestID(h)
	return h
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	platform.WriteJSON(w, http.StatusOK, map[string]string{
		"service": "auto-agent-api",
		"status":  "ok",
		"web":     a.cfg.WebBaseURL,
	})
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		platform.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "db": "down"})
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "ok"})
}
