package token

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	kiroSigninURL          = "https://app.kiro.dev/signin"
	kiroTokenExchangeURL   = "https://prod.us-east-1.auth.desktop.kiro.dev/token"
	externalIdPRedirectURI = "kiro://kiro.oauth/callback"
	DefaultCallbackPort    = 3128
	loginTimeout           = 5 * time.Minute

	builderIDScopes = "codewhisperer:completions codewhisperer:analysis codewhisperer:conversations codewhisperer:transformations codewhisperer:taskassist"
)

// LoginSession tracks a pending PKCE authorization code login flow.
type LoginSession struct {
	ID           string
	AuthURL      string
	CallbackPort int
	Status       string // pending, completed, expired, error
	Error        string

	state        string
	codeVerifier string

	externalIdP           bool
	externalIssuerURL     string
	externalClientID      string
	externalScopes        string
	externalState         string
	externalVerifier      string
	externalAuthEndpoint  string
	externalTokenEndpoint string

	builderID              bool
	builderIDOIDCBase      string
	builderIDClientID      string
	builderIDClientSecret  string
	builderIDDeviceCode    string
	builderIDUserCode      string
	builderIDVerifyURI     string
	builderIDInterval      int
	builderIDTokenEndpoint string

	AccessToken    string
	RefreshToken   string
	ClientID       string
	ClientSecret   string
	TokenEndpoint  string
	TokenExpiresAt time.Time
	ProfileArn     string

	server *http.Server
	mu     sync.Mutex
	done   chan struct{}
}

func (s *LoginSession) Done() <-chan struct{} { return s.done }

func (s *LoginSession) IsExternalIdP() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.externalIdP
}

func (s *LoginSession) IsBuilderID() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builderID
}

type callbackTokenResult struct {
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret,omitempty"`
	TokenEndpoint string `json:"token_endpoint"`
	ExpiresAt     string `json:"expires_at"`
	ExpiresIn     int    `json:"expires_in"`
	ProfileArn    string `json:"profileArn"`
}

// AuthManager manages PKCE login flows.
type AuthManager struct {
	mu       sync.Mutex
	sessions map[string]*LoginSession
}

func NewAuthManager() *AuthManager {
	return &AuthManager{sessions: make(map[string]*LoginSession)}
}

// StartLogin initiates a PKCE authorization code flow.
func (am *AuthManager) StartLogin(callbackPort int) (*LoginSession, error) {
	if callbackPort <= 0 {
		callbackPort = DefaultCallbackPort
	}

	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := computeCodeChallenge(verifier)
	state := uuid.New().String()
	redirectURI := fmt.Sprintf("http://localhost:%d", callbackPort)

	params := url.Values{}
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("redirect_uri", redirectURI)
	params.Set("redirect_from", "KiroIDE")

	session := &LoginSession{
		ID:           uuid.New().String(),
		AuthURL:      kiroSigninURL + "?" + params.Encode(),
		CallbackPort: callbackPort,
		Status:       "pending",
		state:        state,
		codeVerifier: verifier,
		done:         make(chan struct{}),
	}

	if err := am.startCallbackServer(session); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}

	am.mu.Lock()
	am.sessions[session.ID] = session
	am.mu.Unlock()

	log.Printf("[auth] PKCE login started, session=%s port=%d", session.ID, callbackPort)
	return session, nil
}

func (am *AuthManager) startCallbackServer(session *LoginSession) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		am.handleCallback(session, w, r)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		session.mu.Lock()
		st := session.Status
		errStr := session.Error
		session.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": st, "error": errStr})
	})

	addr := fmt.Sprintf("127.0.0.1:%d", session.CallbackPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	session.server = &http.Server{Handler: mux}

	go func() {
		if err := session.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[auth] callback server error: %v", err)
		}
	}()

	go func() {
		timer := time.NewTimer(loginTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			session.mu.Lock()
			if session.Status == "pending" {
				session.Status = "expired"
				session.Error = "login timeout"
			}
			session.mu.Unlock()
			_ = session.server.Close()
		case <-session.done:
		}
	}()

	return nil
}

func (am *AuthManager) handleCallback(session *LoginSession, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("code") == "" && q.Get("access_token") == "" && q.Get("error") == "" &&
		q.Get("login_option") == "" && r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	log.Printf("[auth] callback received: method=%s query_keys=%v", r.Method, keysOf(r.URL.Query()))

	if errMsg := q.Get("error"); errMsg != "" {
		if desc := q.Get("error_description"); desc != "" {
			errMsg = errMsg + ": " + desc
		}
		am.failSession(session, errMsg)
		writeCallbackHTML(w, false, errMsg)
		return
	}

	if q.Get("login_option") == "external_idp" {
		am.handleExternalIdPRedirect(session, w, r)
		return
	}

	if lo := q.Get("login_option"); lo == "builderid" || lo == "awsidc" {
		am.handleBuilderIDRedirect(session, w, r)
		return
	}

	session.mu.Lock()
	isExtIdP := session.externalIdP
	session.mu.Unlock()

	if isExtIdP {
		am.handleExternalIdPCallback(session, w, r)
		return
	}

	if state := q.Get("state"); state != "" && state != session.state {
		am.failSession(session, "state mismatch")
		writeCallbackHTML(w, false, "State mismatch")
		return
	}

	var tokenResult *callbackTokenResult

	if at := q.Get("access_token"); at != "" {
		tokenResult = &callbackTokenResult{
			AccessToken:   at,
			RefreshToken:  q.Get("refresh_token"),
			ClientID:      q.Get("client_id"),
			TokenEndpoint: q.Get("token_endpoint"),
			ExpiresAt:     q.Get("expires_at"),
		}
	}

	if tokenResult == nil && r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err == nil && len(body) > 0 {
			var t callbackTokenResult
			if json.Unmarshal(body, &t) == nil && t.AccessToken != "" {
				tokenResult = &t
			}
		}
	}

	if tokenResult == nil {
		if code := q.Get("code"); code != "" {
			log.Printf("[auth] exchanging authorization code for tokens")
			result, err := am.exchangeCode(session, code)
			if err != nil {
				am.failSession(session, "code exchange failed: "+err.Error())
				writeCallbackHTML(w, false, "Code exchange failed")
				return
			}
			tokenResult = result
		}
	}

	if tokenResult == nil || tokenResult.AccessToken == "" {
		am.failSession(session, "no token in callback")
		writeCallbackHTML(w, false, "No token received")
		return
	}

	am.completeSession(session, tokenResult)
	writeCallbackHTML(w, true, "")
}

func (am *AuthManager) handleExternalIdPRedirect(session *LoginSession, w http.ResponseWriter, r *http.Request) {
	issuerURL := r.URL.Query().Get("issuer_url")
	clientID := r.URL.Query().Get("client_id")
	scopes := r.URL.Query().Get("scopes")

	if issuerURL == "" || clientID == "" {
		am.failSession(session, "external_idp callback missing issuer_url or client_id")
		writeCallbackHTML(w, false, "Missing IdP configuration")
		return
	}

	log.Printf("[auth] External IdP login: issuer=%s client_id=%s", issuerURL, clientID)

	verifier, err := generateCodeVerifier()
	if err != nil {
		am.failSession(session, "generate PKCE verifier: "+err.Error())
		writeCallbackHTML(w, false, "Internal error")
		return
	}
	challenge := computeCodeChallenge(verifier)
	state := uuid.New().String()

	authEndpoint, tokenEndpoint, discErr := oidcDiscover(issuerURL)
	if discErr != nil {
		log.Printf("[auth] OIDC discovery failed, falling back to Azure AD pattern: %v", discErr)
		base := strings.TrimSuffix(issuerURL, "/v2.0")
		authEndpoint = base + "/oauth2/v2.0/authorize"
		tokenEndpoint = base + "/oauth2/v2.0/token"
	}

	session.mu.Lock()
	session.externalIdP = true
	session.externalIssuerURL = issuerURL
	session.externalClientID = clientID
	session.externalScopes = scopes
	session.externalState = state
	session.externalVerifier = verifier
	session.externalAuthEndpoint = authEndpoint
	session.externalTokenEndpoint = tokenEndpoint
	session.mu.Unlock()

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", externalIdPRedirectURI)
	params.Set("scope", scopes)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("response_mode", "query")

	if hint := r.URL.Query().Get("login_hint"); hint != "" {
		params.Set("login_hint", hint)
	}

	authURL := authEndpoint + "?" + params.Encode()
	writeExternalIdPPage(w, authURL)
}

func (am *AuthManager) handleExternalIdPCallback(session *LoginSession, w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	expectedState := session.externalState
	session.mu.Unlock()

	if state := r.URL.Query().Get("state"); state != expectedState {
		am.failSession(session, "external IdP state mismatch")
		writeCallbackHTML(w, false, "State mismatch")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		am.failSession(session, "no authorization code from external IdP")
		writeCallbackHTML(w, false, "No authorization code received")
		return
	}

	log.Printf("[auth] exchanging external IdP authorization code")

	session.mu.Lock()
	clientID := session.externalClientID
	verifier := session.externalVerifier
	scopes := session.externalScopes
	tokenEndpoint := session.externalTokenEndpoint
	session.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", externalIdPRedirectURI)
	form.Set("code_verifier", verifier)
	form.Set("scope", scopes)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.PostForm(tokenEndpoint, form)
	if err != nil {
		am.failSession(session, "external IdP token exchange: "+err.Error())
		writeCallbackHTML(w, false, "Token exchange failed")
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusOK {
		log.Printf("[auth] external IdP token exchange failed: status=%d body=%s", resp.StatusCode, string(body))
		am.failSession(session, fmt.Sprintf("external IdP token exchange failed (%d)", resp.StatusCode))
		writeCallbackHTML(w, false, "Token exchange failed")
		return
	}

	var azResult struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &azResult); err != nil {
		am.failSession(session, "parse external IdP token response: "+err.Error())
		writeCallbackHTML(w, false, "Token parse failed")
		return
	}

	if azResult.AccessToken == "" {
		am.failSession(session, "empty access token from external IdP")
		writeCallbackHTML(w, false, "No access token received")
		return
	}

	tokenResult := &callbackTokenResult{
		AccessToken:   azResult.AccessToken,
		RefreshToken:  azResult.RefreshToken,
		ClientID:      clientID,
		TokenEndpoint: tokenEndpoint,
		ExpiresIn:     azResult.ExpiresIn,
	}

	am.completeSession(session, tokenResult)
	writeCallbackHTML(w, true, "")
}

func (am *AuthManager) handleBuilderIDRedirect(session *LoginSession, w http.ResponseWriter, r *http.Request) {
	idcRegion := r.URL.Query().Get("idc_region")
	if idcRegion == "" {
		idcRegion = "us-east-1"
	}
	startURL := r.URL.Query().Get("issuer_url")
	if startURL == "" {
		startURL = "https://view.awsapps.com/start"
	}
	if idx := strings.Index(startURL, "#"); idx >= 0 {
		startURL = startURL[:idx]
	}
	startURL = strings.TrimRight(startURL, "/")
	oidcBase := "https://oidc." + idcRegion + ".amazonaws.com"
	tokenEndpoint := oidcBase + "/token"

	log.Printf("[auth] Builder ID login: region=%s oidc_base=%s start_url=%s", idcRegion, oidcBase, startURL)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	// Step 1: Register client
	regBody, _ := json.Marshal(map[string]interface{}{
		"clientName": "Kiro IDE",
		"clientType": "public",
		"scopes":     strings.Fields(builderIDScopes),
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  startURL,
	})
	regResp, err := httpClient.Post(oidcBase+"/client/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		am.failSession(session, "AWS OIDC register: "+err.Error())
		writeCallbackHTML(w, false, "Client registration failed")
		return
	}
	regRespBody, _ := io.ReadAll(io.LimitReader(regResp.Body, 32*1024))
	_ = regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK {
		log.Printf("[auth] AWS OIDC register failed: status=%d body=%s", regResp.StatusCode, string(regRespBody))
		am.failSession(session, "AWS OIDC register failed")
		writeCallbackHTML(w, false, "Client registration failed")
		return
	}
	var regResult struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	_ = json.Unmarshal(regRespBody, &regResult)
	if regResult.ClientID == "" {
		am.failSession(session, "empty clientId from registration")
		writeCallbackHTML(w, false, "Registration returned empty clientId")
		return
	}

	// Step 2: Start device authorization
	authBody, _ := json.Marshal(map[string]string{
		"clientId":     regResult.ClientID,
		"clientSecret": regResult.ClientSecret,
		"startUrl":     startURL,
	})
	authResp, err := httpClient.Post(oidcBase+"/device_authorization", "application/json", bytes.NewReader(authBody))
	if err != nil {
		am.failSession(session, "device_authorization: "+err.Error())
		writeCallbackHTML(w, false, "Device authorization failed")
		return
	}
	authRespBody, _ := io.ReadAll(io.LimitReader(authResp.Body, 32*1024))
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		log.Printf("[auth] device_authorization failed: status=%d body=%s", authResp.StatusCode, string(authRespBody))
		am.failSession(session, "device_authorization failed")
		writeCallbackHTML(w, false, "Device authorization failed")
		return
	}
	var daResult struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	_ = json.Unmarshal(authRespBody, &daResult)
	if daResult.DeviceCode == "" || daResult.UserCode == "" {
		am.failSession(session, "device_authorization missing deviceCode/userCode")
		writeCallbackHTML(w, false, "Device authorization incomplete")
		return
	}
	if daResult.Interval < 1 {
		daResult.Interval = 5
	}

	log.Printf("[auth] Builder ID device auth: user_code=%s verify_url=%s", daResult.UserCode, daResult.VerificationURIComplete)

	session.mu.Lock()
	session.builderID = true
	session.builderIDOIDCBase = oidcBase
	session.builderIDClientID = regResult.ClientID
	session.builderIDClientSecret = regResult.ClientSecret
	session.builderIDDeviceCode = daResult.DeviceCode
	session.builderIDUserCode = daResult.UserCode
	session.builderIDVerifyURI = daResult.VerificationURIComplete
	session.builderIDInterval = daResult.Interval
	session.builderIDTokenEndpoint = tokenEndpoint
	session.mu.Unlock()

	go am.pollBuilderIDDeviceCode(session)

	writeBuilderIDDevicePage(w, daResult.UserCode, daResult.VerificationURIComplete)
}

func (am *AuthManager) pollBuilderIDDeviceCode(session *LoginSession) {
	session.mu.Lock()
	oidcBase := session.builderIDOIDCBase
	clientID := session.builderIDClientID
	clientSecret := session.builderIDClientSecret
	deviceCode := session.builderIDDeviceCode
	interval := session.builderIDInterval
	tokenEndpoint := session.builderIDTokenEndpoint
	session.mu.Unlock()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
		}

		reqBody, _ := json.Marshal(map[string]string{
			"clientId":     clientID,
			"clientSecret": clientSecret,
			"deviceCode":   deviceCode,
			"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
		})
		resp, err := httpClient.Post(oidcBase+"/token", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			log.Printf("[auth] Builder ID poll error: %v", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var tokenData struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresIn    int    `json:"expiresIn"`
			}
			if err := json.Unmarshal(body, &tokenData); err != nil {
				am.failSession(session, "parse Builder ID token: "+err.Error())
				return
			}
			log.Printf("[auth] Builder ID device flow completed")
			tokenResult := &callbackTokenResult{
				AccessToken:   tokenData.AccessToken,
				RefreshToken:  tokenData.RefreshToken,
				ClientID:      clientID,
				ClientSecret:  clientSecret,
				TokenEndpoint: tokenEndpoint,
				ExpiresIn:     tokenData.ExpiresIn,
			}
			am.completeSession(session, tokenResult)
			return
		}

		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)

		switch errResp.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			ticker.Reset(time.Duration(interval+5) * time.Second)
			continue
		case "expired_token":
			am.failSession(session, "device code expired")
			return
		default:
			log.Printf("[auth] Builder ID poll failed: status=%d body=%s", resp.StatusCode, string(body))
			am.failSession(session, "Builder ID poll: "+errResp.Error)
			return
		}
	}
}

func (am *AuthManager) completeSession(session *LoginSession, tokenResult *callbackTokenResult) {
	session.mu.Lock()
	session.AccessToken = tokenResult.AccessToken
	session.RefreshToken = tokenResult.RefreshToken
	session.ClientID = tokenResult.ClientID
	session.ClientSecret = tokenResult.ClientSecret
	session.TokenEndpoint = tokenResult.TokenEndpoint
	session.ProfileArn = tokenResult.ProfileArn
	if tokenResult.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, tokenResult.ExpiresAt); err == nil {
			session.TokenExpiresAt = t
		}
	}
	if session.TokenExpiresAt.IsZero() {
		if tokenResult.ExpiresIn > 0 {
			session.TokenExpiresAt = time.Now().Add(time.Duration(tokenResult.ExpiresIn) * time.Second)
		} else {
			session.TokenExpiresAt = time.Now().Add(1 * time.Hour)
		}
	}
	session.Status = "completed"
	session.mu.Unlock()

	log.Printf("[auth] login completed: session=%s external_idp=%v builder_id=%v",
		session.ID, session.externalIdP, session.builderID)

	close(session.done)
	go func() { _ = session.server.Close() }()
}

func (am *AuthManager) exchangeCode(session *LoginSession, code string) (*callbackTokenResult, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", session.codeVerifier)
	form.Set("redirect_uri", fmt.Sprintf("http://localhost:%d", session.CallbackPort))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.PostForm(kiroTokenExchangeURL, form)
	if err != nil {
		return nil, fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var result callbackTokenResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse exchange response: %w", err)
	}
	return &result, nil
}

func (am *AuthManager) failSession(session *LoginSession, errMsg string) {
	session.mu.Lock()
	session.Status = "error"
	session.Error = errMsg
	session.mu.Unlock()

	select {
	case <-session.done:
	default:
		close(session.done)
	}
	go func() { _ = session.server.Close() }()
}

// oidcDiscover fetches the OIDC discovery document.
func oidcDiscover(issuerURL string) (authEndpoint, tokenEndpoint string, err error) {
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(discoveryURL)
	if err != nil {
		return "", "", fmt.Errorf("OIDC discovery request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("OIDC discovery status: %d", resp.StatusCode)
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&doc); err != nil {
		return "", "", fmt.Errorf("OIDC discovery parse: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return "", "", fmt.Errorf("OIDC discovery missing endpoints")
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, nil
}

func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func keysOf(vals url.Values) []string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	return keys
}

func writeCallbackHTML(w http.ResponseWriter, success bool, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if success {
		fmt.Fprint(w, `<!DOCTYPE html><html><body style="display:flex;justify-content:center;align-items:center;height:100vh;font-family:system-ui;background:#0a0a0a;color:#fff"><div style="text-align:center"><h1 style="font-size:48px;margin:0">&#10004;</h1><h2>Login Successful</h2><p style="color:#888">You can close this page.</p></div></body></html>`)
	} else {
		fmt.Fprintf(w, `<!DOCTYPE html><html><body style="display:flex;justify-content:center;align-items:center;height:100vh;font-family:system-ui;background:#0a0a0a;color:#fff"><div style="text-align:center"><h1 style="font-size:48px;margin:0">&#10008;</h1><h2>Login Failed</h2><p style="color:#888">%s</p></div></body></html>`, errMsg)
	}
}

func writeExternalIdPPage(w http.ResponseWriter, authURL string) {
	const tpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Enterprise SSO Login</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0a0a0a;color:#e5e5e5;min-height:100vh;display:flex;justify-content:center;align-items:center}
.card{background:#1a1a1a;border-radius:12px;padding:32px;max-width:560px;width:100%;box-shadow:0 4px 24px rgba(0,0,0,.5)}
h2{font-size:20px;margin-bottom:20px;color:#fff}
.step{margin:14px 0;padding:14px;background:#111;border-radius:8px;border-left:3px solid #3b82f6;font-size:14px;line-height:1.6}
.step-num{font-weight:700;color:#3b82f6;margin-right:4px}
code{background:#222;padding:2px 6px;border-radius:3px;font-size:13px;color:#93c5fd}
input[type="text"]{width:100%;padding:10px 12px;border:1px solid #333;border-radius:6px;font-size:14px;background:#111;color:#e5e5e5;outline:none;transition:border-color .2s}
input[type="text"]:focus{border-color:#3b82f6}
button{padding:10px 20px;border:none;border-radius:6px;font-size:14px;cursor:pointer;font-weight:500;transition:background .2s}
.btn-open{background:#3b82f6;color:#fff;display:inline-block;text-decoration:none;padding:10px 20px;border-radius:6px;font-weight:500}
.btn-open:hover{background:#60a5fa}
.btn-submit{background:#22c55e;color:#fff;margin-top:12px;width:100%}
.btn-submit:hover{background:#4ade80}
.btn-submit:disabled{background:#333;color:#666;cursor:not-allowed}
.error{color:#ef4444;font-size:13px;margin-top:8px;display:none}
.hint{color:#666;font-size:13px;margin-top:6px}
.spinner{display:none;text-align:center;padding:20px;color:#888}
</style>
</head>
<body>
<div class="card">
<h2>&#128274; Enterprise SSO Login</h2>
<div class="step">
<span class="step-num">Step 1:</span> Click the button below to open the Azure AD login page.
<br><br>
<a class="btn-open" id="openBtn" href="#" target="_blank" rel="noopener noreferrer">Open Azure AD Login &#8594;</a>
</div>
<div class="step">
<span class="step-num">Step 2:</span> Open F12 (DevTools) before the page redirects to <code>kiro://...</code>, then copy the full URL (including <code>code</code> and <code>state</code> params).
</div>
<div class="step">
<span class="step-num">Step 3:</span> Paste the URL below and submit.
<br><br>
<input type="text" id="callbackUrl" placeholder="kiro://kiro.oauth/callback?code=...&amp;state=..." autocomplete="off">
<div class="hint">URL should contain a <code>code=</code> parameter</div>
<div class="error" id="error"></div>
<br>
<button class="btn-submit" id="submitBtn" disabled>Submit</button>
</div>
<div class="spinner" id="spinner">&#9203; Exchanging tokens...</div>
</div>
<div class="result" id="result" style="display:none;text-align:center;padding:24px"></div>
<input type="hidden" id="authUrl" value="{{.}}">
<script>
(function(){
var authURL=document.getElementById('authUrl').value;
document.getElementById('openBtn').href=authURL;
var input=document.getElementById('callbackUrl');
var btn=document.getElementById('submitBtn');
var errEl=document.getElementById('error');
input.addEventListener('input',function(){
var v=input.value.trim();errEl.style.display='none';
if(!v){btn.disabled=true;return}
try{var u=new URL(v.replace(/^kiro:\/\//,'https://'));
btn.disabled=!u.searchParams.get('code');
if(!u.searchParams.get('code')){errEl.textContent='No code parameter found in URL';errEl.style.display='block'}}
catch(e){btn.disabled=true;errEl.textContent='Invalid URL format';errEl.style.display='block'}});
btn.addEventListener('click',function(){
var v=input.value.trim();
try{var u=new URL(v.replace(/^kiro:\/\//,'https://'));
var code=u.searchParams.get('code');var state=u.searchParams.get('state');
if(code){var p=new URLSearchParams();p.set('code',code);if(state)p.set('state',state);
document.querySelector('.card').style.display='none';document.getElementById('spinner').style.display='block';
fetch('/?'+p.toString()).then(function(r){return r.text()}).then(function(html){
document.getElementById('spinner').style.display='none';
var res=document.getElementById('result');res.style.display='block';
if(html.indexOf('&#10004;')>=0){res.innerHTML='<h1 style="font-size:48px">&#10004;</h1><h2 style="color:#22c55e">Login Successful</h2><p style="color:#888;margin-top:8px">You can close this page.</p>';}
else{res.innerHTML='<h1 style="font-size:48px">&#10008;</h1><h2 style="color:#ef4444">Login Failed</h2><p style="color:#888;margin-top:8px">Check server logs.</p>';}
}).catch(function(e){
document.getElementById('spinner').style.display='none';
document.querySelector('.card').style.display='block';
errEl.textContent='Request failed: '+e.message;errEl.style.display='block';
});}}
catch(e){errEl.textContent='Failed to parse URL';errEl.style.display='block'}});
})();
</script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := template.Must(template.New("extidp").Parse(tpl))
	_ = t.Execute(w, authURL)
}

func writeBuilderIDDevicePage(w http.ResponseWriter, userCode, verifyURL string) {
	const tpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Builder ID Login</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0a0a0a;color:#e5e5e5;min-height:100vh;display:flex;justify-content:center;align-items:center}
.card{background:#1a1a1a;border-radius:12px;padding:32px;max-width:480px;width:100%;box-shadow:0 4px 24px rgba(0,0,0,.5);text-align:center}
h2{font-size:20px;margin-bottom:20px;color:#fff}
.code{font-size:36px;font-weight:700;letter-spacing:4px;color:#60a5fa;background:#111;padding:16px 24px;border-radius:8px;margin:20px 0;font-family:'Courier New',monospace}
.step{margin:14px 0;padding:14px;background:#111;border-radius:8px;border-left:3px solid #3b82f6;font-size:14px;line-height:1.6;text-align:left}
.step-num{font-weight:700;color:#3b82f6;margin-right:4px}
.btn-open{background:#3b82f6;color:#fff;display:inline-block;text-decoration:none;padding:10px 20px;border-radius:6px;font-weight:500;font-size:14px}
.btn-open:hover{background:#60a5fa}
.hint{color:#888;font-size:13px;margin-top:16px}
.spinner{margin-top:16px;color:#888;font-size:14px}
</style>
</head>
<body>
<div class="card">
<h2>&#128274; AWS Builder ID Login</h2>
<div class="step">
<span class="step-num">Step 1:</span> Click the button below to open the AWS Builder ID authorization page.
<br><br>
<a class="btn-open" href="{{.VerifyURL}}" target="_blank" rel="noopener noreferrer">Open AWS Builder ID &#8594;</a>
</div>
<div class="step">
<span class="step-num">Step 2:</span> Enter the following verification code:
<div class="code">{{.UserCode}}</div>
</div>
<div class="step">
<span class="step-num">Step 3:</span> After completing AWS authorization, this page will close automatically.
</div>
<div class="spinner" id="spinner">&#9203; Waiting for authorization...</div>
<div class="hint" id="hint">Check the admin panel to confirm login status after authorization</div>
</div>
<script>
(function(){
  var spinner = document.getElementById('spinner');
  var hint = document.getElementById('hint');
  function poll(){
    fetch('/status').then(function(r){return r.json()}).then(function(d){
      if(d.status==='completed'){
        spinner.innerHTML='&#9989; Authorization complete!';
        spinner.style.color='#22c55e';
        hint.textContent='Page will close in 3 seconds...';
        setTimeout(function(){window.close();},3000);
      } else if(d.status==='error'||d.status==='expired'){
        spinner.innerHTML='&#10060; '+(d.error||'Login failed');
        spinner.style.color='#ef4444';
        hint.textContent='Please close this page and try again';
      } else {
        setTimeout(poll,3000);
      }
    }).catch(function(){setTimeout(poll,5000);});
  }
  setTimeout(poll,3000);
})();
</script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		UserCode  string
		VerifyURL string
	}{UserCode: userCode, VerifyURL: verifyURL}
	t := template.Must(template.New("builderid").Parse(tpl))
	_ = t.Execute(w, data)
}
