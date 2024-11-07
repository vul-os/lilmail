// handlers/handler.go
package handlers

import (
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"lilmail/internal/auth"
	"lilmail/internal/cache"
	"lilmail/internal/crypto"
	"lilmail/internal/email"
)

type Handler struct {
	auth     *auth.Manager
	email    *email.Client
	cache    *cache.FileCache
	crypto   *crypto.Manager
	sessions map[string]*email.Client // Map session IDs to email clients
}

func NewHandler(auth *auth.Manager, cache *cache.FileCache, crypto *crypto.Manager) *Handler {
	return &Handler{
		auth:     auth,
		cache:    cache,
		crypto:   crypto,
		sessions: make(map[string]*email.Client),
	}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Timeout(30 * time.Second))

	// Public routes
	r.Group(func(r chi.Router) {
		r.Get("/login", h.handleLoginPage)
		r.Post("/login", h.handleLogin)
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(h.authMiddleware)

		r.Get("/folders", h.handleGetFolders)
		r.Route("/folder/{folder}", func(r chi.Router) {
			r.Get("/", h.handleGetMessages)
			r.Get("/message/{uid}", h.handleGetMessage)
			r.Delete("/message/{uid}", h.handleDeleteMessage)
			r.Post("/message/{uid}/move", h.handleMoveMessage)
			r.Post("/message/{uid}/flag", h.handleFlagMessage)
		})
		r.Get("/inbox", h.HandleInbox)
		r.Get("/attachment/{id}", h.handleGetAttachment)
		r.Post("/logout", h.handleLogout)
	})

	return r
}
