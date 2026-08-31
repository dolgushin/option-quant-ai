package alor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type AuthClient struct {
	mu           sync.Mutex
	refreshToken string
	accessToken  string
	expiresAt    time.Time
	httpClient   *http.Client
	baseURL      string
}

type TokenResponse struct {
	AccessToken string `json:"accessToken"`
	Token       string `json:"token"`
}

func NewAuthClient(refreshToken string) *AuthClient {
	return &AuthClient{
		refreshToken: refreshToken,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		baseURL:      "https://oauth.alor.ru",
	}
}

// SetRefreshToken updates the refresh token at runtime (e.g. after the user saves
// it through the UI) and invalidates the cached access token so the next request
// re-authenticates with the new credentials.
func (a *AuthClient) SetRefreshToken(refreshToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshToken = refreshToken
	a.accessToken = ""
	a.expiresAt = time.Time{}
}

// HasRefreshToken reports whether a refresh token is configured.
func (a *AuthClient) HasRefreshToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshToken != ""
}

// ValidateRefreshToken tries to exchange the refresh token for an access token
// and reports whether the token is valid.
func (a *AuthClient) ValidateRefreshToken() (bool, string) {
	_, err := a.GetAccessToken()
	if err != nil {
		return false, err.Error()
	}
	return true, "Refresh token is valid"
}

func (a *AuthClient) GetAccessToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// If token is valid for at least another 2 minutes, return cached
	if a.accessToken != "" && time.Until(a.expiresAt) > 2*time.Minute {
		return a.accessToken, nil
	}

	if a.refreshToken == "" {
		return "", fmt.Errorf("alor refresh token is not configured")
	}

	url := fmt.Sprintf("%s/refresh?token=%s", a.baseURL, url.QueryEscape(a.refreshToken))
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to execute Alor auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := ""
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		body = strings.TrimSpace(buf.String())
		if body == "" {
			body = "no response body"
		}
		return "", fmt.Errorf("alor auth failed with status code %d: %s", resp.StatusCode, body)
	}

	var tokenRes TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenRes); err != nil {
		// Alor sometimes returns raw token string or JSON
		// Let's handle both cases if needed
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		tokenStr := buf.String()
		if tokenStr != "" {
			a.accessToken = tokenStr
			a.expiresAt = time.Now().Add(25 * time.Minute)
			return a.accessToken, nil
		}
		return "", fmt.Errorf("failed to decode Alor token response: %w", err)
	}

	token := tokenRes.AccessToken
	if token == "" {
		token = tokenRes.Token
	}
	if token == "" {
		return "", fmt.Errorf("received empty access token from Alor")
	}

	a.accessToken = token
	a.expiresAt = time.Now().Add(25 * time.Minute) // Alor tokens typically valid for 30 mins
	return a.accessToken, nil
}
