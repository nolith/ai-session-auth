package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nolith/geas/internal/testutil"
)

// Refused before reading stdin or touching any file.
func TestSaveGitLabIssuerRefusedWithGlabIssuer(t *testing.T) {
	c := testutil.Config()
	c.GitLab = testutil.GlabConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	code, err := execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--stdin"}, nil)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "glab") {
		t.Fatalf("save-gitlab-issuer: %d %v", code, err)
	}
}
