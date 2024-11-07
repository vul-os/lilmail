package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"path/filepath"
	"strconv"

	"lilmail/internal/auth"
	"lilmail/internal/email"
	"lilmail/internal/models"
)

// PageData holds common data for all pages
type PageData struct {
	Title string
	User  *models.User // Your user struct
	Error string
}

// LoginPage extends PageData for the login page
type LoginPage struct {
	PageData
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Create template data
	data := &LoginPage{
		PageData: PageData{
			Title: "Login",
		},
	}

	// Get template paths
	templates := []string{
		filepath.Join(GetProjectRoot(), "templates", "layout.html"),
		filepath.Join(GetProjectRoot(), "templates", "login.html"),
	}

	// Parse and execute templates
	tmpl, err := template.ParseFiles(templates...)
	if err != nil {
		fmt.Printf("Template parsing error: %v\n", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.Execute(w, data); err != nil {
		fmt.Printf("Template execution error: %v\n", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Server   string `json:"server,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	username, err := GetUsernameFromEmail(creds.Email)
	if err != nil {
		return
	}

	serverConfig := &models.ServerConfig{
		Username: username,
	}

	// Auto-discover IMAP server if not provided
	if creds.Server == "" {
		imapServer, err := email.GetMailServer(creds.Email)
		fmt.Println(imapServer)
		if err != nil {
			http.Error(w, "Server detection failed", http.StatusBadRequest)
			return
		}
		fmt.Println(imapServer)
		host, portStr, err := net.SplitHostPort(imapServer)
		if err != nil {
			http.Error(w, "Invalid server configuration", http.StatusInternalServerError)
			return
		}
		fmt.Println(host, portStr)
		port, err := strconv.Atoi(portStr)
		if err != nil {
			http.Error(w, "Invalid port number", http.StatusInternalServerError)
			return
		}

		serverConfig.IMAPServer = host
		serverConfig.IMAPPort = port
		serverConfig.UseSSL = port == 993
		serverConfig.AutoDiscovered = true
	} else {
		serverConfig.IMAPServer = creds.Server
		serverConfig.IMAPPort = 993 // Default to SSL port
		serverConfig.UseSSL = true
		serverConfig.AutoDiscovered = false
	}

	// Encrypt password
	encryptedPass, err := h.crypto.Encrypt([]byte(creds.Password))
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	serverConfig.EncryptedPass = base64.StdEncoding.EncodeToString(encryptedPass)

	// Create and connect email client
	client := email.NewClient(serverConfig, h.cache, h.crypto)
	if err := client.Connect(); err != nil {
		http.Error(w, "Connection failed", http.StatusUnauthorized)
		return
	}

	// Create session
	loginCreds := &auth.LoginCredentials{
		Email:    creds.Email,
		Password: creds.Password,
		Server:   creds.Server,
	}

	session, err := h.auth.Login(loginCreds, r.RemoteAddr, serverConfig)
	if err != nil {
		http.Error(w, "Session creation failed", http.StatusInternalServerError)
		return
	}

	h.sessions[session.ID] = client

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    session.ID,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		if client, ok := h.sessions[cookie.Value]; ok {
			client.Disconnect()
			delete(h.sessions, cookie.Value)
		}
		h.auth.Logout(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		session, err := h.auth.ValidateSession(cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		client, ok := h.sessions[session.ID]
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), "client", client)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
