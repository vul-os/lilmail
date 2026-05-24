// handlers/api/oauth.go
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lilmail/config"
)

// OAuthToken holds the credentials returned by an OAuth2 token endpoint.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// Expired reports whether the access token has expired (with a 60s safety
// margin). A zero Expiry is treated as "never expires".
func (t *OAuthToken) Expired() bool {
	if t.Expiry.IsZero() {
		return false
	}
	return time.Now().After(t.Expiry.Add(-60 * time.Second))
}

// BuildAuthURL constructs the provider authorization-endpoint URL.
func BuildAuthURL(cfg config.OAuth2Config, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURL)
	q.Set("scope", strings.Join(cfg.Scopes, " "))
	q.Set("state", state)
	if codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}

	sep := "?"
	if strings.Contains(cfg.AuthURL, "?") {
		sep = "&"
	}
	return cfg.AuthURL + sep + q.Encode()
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func doTokenRequest(cfg config.OAuth2Config, form url.Values) (*OAuthToken, error) {
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("invalid token response (status %d): %s", resp.StatusCode, string(body))
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("token endpoint error: %s %s", tr.Error, tr.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return nil, fmt.Errorf("token request failed (status %d): %s", resp.StatusCode, string(body))
	}

	tok := &OAuthToken{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		TokenType:    tr.TokenType,
	}
	if tr.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// ExchangeCode swaps an authorization code for an access/refresh token.
func ExchangeCode(cfg config.OAuth2Config, code, codeVerifier string) (*OAuthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURL)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	return doTokenRequest(cfg, form)
}

// RefreshOAuthToken obtains a fresh access token using the refresh token.
func RefreshOAuthToken(cfg config.OAuth2Config, refreshToken string) (*OAuthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	tok, err := doTokenRequest(cfg, form)
	if err != nil {
		return nil, err
	}
	// Many providers do not reissue a refresh token on refresh; keep the old one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

// EmailFromToken resolves the user's email address, preferring a configured
// userinfo endpoint and falling back to the id_token claims.
func EmailFromToken(cfg config.OAuth2Config, tok *OAuthToken) (string, error) {
	claim := cfg.EmailClaim
	if claim == "" {
		claim = "email"
	}

	if cfg.UserInfoURL != "" {
		if m, err := fetchUserInfo(cfg.UserInfoURL, tok.AccessToken); err == nil {
			if v := stringClaim(m, claim); v != "" {
				return v, nil
			}
			if v := stringClaim(m, "preferred_username"); strings.Contains(v, "@") {
				return v, nil
			}
		}
	}

	if tok.IDToken != "" {
		if m, err := decodeJWTClaims(tok.IDToken); err == nil {
			if v := stringClaim(m, claim); v != "" {
				return v, nil
			}
			if v := stringClaim(m, "preferred_username"); strings.Contains(v, "@") {
				return v, nil
			}
		}
	}

	return "", fmt.Errorf("could not determine email from provider; set [oauth2] userinfo_url or request the 'openid email' scopes")
}

func fetchUserInfo(userInfoURL, accessToken string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, userInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func stringClaim(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// decodeJWTClaims decodes the (unverified) payload of a JWT. The token is
// obtained directly from the token endpoint over TLS and is additionally
// validated against the mail server, so the signature is not re-checked here.
func decodeJWTClaims(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded encodings.
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil, err
		}
	}

	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// RandomURLToken returns n random bytes encoded as a URL-safe string.
// Used for the OAuth2 state parameter and PKCE code verifier.
func RandomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// PKCEChallenge derives the S256 code challenge from a code verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
