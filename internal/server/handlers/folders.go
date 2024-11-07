// handlers/folders.go
package handlers

import (
	"encoding/json"
	"fmt"
	"lilmail/internal/email"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) handleHome(w http.ResponseWriter, r *http.Request) {
	filepath := filepath.Join(GetProjectRoot(), "templates", "index.html")
	http.ServeFile(w, r, filepath)
}

func (h *Handler) handleGetFolders(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)

	folders, err := client.GetFolders()
	if err != nil {
		http.Error(w, "Failed to get folders", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(folders)
}

func (h *Handler) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	attachID := chi.URLParam(r, "id")

	data, err := h.cache.Get(attachID)
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}

	metadata, err := h.cache.Get(attachID + ".meta")
	if err != nil {
		http.Error(w, "Invalid attachment metadata", http.StatusInternalServerError)
		return
	}

	var meta struct {
		ContentType string `json:"content_type"`
		Filename    string `json:"filename"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		http.Error(w, "Invalid attachment metadata", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", meta.Filename))
	w.Write(data)
}
