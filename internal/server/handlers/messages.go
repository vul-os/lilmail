// handlers/messages.go
package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"lilmail/internal/email"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)
	folder := chi.URLParam(r, "folder")

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	if page == 0 {
		page = 1
	}

	opts := email.FetchOptions{
		Folder:    folder,
		Start:     uint32((page-1)*limit + 1),
		Count:     uint32(limit),
		FetchBody: false,
		UseCache:  true,
	}

	messages, err := client.FetchMessages(r.Context(), opts)
	if err != nil {
		http.Error(w, "Failed to fetch messages", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(messages)
}

func (h *Handler) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)
	folder := chi.URLParam(r, "folder")
	uid, err := strconv.ParseUint(chi.URLParam(r, "uid"), 10, 32)
	if err != nil {
		http.Error(w, "Invalid UID", http.StatusBadRequest)
		return
	}

	opts := email.FetchOptions{
		Folder:    folder,
		Start:     uint32(uid),
		Count:     1,
		FetchBody: true,
		UseCache:  true,
	}

	messages, err := client.FetchMessages(r.Context(), opts)
	if err != nil {
		http.Error(w, "Failed to fetch message", http.StatusInternalServerError)
		return
	}

	if len(messages) == 0 {
		http.NotFound(w, r)
		return
	}

	go client.MarkMessageSeen(uint32(uid), folder)

	json.NewEncoder(w).Encode(messages[0])
}

func (h *Handler) handleMoveMessage(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)
	folder := chi.URLParam(r, "folder")
	uid, _ := strconv.ParseUint(chi.URLParam(r, "uid"), 10, 32)

	var req struct {
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if err := client.MoveMessage(uint32(uid), folder, req.Destination); err != nil {
		http.Error(w, "Failed to move message", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)
	folder := chi.URLParam(r, "folder")
	uid, _ := strconv.ParseUint(chi.URLParam(r, "uid"), 10, 32)

	if err := client.MoveMessage(uint32(uid), folder, "Trash"); err != nil {
		http.Error(w, "Failed to delete message", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleFlagMessage(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)
	folder := chi.URLParam(r, "folder")
	uid, _ := strconv.ParseUint(chi.URLParam(r, "uid"), 10, 32)

	var req struct {
		Flag string `json:"flag"`
		Set  bool   `json:"set"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if err := client.MarkMessageFlag(uint32(uid), folder, req.Flag, req.Set); err != nil {
		http.Error(w, "Failed to set flag", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
