// handlers/web/accounts.go
//
// Multi-account HTTP handlers.
//
// The primary account is always the one stored in the session.  Additional
// accounts are stored in AccountStore (bbolt) and rendered in a Settings panel.
//
// Routes (registered only when accounts.enabled = true):
//
//	GET  /api/accounts              → JSON list of additional accounts
//	POST /api/accounts              → add an account (validate IMAP, store encrypted)
//	DELETE /api/accounts/:email     → remove an account
//	POST /api/accounts/:email/switch → switch active session to this account
//	GET  /settings                  → settings page (accounts panel + push enable)
package web

import (
	"fmt"
	"lilmail/config"
	"lilmail/handlers/api"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
)

// AccountsHandler manages multi-account operations.
type AccountsHandler struct {
	store     *session.Store
	config    *config.Config
	auth      *AuthHandler
	acctStore *AccountStore
}

// NewAccountsHandler creates a handler. acctStore must be non-nil.
func NewAccountsHandler(store *session.Store, cfg *config.Config, auth *AuthHandler, acctStore *AccountStore) *AccountsHandler {
	return &AccountsHandler{
		store:     store,
		config:    cfg,
		auth:      auth,
		acctStore: acctStore,
	}
}

// HandleListAccounts returns the list of additional accounts for the current user.
func (h *AccountsHandler) HandleListAccounts(c *fiber.Ctx) error {
	owner, _ := c.Locals("username").(string)
	if owner == "" {
		return fiber.ErrUnauthorized
	}
	entries, err := h.acctStore.List(owner)
	if err != nil {
		log.Printf("accounts: list for %s: %v", owner, err)
		return fiber.ErrInternalServerError
	}
	// Strip encrypted password from API response.
	type safe struct {
		Email      string `json:"email"`
		Label      string `json:"label"`
		Color      string `json:"color,omitempty"`
		IMAPServer string `json:"imap_server"`
		IMAPPort   int    `json:"imap_port"`
		SMTPServer string `json:"smtp_server"`
		SMTPPort   int    `json:"smtp_port"`
	}
	out := make([]safe, 0, len(entries))
	for _, e := range entries {
		out = append(out, safe{
			Email:      e.Email,
			Label:      e.Label,
			Color:      e.Color,
			IMAPServer: e.IMAPServer,
			IMAPPort:   e.IMAPPort,
			SMTPServer: e.SMTPServer,
			SMTPPort:   e.SMTPPort,
		})
	}
	return c.JSON(out)
}

// HandleAddAccount validates the new account credentials against the IMAP
// server and, if successful, stores the account in AccountStore.
//
// Body (JSON):
//
//	{
//	  "email":       "alice@other.com",
//	  "password":    "secret",
//	  "label":       "Work",
//	  "color":       "#4CAF50",
//	  "imap_server": "imap.other.com",
//	  "imap_port":   993,
//	  "smtp_server": "smtp.other.com",
//	  "smtp_port":   587
//	}
func (h *AccountsHandler) HandleAddAccount(c *fiber.Ctx) error {
	owner, _ := c.Locals("username").(string)
	if owner == "" {
		return fiber.ErrUnauthorized
	}

	var req struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		Label      string `json:"label"`
		Color      string `json:"color"`
		IMAPServer string `json:"imap_server"`
		IMAPPort   int    `json:"imap_port"`
		SMTPServer string `json:"smtp_server"`
		SMTPPort   int    `json:"smtp_port"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	req.Email = strings.TrimSpace(req.Email)
	req.Password = strings.TrimSpace(req.Password)
	if req.Email == "" || req.Password == "" {
		return fiber.NewError(fiber.StatusBadRequest, "email and password required")
	}

	// Fill in defaults from global config when not specified.
	if req.IMAPServer == "" {
		req.IMAPServer = h.config.IMAP.Server
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = h.config.IMAP.Port
	}
	if req.SMTPServer == "" {
		req.SMTPServer = h.config.SMTP.Server
	}
	if req.SMTPPort == 0 {
		req.SMTPPort = h.config.SMTP.GetPort()
	}
	if req.Label == "" {
		req.Label = req.Email
	}

	// Derive IMAP username.
	username := req.Email
	if !h.config.Server.UsernameIsEmail {
		username = api.GetUsernameFromEmail(req.Email)
	}
	if username == "" {
		return fiber.NewError(fiber.StatusBadRequest, "invalid email format")
	}

	// Validate credentials by opening and immediately closing an IMAP connection.
	client, err := api.NewClient(req.IMAPServer, req.IMAPPort, username, req.Password)
	if err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, fmt.Sprintf("IMAP login failed: %v", err))
	}
	client.Close()

	// Encrypt the password using the application encryption key.
	encPwd, err := api.EncryptJSON(req.Password, h.config.Encryption.Key)
	if err != nil {
		log.Printf("accounts: encrypt password for %s: %v", req.Email, err)
		return fiber.ErrInternalServerError
	}

	entry := AccountEntry{
		Email:             req.Email,
		Label:             req.Label,
		Color:             req.Color,
		IMAPServer:        req.IMAPServer,
		IMAPPort:          req.IMAPPort,
		SMTPServer:        req.SMTPServer,
		SMTPPort:          req.SMTPPort,
		EncryptedPassword: encPwd,
	}
	if err := h.acctStore.Save(owner, entry); err != nil {
		log.Printf("accounts: save for %s: %v", owner, err)
		return fiber.ErrInternalServerError
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"ok":    true,
		"email": entry.Email,
		"label": entry.Label,
	})
}

// HandleDeleteAccount removes an additional account.
func (h *AccountsHandler) HandleDeleteAccount(c *fiber.Ctx) error {
	owner, _ := c.Locals("username").(string)
	if owner == "" {
		return fiber.ErrUnauthorized
	}
	email := c.Params("email")
	if email == "" {
		return fiber.NewError(fiber.StatusBadRequest, "email param required")
	}
	if err := h.acctStore.Delete(owner, email); err != nil {
		log.Printf("accounts: delete %s for %s: %v", email, owner, err)
		return fiber.ErrInternalServerError
	}
	return c.JSON(fiber.Map{"ok": true})
}

// HandleSwitchAccount replaces the session credentials with those of the
// requested additional account and redirects to /inbox.  The previously active
// account credentials are saved as an additional account under the NEW owner so
// the user can switch back.
func (h *AccountsHandler) HandleSwitchAccount(c *fiber.Ctx) error {
	owner, _ := c.Locals("username").(string)
	if owner == "" {
		return fiber.ErrUnauthorized
	}
	targetEmail := c.Params("email")
	if targetEmail == "" {
		return fiber.NewError(fiber.StatusBadRequest, "email param required")
	}

	// Load the target account.
	entries, err := h.acctStore.List(owner)
	if err != nil {
		return fiber.ErrInternalServerError
	}
	var target *AccountEntry
	for i := range entries {
		if entries[i].Email == targetEmail {
			target = &entries[i]
			break
		}
	}
	if target == nil {
		return fiber.NewError(fiber.StatusNotFound, "account not found")
	}

	// Decrypt the target password.
	var password string
	if err := api.DecryptJSON(target.EncryptedPassword, &password, h.config.Encryption.Key); err != nil {
		log.Printf("accounts: decrypt password for %s: %v", targetEmail, err)
		return fiber.ErrInternalServerError
	}

	// Derive IMAP username for the target account.
	targetUsername := targetEmail
	if !h.config.Server.UsernameIsEmail {
		targetUsername = api.GetUsernameFromEmail(targetEmail)
	}

	// Validate the target credentials are still good.
	client, err := api.NewClient(target.IMAPServer, target.IMAPPort, targetUsername, password)
	if err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, fmt.Sprintf("IMAP login failed for target account: %v", err))
	}
	client.Close()

	// Generate a new JWT for the target identity.
	token, err := api.GenerateToken(targetUsername, targetEmail, h.config.JWT.Secret)
	if err != nil {
		return fiber.ErrInternalServerError
	}

	// Encrypt the new credentials.
	encCreds, err := api.EncryptJSON(&api.Credentials{Email: targetEmail, Password: password}, h.config.Encryption.Key)
	if err != nil {
		return fiber.ErrInternalServerError
	}

	// Persist the current session account as an additional account under the new owner
	// (so the user can switch back).  We need the current password from the session.
	sess, err := h.store.Get(c)
	if err == nil {
		currentEmail, _ := sess.Get("email").(string)
		currentEncCreds, _ := sess.Get("credentials").(string)
		if currentEmail != "" && currentEncCreds != "" {
			var currentCreds api.Credentials
			if decErr := api.DecryptJSON(currentEncCreds, &currentCreds, h.config.Encryption.Key); decErr == nil {
				encBack, encErr := api.EncryptJSON(currentCreds.Password, h.config.Encryption.Key)
				if encErr == nil {
					backEntry := AccountEntry{
						Email:             currentEmail,
						Label:             currentEmail,
						IMAPServer:        h.config.IMAP.Server,
						IMAPPort:          h.config.IMAP.Port,
						SMTPServer:        h.config.SMTP.Server,
						SMTPPort:          h.config.SMTP.GetPort(),
						EncryptedPassword: encBack,
					}
					// Store it under the target user (our new identity).
					if saveErr := h.acctStore.Save(targetEmail, backEntry); saveErr != nil {
						log.Printf("accounts: save back-link account: %v", saveErr)
					}
				}
			}
		}
	}

	// Overwrite session with new identity.
	if sess == nil {
		sess, err = h.store.Get(c)
		if err != nil {
			return fiber.ErrInternalServerError
		}
	}
	sess.Set("authenticated", true)
	sess.Set("email", targetEmail)
	sess.Set("username", targetUsername)
	sess.Set("token", token)
	sess.Set("credentials", encCreds)
	// Clear any OAuth state from previous session.
	sess.Delete("auth_type")
	sess.Delete("oauth_token")

	if err := sess.Save(); err != nil {
		return fiber.ErrInternalServerError
	}

	return c.Redirect("/inbox")
}

// HandleSettings renders the settings page with the accounts panel.
func (h *AccountsHandler) HandleSettings(c *fiber.Ctx) error {
	owner, _ := c.Locals("username").(string)
	email, _ := c.Locals("email").(string)

	// Load additional accounts for display.
	entries, err := h.acctStore.List(owner)
	if err != nil {
		log.Printf("settings: list accounts for %s: %v", owner, err)
		entries = nil
	}
	// Strip passwords.
	type safeEntry struct {
		Email      string `json:"email"`
		Label      string
		Color      string
		IMAPServer string
	}
	safe := make([]safeEntry, 0, len(entries))
	for _, e := range entries {
		safe = append(safe, safeEntry{
			Email:      e.Email,
			Label:      e.Label,
			Color:      e.Color,
			IMAPServer: e.IMAPServer,
		})
	}

	// Load token from session.
	sess, _ := h.store.Get(c)
	token, _ := sess.Get("token").(string)

	return c.Render("settings", fiber.Map{
		"Title":           "Settings",
		"Username":        owner,
		"Email":           email,
		"Token":           token,
		"Accounts":        safe,
		"AccountsEnabled": h.config.Accounts.Enabled,
	})
}
