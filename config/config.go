package config

import (
	"crypto/tls"
	"fmt"

	"github.com/BurntSushi/toml"
)

type ServerConfig struct {
	Port            int  `toml:"port"`
	UsernameIsEmail bool `toml:"username_is_email"`
	// FrameAncestors, when set, allows LilMail to be embedded as an iframe by the
	// listed origins (space-separated, CSP frame-ancestors syntax). This is what
	// lets a host shell such as Vulos OS embed LilMail as its built-in Mail app.
	// When empty, the default same-origin-only framing policy applies.
	FrameAncestors string `toml:"frame_ancestors"`
	// SecureCookies sets the Secure flag on the session cookie. Enable this when
	// LilMail is served over HTTPS (either directly via [ssl] or behind a TLS
	// reverse proxy). Defaults to false so plain-HTTP local dev works out of the
	// box; set to true in any internet-facing deployment.
	SecureCookies bool `toml:"secure_cookies"`
}

type IMAPConfig struct {
	Server string `toml:"server"`
	Port   int    `toml:"port"`
}

type SMTPConfig struct {
	Server             string `toml:"server"`
	Port               int    `toml:"port"`
	UseSTARTTLS        bool   `toml:"use_starttls"`         // true for port 587, false for port 465
	InsecureSkipVerify bool   `toml:"insecure_skip_verify"` // allow self-signed certs; default false
}

type JWTConfig struct {
	Secret string `toml:"secret"` // For JWT signing
}

// OAuth2Config configures OAuth2 / OpenID Connect login for IMAP and SMTP.
// The same access token is presented to the mail server using either the
// XOAUTH2 or the OAUTHBEARER SASL mechanism.
type OAuth2Config struct {
	Enabled      bool     `toml:"enabled"`
	ClientID     string   `toml:"client_id"`
	ClientSecret string   `toml:"client_secret"`
	AuthURL      string   `toml:"auth_url"`     // Authorization endpoint
	TokenURL     string   `toml:"token_url"`    // Token endpoint
	UserInfoURL  string   `toml:"userinfo_url"` // Optional; used to look up the email
	RedirectURL  string   `toml:"redirect_url"` // Must match the registered redirect URI
	Scopes       []string `toml:"scopes"`
	Mechanism    string   `toml:"mechanism"`   // "xoauth2" (default) or "oauthbearer"
	EmailClaim   string   `toml:"email_claim"` // Claim holding the email (default "email")
	UsePKCE      bool     `toml:"use_pkce"`    // Use PKCE (S256); recommended
}

type CacheConfig struct {
	Folder string `toml:"folder"`
}

type EncryptionConfig struct {
	Key string `toml:"key"` // 32-byte key for AES encryption
}

type SSLConfig struct {
	Enabled      bool   `toml:"enabled"`
	CertFile     string `toml:"cert_file"`     // Path to fullchain.pem
	KeyFile      string `toml:"key_file"`      // Path to privkey.pem
	Port         int    `toml:"port"`          // HTTPS port (default 443)
	HTTPPort     int    `toml:"http_port"`     // HTTP port for redirect (default 80)
	AutoRedirect bool   `toml:"auto_redirect"` // Redirect HTTP to HTTPS
	Domain       string `toml:"domain"`        // Domain name for HSTS
	HSTSMaxAge   int    `toml:"hsts_max_age"`  // Max age for HSTS in seconds
}

// CalDAVConfig configures the optional CalDAV calendar integration.
// Set [caldav] enabled = true in config.toml to activate the calendar routes.
type CalDAVConfig struct {
	Enabled  bool   `toml:"enabled"`
	URL      string `toml:"url"`      // CalDAV endpoint / principal or discovery URL
	Auth     string `toml:"auth"`     // "basic" (default) or "oauth2"
	Username string `toml:"username"` // used when auth = "basic"
	Password string `toml:"password"` // used when auth = "basic"
}

// AIConfig configures the mail-AI assistant endpoints.
//
// LilMail is a standalone mail client; all LLM inference is delegated to a
// configurable completion endpoint rather than performed locally. When running
// embedded inside Vulos OS, the default endpoint points at the OS airouter's
// /api/ai/chat (POST, SSE), which handles provider routing, metering, and
// rate-limiting. For standalone use, point at any OpenAI-compatible SSE
// completion endpoint.
//
//	[ai]
//	enabled  = true
//	endpoint = "http://localhost:8080/api/ai/chat"
//	api_key  = ""      # Bearer token — leave empty when calling local airouter
//	model    = ""      # forwarded to the endpoint; leave empty to use endpoint default
type AIConfig struct {
	// Enabled is the master switch. When false, all /api/ai/* routes return
	// 404 {"error":"ai_disabled"}. Default: false (opt-in).
	Enabled bool `toml:"enabled"`

	// Endpoint is the URL of the OpenAI-compatible SSE chat-completion API.
	// Defaults to the Vulos OS airouter URL so LilMail works out of the box
	// when embedded in Vulos; override for standalone or BYO use.
	Endpoint string `toml:"endpoint"`

	// APIKey is sent as "Authorization: Bearer <key>" when non-empty.
	// Leave empty when calling the local Vulos airouter (it uses session auth).
	APIKey string `toml:"api_key"`

	// Model is the model slug forwarded to the completion endpoint.
	// Leave empty to use the endpoint's configured default.
	Model string `toml:"model"`
}

// NotificationsConfig configures Phase-6 real-time notifications.
// Everything is opt-in and default-disabled: with Enabled = false (the
// default) the application behaves exactly as without this feature — no extra
// goroutines, no SSE route, no JS injected into pages.
//
//	[notifications]
//	enabled = false          # master switch — MUST be true to activate anything
//	idle    = true           # start an IMAP IDLE watcher when enabled
//	desktop = false          # native OS toast via gen2brain/beeep (local runs)
type NotificationsConfig struct {
	Enabled bool `toml:"enabled"` // master switch; default false
	Idle    bool `toml:"idle"`    // IMAP IDLE watcher; default true when Enabled
	Desktop bool `toml:"desktop"` // native OS toasts via beeep; default false
}

type Config struct {
	Server        ServerConfig        `toml:"server"`
	IMAP          IMAPConfig          `toml:"imap"`
	SMTP          SMTPConfig          `toml:"smtp"`
	JWT           JWTConfig           `toml:"jwt"`
	Cache         CacheConfig         `toml:"cache"`
	Encryption    EncryptionConfig    `toml:"encryption"`
	SSL           SSLConfig           `toml:"ssl"`
	OAuth2        OAuth2Config        `toml:"oauth2"`
	CalDAV        CalDAVConfig        `toml:"caldav"`
	Notifications NotificationsConfig `toml:"notifications"`
	AI            AIConfig            `toml:"ai"`
}

func LoadConfig(filepath string) (*Config, error) {
	var config Config

	config.Server.UsernameIsEmail = true
	config.Server.Port = 3000
	// Set default values
	config.SMTP.Port = 587 // Default to STARTTLS port
	config.SMTP.UseSTARTTLS = true

	// Default SSL configuration
	config.SSL.Port = 443
	config.SSL.HTTPPort = 80
	config.SSL.HSTSMaxAge = 31536000 // 1 year
	config.SSL.AutoRedirect = true

	// Default OAuth2 configuration
	config.OAuth2.Mechanism = "xoauth2"
	config.OAuth2.EmailClaim = "email"
	config.OAuth2.UsePKCE = true

	// Default CalDAV configuration
	config.CalDAV.Auth = "basic"

	// Default Notifications configuration — everything OFF by default.
	// Idle is set to true here so that it activates automatically once the
	// user opts in by setting enabled = true; they can still turn it off
	// individually with idle = false.
	config.Notifications.Enabled = false
	config.Notifications.Idle = true
	config.Notifications.Desktop = false

	// Default AI configuration.
	// Enabled defaults to false (explicit opt-in required).
	// The default endpoint is the Vulos OS airouter so LilMail works without
	// extra configuration when embedded in a Vulos installation.
	config.AI.Enabled = false
	config.AI.Endpoint = "http://localhost:8080/api/ai/chat"
	config.AI.APIKey = ""
	config.AI.Model = ""

	// Load config file
	_, err := toml.DecodeFile(filepath, &config)
	if err != nil {
		return nil, err
	}

	// If SMTP server is not specified, derive it from IMAP server
	if config.SMTP.Server == "" {
		config.SMTP.Server = config.IMAP.Server
		// Convert imap.server.com to smtp.server.com
		if len(config.SMTP.Server) > 5 && config.SMTP.Server[:5] == "imap." {
			config.SMTP.Server = "smtp" + config.SMTP.Server[4:]
		}
	}

	// Validate SSL configuration if enabled
	if config.SSL.Enabled {
		if err := config.ValidateSSL(); err != nil {
			return nil, fmt.Errorf("SSL configuration error: %w", err)
		}
	}

	return &config, nil
}

// Helper method to get the appropriate SMTP port based on encryption
func (c *SMTPConfig) GetPort() int {
	if c.Port != 0 {
		return c.Port
	}
	if c.UseSTARTTLS {
		return 587 // STARTTLS port
	}
	return 465 // SSL/TLS port
}

// ValidateSSL checks if the SSL configuration is valid
func (c *Config) ValidateSSL() error {
	if !c.SSL.Enabled {
		return nil
	}

	if c.SSL.CertFile == "" {
		return fmt.Errorf("SSL certificate file path is required")
	}

	if c.SSL.KeyFile == "" {
		return fmt.Errorf("SSL key file path is required")
	}

	// Try loading the certificates to verify they're valid
	_, err := tls.LoadX509KeyPair(c.SSL.CertFile, c.SSL.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to load SSL certificates: %w", err)
	}

	return nil
}

// GetSecurityHeaders returns a map of security headers based on the configuration.
//
// The baseline hardening headers (content-type, XSS, referrer, and the framing
// policy) are emitted unconditionally so they apply whether or not TLS is
// terminated here — LilMail commonly runs plain HTTP behind a host shell or
// reverse proxy. HSTS is the only SSL-gated header (it is meaningless without
// TLS).
func (c *Config) GetSecurityHeaders() map[string]string {
	headers := make(map[string]string)

	// HSTS only makes sense when TLS is terminated by LilMail itself.
	if c.SSL.Enabled && c.SSL.Domain != "" {
		headers["Strict-Transport-Security"] = fmt.Sprintf("max-age=%d; includeSubDomains", c.SSL.HSTSMaxAge)
	}

	headers["X-Content-Type-Options"] = "nosniff"
	headers["X-XSS-Protection"] = "1; mode=block"
	headers["Referrer-Policy"] = "strict-origin-when-cross-origin"

	// Content-Security-Policy — combines the framing policy with a strict
	// script-src so that injected HTML (e.g. from a broken srcdoc attribute)
	// cannot execute scripts in the LilMail origin.
	//
	// 'self'            — allow scripts/styles loaded from the same origin
	// 'unsafe-inline'   — needed for Alpine.js x-data / x-on inline handlers
	//                     and the small inline <style> blocks in the layout.
	//                     HTMX+Alpine rely heavily on inline JS attributes, so
	//                     removing this would require a nonce/hash approach.
	// blob:             — allows HTMX to use blob: object-URLs when needed.
	//
	// Email HTML bodies are sandboxed inside <iframe sandbox> (no allow-scripts)
	// so they never reach this CSP; this policy is the outer-page defence.
	scriptSrc := "'self' 'unsafe-inline'"
	imgSrc := "'self' data: blob:"
	connectSrc := "'self'"
	csp := "default-src 'self'; script-src " + scriptSrc + "; style-src 'self' 'unsafe-inline'; img-src " + imgSrc + "; connect-src " + connectSrc + "; object-src 'none'; base-uri 'self';"

	// Framing policy. When a host shell (e.g. Vulos OS) is allowed to embed
	// LilMail, express it via CSP frame-ancestors and omit the legacy
	// X-Frame-Options header (which has no allow-list form). Otherwise keep
	// the strict same-origin default.
	if c.Server.FrameAncestors != "" {
		csp += " frame-ancestors " + c.Server.FrameAncestors
	} else {
		headers["X-Frame-Options"] = "SAMEORIGIN"
		csp += " frame-ancestors 'self'"
	}
	headers["Content-Security-Policy"] = csp

	return headers
}
