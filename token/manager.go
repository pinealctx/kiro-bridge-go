package token

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type TokenData struct {
	AccessToken   string
	RefreshToken  string
	ExpiresAt     time.Time
	ClientID      string
	TokenEndpoint string // external-idp only
	ProfileARN    string
}

type Manager struct {
	dbPath                   string
	profileARN               string // override from config
	token                    *TokenData
	mu                       sync.Mutex
	isLegacy                 bool
	legacyClientSecret       string
	legacyClientSecretExpiry time.Time
	IsExternalIdP            bool
	http                     *http.Client
	loginToken               *LoginToken // PKCE login token
	tokenFilePath            string      // path to token.json
}

func NewManager(dbPath, profileARN, tokenFilePath string) *Manager {
	return &Manager{
		dbPath:        dbPath,
		profileARN:    profileARN,
		tokenFilePath: tokenFilePath,
		http:          &http.Client{Timeout: 15 * time.Second},
	}
}

func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSuffix(s, "Z")
	if idx := strings.Index(s, "."); idx >= 0 {
		s = s[:idx]
	}
	t, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func (m *Manager) readDB() (*TokenData, error) {
	db, err := sql.Open("sqlite", m.dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	defer db.Close()

	// Try external-idp first
	var raw string
	err = db.QueryRow("SELECT value FROM auth_kv WHERE key='kirocli:external-idp:token'").Scan(&raw)
	if err == nil {
		return m.parseExternalIdPToken(db, raw)
	}

	// Fall back to legacy
	err = db.QueryRow("SELECT value FROM auth_kv WHERE key='kirocli:odic:token'").Scan(&raw)
	if err == nil {
		return m.parseLegacyToken(db, raw)
	}

	return nil, fmt.Errorf("no kiro-cli token found — run 'kiro-cli login' first")
}

func (m *Manager) readProfileARN(db *sql.DB) string {
	if m.profileARN != "" {
		return m.profileARN
	}
	var raw string
	if err := db.QueryRow("SELECT value FROM state WHERE key='api.codewhisperer.profile'").Scan(&raw); err != nil {
		return ""
	}
	var profile map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return ""
	}
	if arn, ok := profile["arn"].(string); ok {
		return arn
	}
	return ""
}

func (m *Manager) parseExternalIdPToken(db *sql.DB, raw string) (*TokenData, error) {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("parse external-idp token: %w", err)
	}
	m.isLegacy = false
	m.IsExternalIdP = true

	expiresAt, err := parseTimestamp(data["expires_at"].(string))
	if err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}

	profileARN := m.readProfileARN(db)
	log.Printf("Loaded external-idp token (Microsoft OAuth2), expires at %s, profileARN: %s", data["expires_at"], profileARN)

	td := &TokenData{
		AccessToken:  data["access_token"].(string),
		RefreshToken: data["refresh_token"].(string),
		ExpiresAt:    expiresAt,
		ProfileARN:   profileARN,
	}
	if v, ok := data["client_id"].(string); ok {
		td.ClientID = v
	}
	if v, ok := data["token_endpoint"].(string); ok {
		td.TokenEndpoint = v
	}
	return td, nil
}

func (m *Manager) parseLegacyToken(db *sql.DB, raw string) (*TokenData, error) {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("parse legacy token: %w", err)
	}
	m.isLegacy = true
	m.IsExternalIdP = false

	expiresAt, err := parseTimestamp(data["expires_at"].(string))
	if err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}

	profileARN := m.readProfileARN(db)

	// Read device registration
	var regRaw string
	clientID := ""
	if err := db.QueryRow("SELECT value FROM auth_kv WHERE key='kirocli:odic:device-registration'").Scan(&regRaw); err == nil {
		var reg map[string]interface{}
		if json.Unmarshal([]byte(regRaw), &reg) == nil {
			if v, ok := reg["client_id"].(string); ok {
				clientID = v
			}
			if v, ok := reg["client_secret"].(string); ok {
				m.legacyClientSecret = v
			}
			if v, ok := reg["client_secret_expires_at"].(string); ok {
				if t, err := parseTimestamp(v); err == nil {
					m.legacyClientSecretExpiry = t
				}
			}
		}
	}

	log.Printf("Loaded legacy IdC token (Builder ID)")
	return &TokenData{
		AccessToken:  data["access_token"].(string),
		RefreshToken: data["refresh_token"].(string),
		ExpiresAt:    expiresAt,
		ClientID:     clientID,
		ProfileARN:   profileARN,
	}, nil
}

var retryDelays = []time.Duration{1 * time.Second, 3 * time.Second, 10 * time.Second}

func (m *Manager) refreshExternalIdP(token *TokenData) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelays[attempt-1])
		}
		log.Printf("Refreshing external-idp token via Microsoft OAuth2 (attempt %d)...", attempt+1)

		form := url.Values{
			"client_id":     {token.ClientID},
			"grant_type":    {"refresh_token"},
			"refresh_token": {token.RefreshToken},
			"scope":         {"openid profile offline_access"},
		}
		req, _ := http.NewRequest("POST", token.TokenEndpoint, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := m.http.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("Token refresh failed (attempt %d): %v", attempt+1, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("external-idp refresh failed: %d %s", resp.StatusCode, string(body[:min(200, len(body))]))
			log.Printf("Token refresh failed (attempt %d): %v", attempt+1, lastErr)
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			lastErr = err
			continue
		}
		expiresIn := 3600.0
		if v, ok := data["expires_in"].(float64); ok {
			expiresIn = v
		}
		token.AccessToken = data["access_token"].(string)
		token.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		if v, ok := data["refresh_token"].(string); ok {
			token.RefreshToken = v
		}
		log.Printf("External-idp token refreshed, expires in %.0fs", expiresIn)
		return nil
	}
	return fmt.Errorf("token refresh failed after %d attempts: %w", len(retryDelays)+1, lastErr)
}

func (m *Manager) refreshLegacy(token *TokenData, idcURL string) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelays[attempt-1])
		}
		log.Printf("Refreshing IdC access token (legacy, attempt %d)...", attempt+1)

		body, _ := json.Marshal(map[string]string{
			"clientId":     token.ClientID,
			"clientSecret": m.legacyClientSecret,
			"grantType":    "refresh_token",
			"refreshToken": token.RefreshToken,
		})
		req, _ := http.NewRequest("POST", idcURL, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Host", "oidc.us-east-1.amazonaws.com")
		req.Header.Set("x-amz-user-agent", "aws-sdk-js/3.738.0 ua/2.1 os/other lang/js md/browser#unknown_unknown api/sso-oidc#3.738.0 m/E KiroIDE")

		resp, err := m.http.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("Token refresh failed (attempt %d): %v", attempt+1, err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("IdC refresh failed: %d %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
			log.Printf("Token refresh failed (attempt %d): %v", attempt+1, lastErr)
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal(respBody, &data); err != nil {
			lastErr = err
			continue
		}
		expiresIn := 3600.0
		if v, ok := data["expiresIn"].(float64); ok {
			expiresIn = v
		}
		token.AccessToken = data["accessToken"].(string)
		token.ExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		log.Printf("Legacy token refreshed, expires in %.0fs", expiresIn)
		return nil
	}
	return fmt.Errorf("token refresh failed after %d attempts: %w", len(retryDelays)+1, lastErr)
}

func (m *Manager) GetAccessToken(idcURL string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Priority 1: PKCE login token (from token.json)
	if m.loginToken == nil && m.tokenFilePath != "" {
		if lt, err := LoadLoginToken(m.tokenFilePath); err == nil {
			m.loginToken = lt
			m.IsExternalIdP = lt.IsExternalIdP
			if lt.ProfileArn != "" {
				m.profileARN = lt.ProfileArn
			}
			log.Printf("Loaded login token from %s (external_idp=%v)", m.tokenFilePath, lt.IsExternalIdP)
		}
	}

	if m.loginToken != nil {
		// Check if login token needs refresh
		if time.Now().After(m.loginToken.ExpiresAt.Add(-5 * time.Minute)) {
			if err := m.refreshLoginToken(); err != nil {
				log.Printf("Login token refresh failed: %v", err)
				// If still valid, use it
				if time.Now().Before(m.loginToken.ExpiresAt) {
					return m.loginToken.AccessToken, nil
				}
				// Fall through to SQLite
			} else {
				return m.loginToken.AccessToken, nil
			}
		} else {
			return m.loginToken.AccessToken, nil
		}
	}

	// Priority 2: SQLite DB (kiro-cli)
	if m.token == nil {
		t, err := m.readDB()
		if err != nil {
			return "", err
		}
		m.token = t
		log.Printf("Loaded token from %s", m.dbPath)
	}

	// Legacy: check client_secret expiry
	if m.isLegacy && !m.legacyClientSecretExpiry.IsZero() {
		remaining := time.Until(m.legacyClientSecretExpiry)
		if remaining < 0 {
			return "", fmt.Errorf("device registration (client_secret) expired — run 'kiro-cli login' or 'kiro-bridge-go login' to re-authenticate")
		}
		if remaining < 7*24*time.Hour {
			log.Printf("WARNING: client_secret expires in %.1f days — run 'kiro-cli login' soon", remaining.Hours()/24)
		}
	}

	// Refresh if expiring within 5 minutes
	if time.Now().After(m.token.ExpiresAt.Add(-5 * time.Minute)) {
		var refreshErr error
		if m.isLegacy {
			refreshErr = m.refreshLegacy(m.token, idcURL)
		} else {
			refreshErr = m.refreshExternalIdP(m.token)
		}
		if refreshErr != nil {
			if time.Now().Before(m.token.ExpiresAt) {
				log.Printf("Token refresh failed, using existing token (still valid): %v", refreshErr)
				return m.token.AccessToken, nil
			}
			return "", refreshErr
		}
	}

	return m.token.AccessToken, nil
}

func (m *Manager) ProfileARN() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loginToken != nil && m.loginToken.ProfileArn != "" {
		return m.loginToken.ProfileArn
	}
	if m.token == nil {
		t, err := m.readDB()
		if err != nil {
			return ""
		}
		m.token = t
	}
	return m.token.ProfileARN
}

// SetLoginToken injects a token obtained from the PKCE login flow.
func (m *Manager) SetLoginToken(lt *LoginToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lt.RefreshScope == "" {
		lt.RefreshScope = ExtractRefreshScope(lt.AccessToken)
		if lt.RefreshScope != "" {
			log.Printf("Extracted refresh scope from JWT: %s", lt.RefreshScope)
		}
	}
	m.loginToken = lt
	m.IsExternalIdP = lt.IsExternalIdP
	if lt.ProfileArn != "" {
		m.profileARN = lt.ProfileArn
	}
}

// refreshLoginToken refreshes the PKCE login token.
func (m *Manager) refreshLoginToken() error {
	lt := m.loginToken
	if lt.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}
	if lt.TokenEndpoint == "" {
		return fmt.Errorf("no token endpoint for login token refresh")
	}

	var err error
	if IsAWSIdCEndpoint(lt.TokenEndpoint) {
		err = m.refreshLoginAWSIdC(lt)
	} else {
		err = m.refreshLoginOAuth2(lt)
	}
	if err != nil {
		return err
	}

	// Persist refreshed token
	if m.tokenFilePath != "" {
		if saveErr := SaveLoginToken(m.tokenFilePath, lt); saveErr != nil {
			log.Printf("WARNING: failed to persist refreshed token: %v", saveErr)
		}
	}
	return nil
}

func (m *Manager) refreshLoginOAuth2(lt *LoginToken) error {
	form := url.Values{}
	form.Set("client_id", lt.ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", lt.RefreshToken)
	if lt.ClientSecret != "" {
		form.Set("client_secret", lt.ClientSecret)
	}
	if lt.RefreshScope != "" {
		form.Set("scope", lt.RefreshScope)
	}

	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelays[attempt-1])
		}

		resp, err := m.http.PostForm(lt.TokenEndpoint, form)
		if err != nil {
			lastErr = fmt.Errorf("login token refresh request: %w", err)
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("login token refresh status: %d", resp.StatusCode)
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("login token refresh status: %d", resp.StatusCode)
		}

		var result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decode login token refresh: %w", err)
		}
		resp.Body.Close()

		if result.RefreshToken != "" {
			lt.RefreshToken = result.RefreshToken
		}
		lt.AccessToken = result.AccessToken
		lt.ExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		log.Printf("Login token refreshed (OAuth2), expires in %ds", result.ExpiresIn)
		return nil
	}
	return fmt.Errorf("login token refresh failed after retries: %w", lastErr)
}

func (m *Manager) refreshLoginAWSIdC(lt *LoginToken) error {
	reqBody, _ := json.Marshal(map[string]string{
		"clientId":     lt.ClientID,
		"clientSecret": lt.ClientSecret,
		"grantType":    "refresh_token",
		"refreshToken": lt.RefreshToken,
	})

	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelays[attempt-1])
		}

		req, _ := http.NewRequest("POST", lt.TokenEndpoint, strings.NewReader(string(reqBody)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := m.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("AWS IdC token refresh request: %w", err)
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("AWS IdC token refresh status: %d", resp.StatusCode)
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("AWS IdC token refresh status: %d", resp.StatusCode)
		}

		var result struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decode AWS IdC token refresh: %w", err)
		}
		resp.Body.Close()

		if result.RefreshToken != "" {
			lt.RefreshToken = result.RefreshToken
		}
		lt.AccessToken = result.AccessToken
		lt.ExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		log.Printf("Login token refreshed (AWS IdC), expires in %ds", result.ExpiresIn)
		return nil
	}
	return fmt.Errorf("AWS IdC token refresh failed after retries: %w", lastErr)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
