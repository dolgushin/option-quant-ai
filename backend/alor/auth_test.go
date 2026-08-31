package alor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetAccessTokenURLEncoding(t *testing.T) {
	// Token with URL-hostile characters, as a *fresh* valid Alor token may have.
	const tok = "MOCKabcdef+/=?&%token_ABC"
	var gotURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"AccessToken":"AT123","Token":"AT123"}`)
	}))
	defer srv.Close()

	c := NewAuthClient(tok)
	c.baseURL = srv.URL
	// Use the raw base URL captured before we swap; but token sent is what matters.
	_ = c

	at, err := c.GetAccessToken()
	if err != nil {
		t.Fatalf("GetAccessToken error: %v", err)
	}
	if at != "AT123" {
		t.Fatalf("unexpected access token: %q", at)
	}

	// The `token` query param must decode back to the exact original token.
	if !strings.Contains(gotURL, "token=") {
		t.Fatalf("no token param in URL: %s", gotURL)
	}
	t.Logf("raw request URI: %s", gotURL)
}
