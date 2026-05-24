// handlers/web/oauth.go
package web

import (
	"fmt"
	"lilmail/handlers/api"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v2"
)

// HandleOAuthLogin starts the OAuth2 authorization-code flow by redirecting the
// user to the provider's authorization endpoint.
func (h *AuthHandler) HandleOAuthLogin(c *fiber.Ctx) error {
	if !h.config.OAuth2.Enabled {
		return c.Status(404).SendString("OAuth2 is not enabled")
	}

	sess, err := h.store.Get(c)
	if err != nil {
		return c.Status(500).SendString("Session error")
	}

	state, err := api.RandomURLToken(24)
	if err != nil {
		return c.Status(500).SendString("Failed to generate state")
	}
	sess.Set("oauth_state", state)

	var challenge string
	if h.config.OAuth2.UsePKCE {
		verifier, err := api.RandomURLToken(48)
		if err != nil {
			return c.Status(500).SendString("Failed to generate PKCE verifier")
		}
		sess.Set("oauth_verifier", verifier)
		challenge = api.PKCEChallenge(verifier)
	}

	if err := sess.Save(); err != nil {
		return c.Status(500).SendString("Failed to save session")
	}

	return c.Redirect(api.BuildAuthURL(h.config.OAuth2, state, challenge))
}

// HandleOAuthCallback completes the OAuth2 flow: it verifies state, exchanges
// the code for tokens, resolves the email, validates the token against the
// IMAP server, and establishes the session.
func (h *AuthHandler) HandleOAuthCallback(c *fiber.Ctx) error {
	if !h.config.OAuth2.Enabled {
		return c.Status(404).SendString("OAuth2 is not enabled")
	}

	sess, err := h.store.Get(c)
	if err != nil {
		return c.Status(500).SendString("Session error")
	}

	if errParam := c.Query("error"); errParam != "" {
		return h.oauthError(c, fmt.Sprintf("OAuth2 error: %s %s", errParam, c.Query("error_description")))
	}

	state := c.Query("state")
	wantState, _ := sess.Get("oauth_state").(string)
	if state == "" || state != wantState {
		return h.oauthError(c, "Invalid OAuth2 state")
	}

	code := c.Query("code")
	if code == "" {
		return h.oauthError(c, "Missing authorization code")
	}

	verifier, _ := sess.Get("oauth_verifier").(string)

	tok, err := api.ExchangeCode(h.config.OAuth2, code, verifier)
	if err != nil {
		return h.oauthError(c, "Token exchange failed: "+err.Error())
	}

	email, err := api.EmailFromToken(h.config.OAuth2, tok)
	if err != nil {
		return h.oauthError(c, err.Error())
	}

	username := email
	if !h.config.Server.UsernameIsEmail {
		username = api.GetUsernameFromEmail(email)
	}

	// Validate the token by authenticating against the IMAP server.
	client, err := api.NewClientOAuth(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		username,
		tok.AccessToken,
		h.config.OAuth2.Mechanism,
	)
	if err != nil {
		return h.oauthError(c, "Mail server rejected the OAuth2 token: "+err.Error())
	}
	defer client.Close()

	encToken, err := api.EncryptOAuthToken(tok, h.config.Encryption.Key)
	if err != nil {
		return h.oauthError(c, "Failed to secure token")
	}

	jwtToken, err := api.GenerateToken(username, email, h.config.JWT.Secret)
	if err != nil {
		return h.oauthError(c, "Failed to create authentication token")
	}

	sess.Delete("oauth_state")
	sess.Delete("oauth_verifier")
	sess.Set("authenticated", true)
	sess.Set("email", email)
	sess.Set("username", username)
	sess.Set("token", jwtToken)
	sess.Set("auth_type", "oauth2")
	sess.Set("oauth_token", encToken)
	sess.SetExpiry(24 * 60 * 60 * time.Second)

	if err := sess.Save(); err != nil {
		return h.oauthError(c, "Failed to create session")
	}

	userCacheFolder := filepath.Join(h.config.Cache.Folder, api.SanitizeUsername(username))
	if err := h.ensureUserCacheFolder(userCacheFolder); err == nil {
		if err := h.fetchInitialData(client, userCacheFolder); err != nil {
			fmt.Printf("Error fetching initial data for user %s: %v\n", username, err)
		}
	}

	return c.Redirect("/inbox")
}

// oauthError renders the login page with an error, keeping the OAuth2 button.
func (h *AuthHandler) oauthError(c *fiber.Ctx, msg string) error {
	return c.Status(401).Render("login", fiber.Map{
		"Error":         msg,
		"OAuth2Enabled": h.config.OAuth2.Enabled,
	})
}

// validOAuthToken returns a valid access token for the current session,
// transparently refreshing and re-persisting it when expired.
func (h *AuthHandler) validOAuthToken(c *fiber.Ctx) (username, accessToken string, err error) {
	sess, err := h.store.Get(c)
	if err != nil {
		return "", "", fmt.Errorf("failed to get session: %v", err)
	}

	enc, ok := sess.Get("oauth_token").(string)
	if !ok || enc == "" {
		return "", "", fmt.Errorf("no oauth token in session")
	}

	tok, err := api.DecryptOAuthToken(enc, h.config.Encryption.Key)
	if err != nil {
		return "", "", fmt.Errorf("failed to decrypt oauth token: %v", err)
	}

	if tok.Expired() && tok.RefreshToken != "" {
		newTok, rerr := api.RefreshOAuthToken(h.config.OAuth2, tok.RefreshToken)
		if rerr != nil {
			return "", "", fmt.Errorf("failed to refresh oauth token: %v", rerr)
		}
		tok = newTok
		if encNew, eerr := api.EncryptOAuthToken(tok, h.config.Encryption.Key); eerr == nil {
			sess.Set("oauth_token", encNew)
			_ = sess.Save()
		}
	}

	email, _ := sess.Get("email").(string)
	username = email
	if !h.config.Server.UsernameIsEmail {
		username = api.GetUsernameFromEmail(email)
	}
	return username, tok.AccessToken, nil
}
