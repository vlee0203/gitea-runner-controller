package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplates(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "templates.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITEA_INSTANCE_URL", "https://gitea.example.com")
	t.Setenv("GITEA_API_TOKEN", "api-token")
	t.Setenv("TARGET_NAMESPACE", "ci")
	t.Setenv("TEMPLATES_FILE", writeTemplates(t, `
env:
  PIP_INDEX_URL: https://kkrepo.example/repository/pypi-group/simple
templates:
  - name: standard
    labels: [self-hosted, ubuntu-latest]
    image: example/runner:1
    maxConcurrent: 2
  - name: heavy
    labels: [self-hosted, ubuntu-latest, heavy]
    image: example/runner:1
    maxConcurrent: 1
`))
}

func TestLoadAndTemplateSelection(t *testing.T) {
	minimalEnv(t)
	t.Setenv("CACHE_SERVER_SECRET", "s3cret")
	t.Setenv("KANIKO_IMAGE", "kaniko:test")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "s3cret" {
		t.Fatalf("CacheServerSecret = %q", c.CacheServerSecret)
	}
	if len(c.Templates) != 2 || c.Templates[0].Timeout != "1h" {
		t.Fatalf("templates = %+v", c.Templates)
	}
	if got := c.TemplateFor([]string{"self-hosted", "ubuntu-latest"}); got == nil || got.Name != "standard" {
		t.Fatalf("standard match = %+v", got)
	}
	// A plain `runs-on: ubuntu-latest` must match the standard template: the
	// template is the superset of the job's labels.
	if got := c.TemplateFor([]string{"ubuntu-latest"}); got == nil || got.Name != "standard" {
		t.Fatalf("plain ubuntu-latest match = %+v", got)
	}
	// A job asking for the extra label matches only the heavier template.
	if got := c.TemplateFor([]string{"self-hosted", "ubuntu-latest", "heavy"}); got == nil || got.Name != "heavy" {
		t.Fatalf("heavy match = %+v", got)
	}
	if got := c.TemplateFor([]string{"windows"}); got != nil {
		t.Fatalf("unexpected match: %+v", got)
	}
	env := c.TemplateEnv(c.Templates[1])
	if env["PIP_INDEX_URL"] == "" {
		t.Fatal("top-level env was not merged into template")
	}
}

func TestLoadRejectsIncompleteTemplates(t *testing.T) {
	minimalEnv(t)
	t.Setenv("TEMPLATES_FILE", writeTemplates(t, `
templates:
  - name: broken
    image: example/runner:1
`))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "labels") {
		t.Fatalf("err = %v, want labels validation", err)
	}
}

func TestLoadRejectsInvalidResourceAndEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("TEMPLATES_FILE", writeTemplates(t, `
templates:
  - name: bad
    labels: [self-hosted]
    image: example/runner:1
    maxConcurrent: 1
    resources:
      cpu: not-a-quantity
`))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "invalid cpu") {
		t.Fatalf("resource err = %v", err)
	}

	t.Setenv("TEMPLATES_FILE", writeTemplates(t, `
templates:
  - name: bad
    labels: [self-hosted]
    image: example/runner:1
    maxConcurrent: 1
    env:
      "not a name": value
`))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "invalid variable name") {
		t.Fatalf("env err = %v", err)
	}
}

func TestLoadRequiresAPIToken(t *testing.T) {
	minimalEnv(t)
	t.Setenv("GITEA_API_TOKEN", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GITEA_API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "example.org")
	c := &Config{
		GiteaURL:       "https://gitea.example.com",
		CacheServerURL: "http://gitea-runner-controller-cache:8088/",
	}
	bare := c.ProxyEnv()
	if _, ok := bare["HTTP_PROXY"]; ok {
		t.Fatalf("no proxy configured, got %v", bare)
	}
	if !strings.Contains(bare["GRC_PLATFORM_NO_PROXY"], ".svc.cluster.local") {
		t.Fatalf("GRC_PLATFORM_NO_PROXY = %q", bare["GRC_PLATFORM_NO_PROXY"])
	}

	t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
	got := c.ProxyEnv()
	if got["HTTPS_PROXY"] != "http://proxy.corp:3128" || got["https_proxy"] != "http://proxy.corp:3128" {
		t.Errorf("HTTPS_PROXY not passed through: %v", got)
	}
	if got["NODE_USE_ENV_PROXY"] != "1" {
		t.Errorf("NODE_USE_ENV_PROXY missing: %v", got)
	}
	np := got["NO_PROXY"]
	for _, want := range []string{"example.org", "gitea.example.com", "gitea-runner-controller-cache", ".svc.cluster.local"} {
		if !strings.Contains(np, want) {
			t.Errorf("NO_PROXY missing %q: %s", want, np)
		}
	}
}
