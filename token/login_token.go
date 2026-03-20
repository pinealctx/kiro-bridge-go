package token

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// LoginToken holds credentials obtained from the built-in PKCE login flow.
type LoginToken struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	TokenEndpoint string    `json:"token_endpoint"`
	ExpiresAt     time.Time `json:"expires_at"`
	IsExternalIdP bool      `json:"is_external_idp"`
	RefreshScope  string    `json:"refresh_scope,omitempty"`
	ProfileArn    string    `json:"profile_arn,omitempty"`
}

// ExtractRefreshScope parses the JWT access_token to build the correct OAuth2 scope
// for token refresh. Azure AD requires resource-specific scopes (e.g.
// api://app-id/permission) rather than generic OIDC scopes.
// Returns empty string if extraction fails.
func ExtractRefreshScope(accessToken string) string {
	parts := strings.SplitN(accessToken, ".", 3)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
		Scp string          `json:"scp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	var aud string
	if err := json.Unmarshal(claims.Aud, &aud); err != nil {
		var auds []string
		if err := json.Unmarshal(claims.Aud, &auds); err != nil || len(auds) == 0 {
			return ""
		}
		aud = auds[0]
	}

	if aud == "" || claims.Scp == "" {
		return ""
	}

	scpItems := strings.Fields(claims.Scp)
	result := make([]string, 0, len(scpItems)+1)
	for _, s := range scpItems {
		result = append(result, aud+"/"+s)
	}
	result = append(result, "offline_access")
	return strings.Join(result, " ")
}

// IsAWSIdCEndpoint returns true if the token endpoint belongs to AWS IAM Identity Center.
func IsAWSIdCEndpoint(endpoint string) bool {
	return strings.Contains(endpoint, "oidc.") && strings.Contains(endpoint, ".amazonaws.com")
}
