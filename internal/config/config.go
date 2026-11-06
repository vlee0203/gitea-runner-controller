// Package config loads the controller configuration from the environment plus
// the runner template file (expected to be mounted from a ConfigMap).
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// ResourceLimits mirrors the subset of Kubernetes resource requirements a
// runner template may set.
type ResourceLimits struct {
	CPU              string `yaml:"cpu"`
	Memory           string `yaml:"memory"`
	EphemeralStorage string `yaml:"ephemeralStorage"`
}

// Template is one runner template. Every runner Pod is created from exactly one
// template: its image, labels, resources, timeout and default environment.
type Template struct {
	Name          string            `yaml:"name"`
	Labels        []string          `yaml:"labels"`
	Image         string            `yaml:"image"`
	Resources     ResourceLimits    `yaml:"resources"`
	MaxConcurrent int               `yaml:"maxConcurrent"`
	Timeout       string            `yaml:"timeout"`
	Env           map[string]string `yaml:"env"`
	// BuildEnv is exported to the runner and injected into every Dockerfile
	// stage as ARG declarations (values travel as --build-arg), so package
	// managers inside image builds use the same proxies as run: steps.
	BuildEnv map[string]string `yaml:"buildEnv"`
	// RegistryMaps redirects kaniko base-image pulls. Keys are original
	// registries; values are kkRepo group/proxy addresses, optionally with a
	// path prefix.
	RegistryMaps map[string]string `yaml:"registryMaps"`
}

type templatesFile struct {
	Env                         map[string]string `yaml:"env"`
	BuildEnv                    map[string]string `yaml:"buildEnv"`
	RegistryMaps                map[string]string `yaml:"registryMaps"`
	SkipDefaultRegistryFallback bool              `yaml:"skipDefaultRegistryFallback"`
	Templates                   []Template        `yaml:"templates"`
}

// Config is the resolved controller configuration.
type Config struct {
	GiteaURL string
	// GiteaAPIToken is a user API token with admin Actions read/runner manage
	// permissions. The controller uses it to list jobs, create registration
	// tokens, and clean up its own runner records.
	GiteaAPIToken string
	WebhookSecret string

	ListenAddr      string
	TargetNamespace string
	ImagePullSecret string
	KanikoImage     string

	CacheServerURL    string
	CacheServerSecret string
	ExecutorCAPath    string

	ReconcileInterval time.Duration
	IdleRunnerTimeout time.Duration
	ShutdownTimeout   time.Duration

	// JobEnv is merged into every template's Env; template Env wins on conflict.
	JobEnv    map[string]string
	Templates []Template

	// BuildEnv and RegistryMaps are top-level defaults; template values win.
	BuildEnv                    map[string]string
	RegistryMaps                map[string]string
	SkipDefaultRegistryFallback bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrFile(valueKey, fileKey string) (string, error) {
	if v := os.Getenv(valueKey); v != "" {
		return v, nil
	}
	path := os.Getenv(fileKey)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileKey, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Load reads configuration from the environment. Runner templates come from the
// file named by TEMPLATES_FILE.
func Load() (*Config, error) {
	c := &Config{
		GiteaURL:          strings.TrimRight(env("GITEA_INSTANCE_URL", ""), "/"),
		ListenAddr:        env("LISTEN_ADDR", ":8080"),
		TargetNamespace:   env("TARGET_NAMESPACE", ""),
		ImagePullSecret:   env("IMAGE_PULL_SECRET", ""),
		KanikoImage:       env("KANIKO_IMAGE", "ghcr.io/osscontainertools/kaniko:v1.28.6-debug"),
		CacheServerURL:    env("CACHE_SERVER_URL", ""),
		CacheServerSecret: env("CACHE_SERVER_SECRET", ""),
		ExecutorCAPath:    env("EXECUTOR_CA_PATH", ""),
		ReconcileInterval: 30 * time.Second,
		IdleRunnerTimeout: 15 * time.Minute,
		ShutdownTimeout:   30 * time.Second,
	}
	var err error
	if c.GiteaAPIToken, err = envOrFile("GITEA_API_TOKEN", "GITEA_API_TOKEN_FILE"); err != nil {
		return nil, err
	}
	if c.WebhookSecret, err = envOrFile("WEBHOOK_SECRET", "WEBHOOK_SECRET_FILE"); err != nil {
		return nil, err
	}

	if v := env("RECONCILE_INTERVAL", ""); v != "" {
		if c.ReconcileInterval, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("RECONCILE_INTERVAL: %w", err)
		}
	}
	if v := env("IDLE_RUNNER_TIMEOUT", ""); v != "" {
		if c.IdleRunnerTimeout, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("IDLE_RUNNER_TIMEOUT: %w", err)
		}
	}
	if v := env("SHUTDOWN_TIMEOUT", ""); v != "" {
		if c.ShutdownTimeout, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
	}

	if c.GiteaURL == "" {
		return nil, fmt.Errorf("GITEA_INSTANCE_URL is required")
	}
	if u, err := url.Parse(c.GiteaURL); err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("GITEA_INSTANCE_URL is not a valid URL: %q", c.GiteaURL)
	}
	if c.GiteaAPIToken == "" {
		return nil, fmt.Errorf("GITEA_API_TOKEN or GITEA_API_TOKEN_FILE is required")
	}
	if c.TargetNamespace == "" {
		return nil, fmt.Errorf("TARGET_NAMESPACE is required")
	}

	pf := env("TEMPLATES_FILE", "/etc/gitea-runner-controller/templates.yaml")
	b, err := os.ReadFile(pf)
	if err != nil {
		return nil, fmt.Errorf("read templates file %s: %w", pf, err)
	}
	var tf templatesFile
	if err := yaml.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("parse templates file %s: %w", pf, err)
	}
	if len(tf.Templates) == 0 {
		return nil, fmt.Errorf("templates file %s defines no templates", pf)
	}
	if err := validateEnv("top-level env", tf.Env); err != nil {
		return nil, err
	}
	if err := validateBuildEnv("top-level buildEnv", tf.BuildEnv); err != nil {
		return nil, err
	}
	topMaps, err := normalizeRegistryMaps("top-level registryMaps", tf.RegistryMaps)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range tf.Templates {
		t := &tf.Templates[i]
		if t.Name == "" || len(t.Labels) == 0 || t.Image == "" {
			return nil, fmt.Errorf("template %d: name, labels and image are required", i)
		}
		if errs := validation.IsDNS1123Label(t.Name); len(errs) > 0 {
			return nil, fmt.Errorf("template name %q is not a valid label: %s", t.Name, strings.Join(errs, "; "))
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("template %q is defined more than once", t.Name)
		}
		seen[t.Name] = true
		if t.MaxConcurrent <= 0 {
			return nil, fmt.Errorf("template %q: maxConcurrent must be positive", t.Name)
		}
		if t.Timeout == "" {
			t.Timeout = "1h"
		}
		if d, err := time.ParseDuration(t.Timeout); err != nil || d <= 0 {
			return nil, fmt.Errorf("template %q: invalid timeout %q", t.Name, t.Timeout)
		}
		if err := validateResources(t.Name, t.Resources); err != nil {
			return nil, err
		}
		if err := validateEnv("template "+t.Name+" env", t.Env); err != nil {
			return nil, err
		}
		if err := validateBuildEnv("template "+t.Name+" buildEnv", t.BuildEnv); err != nil {
			return nil, err
		}
		maps, err := normalizeRegistryMaps("template "+t.Name+" registryMaps", t.RegistryMaps)
		if err != nil {
			return nil, err
		}
		t.RegistryMaps = maps
	}
	c.JobEnv = tf.Env
	c.BuildEnv = tf.BuildEnv
	c.RegistryMaps = topMaps
	c.SkipDefaultRegistryFallback = tf.SkipDefaultRegistryFallback
	c.Templates = tf.Templates
	return c, nil
}

func validateEnv(where string, env map[string]string) error {
	for k, v := range env {
		if errs := validation.IsEnvVarName(k); len(errs) > 0 {
			return fmt.Errorf("%s: invalid variable name %q: %s", where, k, strings.Join(errs, "; "))
		}
		if strings.ContainsAny(v, "\x00\n") {
			return fmt.Errorf("%s: value of %q contains a newline or NUL", where, k)
		}
	}
	return nil
}

// validateBuildEnv rejects variables that would change how the build itself
// runs (shell/PATH/linker) or collide with controller-internal plumbing. Package
// manager endpoints are the intended content.
func validateBuildEnv(where string, env map[string]string) error {
	if err := validateEnv(where, env); err != nil {
		return err
	}
	denied := map[string]bool{
		"PATH": true, "HOME": true, "SHELL": true, "IFS": true, "ENV": true,
		"BASH_ENV": true, "CDPATH": true, "GLOBIGNORE": true, "PS4": true,
		"PROMPT_COMMAND": true, "LD_PRELOAD": true, "LD_LIBRARY_PATH": true,
	}
	for k := range env {
		if denied[k] {
			return fmt.Errorf("%s: %q cannot be injected into image builds", where, k)
		}
		if strings.HasPrefix(k, "GRC_") {
			return fmt.Errorf("%s: %q is reserved for controller plumbing", where, k)
		}
	}
	return nil
}

// normalizeRegistryMaps validates and canonicalizes kaniko registry mappings.
// Docker Hub is stored as index.docker.io, matching go-containerregistry's
// normalization; a target without a scheme defaults to https.
func normalizeRegistryMaps(where string, in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for rawKey, rawValue := range in {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "docker.io" {
			key = "index.docker.io"
		}
		if key == "" || key == "*" || strings.Contains(key, "://") ||
			strings.ContainsAny(key, " \t\r\n/") {
			return nil, fmt.Errorf("%s: invalid original registry %q", where, rawKey)
		}
		u, err := url.Parse("//" + key)
		if err != nil || u.Host == "" || u.Path != "" || u.User != nil {
			return nil, fmt.Errorf("%s: invalid original registry %q", where, rawKey)
		}

		value := strings.TrimSpace(rawValue)
		if value == "" || strings.ContainsAny(value, " \t\r\n;=") {
			return nil, fmt.Errorf("%s: registry %q has an invalid target", where, rawKey)
		}
		if !strings.Contains(value, "://") {
			value = "https://" + value
		}
		target, err := url.Parse(value)
		if err != nil || target.Host == "" || target.User != nil ||
			target.RawQuery != "" || target.Fragment != "" ||
			(target.Scheme != "http" && target.Scheme != "https") {
			return nil, fmt.Errorf("%s: registry %q has an invalid target %q", where, rawKey, rawValue)
		}
		value = strings.TrimRight(value, "/")
		if existing, ok := out[key]; ok && existing != value {
			return nil, fmt.Errorf("%s: registry %q is defined more than once", where, key)
		}
		out[key] = value
	}
	return out, nil
}

func validateResources(template string, r ResourceLimits) error {
	for name, value := range map[string]string{
		"cpu": r.CPU, "memory": r.Memory, "ephemeralStorage": r.EphemeralStorage,
	} {
		if value == "" {
			continue
		}
		if _, err := resource.ParseQuantity(value); err != nil {
			return fmt.Errorf("template %q: invalid %s quantity %q: %w", template, name, value, err)
		}
	}
	return nil
}

// TemplateFor returns the smallest template whose labels cover every label a job
// asks for. Gitea matches a job when all of its runs-on labels exist on the
// runner, so the template is the superset; the smallest superset avoids sending
// a generic job to a more specialized (for example build) template.
func (c *Config) TemplateFor(jobLabels []string) *Template {
	var best *Template
	bestLen := 0
	for i := range c.Templates {
		t := &c.Templates[i]
		have := make(map[string]bool, len(t.Labels))
		for _, l := range t.Labels {
			have[l] = true
		}
		ok := true
		for _, l := range jobLabels {
			if !have[l] {
				ok = false
				break
			}
		}
		if ok && (best == nil || len(t.Labels) < bestLen) {
			best = t
			bestLen = len(t.Labels)
		}
	}
	return best
}

// TemplateEnv returns the environment every runner of this template receives.
func (c *Config) TemplateEnv(t Template) map[string]string {
	out := make(map[string]string, len(c.JobEnv)+len(t.Env))
	for k, v := range c.JobEnv {
		out[k] = v
	}
	for k, v := range t.Env {
		out[k] = v
	}
	return out
}

// TemplateBuildEnv merges the controller defaults with a template's build env.
func (c *Config) TemplateBuildEnv(t Template) map[string]string {
	out := make(map[string]string, len(c.BuildEnv)+len(t.BuildEnv))
	for k, v := range c.BuildEnv {
		out[k] = v
	}
	for k, v := range t.BuildEnv {
		out[k] = v
	}
	return out
}

// TemplateRegistryMaps merges the controller defaults with a template's maps.
func (c *Config) TemplateRegistryMaps(t Template) map[string]string {
	out := make(map[string]string, len(c.RegistryMaps)+len(t.RegistryMaps))
	for k, v := range c.RegistryMaps {
		out[k] = v
	}
	for k, v := range t.RegistryMaps {
		out[k] = v
	}
	return out
}

// ProxyEnv reports the proxy environment for executor pods, or nil when no proxy
// is set on the controller.
func (c *Config) ProxyEnv() map[string]string {
	inCluster := c.noProxyHosts()
	httpProxy, _ := envOrFile("HTTP_PROXY", "HTTP_PROXY_FILE")
	httpsProxy, _ := envOrFile("HTTPS_PROXY", "HTTPS_PROXY_FILE")
	if httpProxy == "" {
		httpProxy = env("http_proxy", "")
	}
	if httpsProxy == "" {
		httpsProxy = env("https_proxy", "")
	}
	noProxy := mergeNoProxy(env("NO_PROXY", os.Getenv("no_proxy")), inCluster...)
	always := map[string]string{
		"NO_PROXY":              noProxy,
		"no_proxy":              noProxy,
		"GRC_PLATFORM_NO_PROXY": strings.Join(inCluster, ","),
	}
	if httpProxy == "" && httpsProxy == "" {
		return always
	}
	out := map[string]string{
		"NO_PROXY":              noProxy,
		"no_proxy":              noProxy,
		"NODE_USE_ENV_PROXY":    "1",
		"GRC_PLATFORM_NO_PROXY": always["GRC_PLATFORM_NO_PROXY"],
	}
	if httpProxy != "" {
		out["HTTP_PROXY"], out["http_proxy"] = httpProxy, httpProxy
	}
	if httpsProxy != "" {
		out["HTTPS_PROXY"], out["https_proxy"] = httpsProxy, httpsProxy
	}
	return out
}

func (c *Config) noProxyHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1", "kubernetes.default.svc", ".svc", ".svc.cluster.local"}
	for _, raw := range []string{c.GiteaURL, c.CacheServerURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// mergeNoProxy appends the missing entries to a NO_PROXY list, preserving what
// the operator set and keeping each entry once.
func mergeNoProxy(base string, extra ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, list := range append([]string{base}, extra...) {
		for _, e := range strings.Split(list, ",") {
			e = strings.TrimSpace(e)
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return strings.Join(out, ",")
}

// CacheConfigYAML is the runner config fragment that points an executor job at
// the shared cache server.
func (c *Config) CacheConfigYAML() string {
	if c.CacheServerURL == "" || c.CacheServerSecret == "" {
		return ""
	}
	return fmt.Sprintf("cache:\n  external_server: %q\n  external_secret: %q\n",
		c.CacheServerURL, c.CacheServerSecret)
}
