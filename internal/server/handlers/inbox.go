package handlers

import (
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"lilmail/internal/email"
	"lilmail/internal/models"
)

// Update InboxPage struct to use the new Folder type
type InboxPage struct {
	PageData
	Folders       []Folder        // Changed from []string to []Folder
	Messages      []*models.Email // Make sure this matches your Email type
	CurrentFolder string
	Pagination    *PaginationData
}

// PaginationData holds pagination information
type PaginationData struct {
	CurrentPage int
	TotalPages  int
	HasNext     bool
	HasPrev     bool
	NextPage    int
	PrevPage    int
}

// Add this struct at the top of your handler file
type Folder struct {
	Name   string
	Unread int
}

func (h *Handler) HandleInbox(w http.ResponseWriter, r *http.Request) {
	client := r.Context().Value("client").(*email.Client)

	// Get folders
	folderStrings, err := client.GetFolders()
	if err != nil {
		http.Error(w, "Failed to fetch folders", http.StatusInternalServerError)
		return
	}

	// Convert string folders to Folder structs
	folders := make([]Folder, len(folderStrings))
	for i, name := range folderStrings {
		// Get unread count for each folder
		// status, _ := client.GetFolderStatus(name)
		// unread := 0

		folders[i] = Folder{
			Name:   name,
			Unread: 0,
		}
	}

	// Get pagination parameters
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	limit := 50

	// Fetch messages from INBOX
	opts := email.FetchOptions{
		Folder:    "INBOX",
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

	for _, v := range messages {
		fmt.Println(v.Body)
	}

	// Create template data
	data := &InboxPage{
		PageData: PageData{
			Title: "Inbox",
		},
		Folders:       folders, // Now using []Folder instead of []string
		Messages:      messages,
		CurrentFolder: "INBOX",
		Pagination: &PaginationData{
			CurrentPage: page,
			TotalPages:  (len(messages) + limit - 1) / limit,
			HasNext:     len(messages) == limit,
			HasPrev:     page > 1,
			NextPage:    page + 1,
			PrevPage:    page - 1,
		},
	}

	// Create template functions
	funcMap := template.FuncMap{
		"formatDate": func(t time.Time) string {
			now := time.Now()
			if t.Year() == now.Year() {
				if t.Month() == now.Month() && t.Day() == now.Day() {
					return t.Format("15:04")
				}
				return t.Format("Jan 2")
			}
			return t.Format("2006-01-02")
		},
	}

	// Get template paths
	templates := []string{
		filepath.Join(GetProjectRoot(), "templates", "layout.html"),
		filepath.Join(GetProjectRoot(), "templates", "inbox.html"),
	}

	// Parse and execute templates
	tmpl, err := template.New("layout.html").Funcs(funcMap).ParseFiles(templates...)
	if err != nil {
		http.Error(w, "Template parsing error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "content", data); err != nil {
		fmt.Printf("Template execution error: %v\n", err)
		http.Error(w, "Template execution error", http.StatusInternalServerError)
		return
	}
}
