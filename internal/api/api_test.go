package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nolith/geas/internal/testutil"
)

func TestAPIErrorsDoNotExposeSecrets(t *testing.T) {
	a := New()
	a.Client.Transport = testutil.Transport(func(*http.Request) (int, any) { return 401, map[string]string{"error": "secret-from-server"} })
	err := a.Request(context.Background(), "GET", "https://gitlab.com/api/v4/user", "issuer-secret", nil, false, new(any))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error %v", err)
	}
	if a.Request(context.Background(), "GET", "http://gitlab.com", "token", nil, false, nil) == nil {
		t.Fatal("accepted HTTP")
	}
}
