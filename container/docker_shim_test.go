package container

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shim is the only place where build-env injection and registry mapping are
// wired together, so exercise it with a fake kaniko sidecar. The real
// dockerfile-inject binary has its own parser tests; here a fake injector proves
// the shim passes the right keys and uses the derived Dockerfile.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}

func fakeInjector(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "dockerfile-inject")
	script := `#!/bin/sh
set -eu
in=""; out=""; keys=""
while [ $# -gt 0 ]; do
  case "$1" in
    -in) in=$2; shift 2 ;;
    -out) out=$2; shift 2 ;;
    -keys) keys=$2; shift 2 ;;
    *) shift ;;
  esac
done
cp "$in" "$out"
printf '%s' "$keys" > "$out.keys"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func startFakeSidecar(t *testing.T, shared, capture string) {
	t.Helper()
	script := `set -eu
shared=$1; capture=$2
while :; do
  for d in "$shared"/req/*/; do
    [ -f "$d/go" ] || continue
    [ -f "$d/done" ] && continue
    cp -a "${d%/}" "$capture"/ 2>/dev/null || true
    echo 0 > "$d/rc"
    : > "$d/done"
  done
  sleep 0.05
done`
	cmd := exec.Command("sh", "-c", script, "sidecar", shared, capture)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func runShim(t *testing.T, ctx, shared, injector string, env []string, args ...string) string {
	t.Helper()
	shim := filepath.Join(repoRoot(t), "container", "docker.sh")
	cmd := exec.Command("sh", append([]string{shim}, args...)...)
	cmd.Dir = ctx
	base := make([]string, 0, len(os.Environ())+len(env))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GRC_") {
			continue
		}
		base = append(base, e)
	}
	cmd.Env = append(base, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\n%s", err, out)
	}
	return string(out)
}

func readCaptured(t *testing.T, capture, name string) string {
	t.Helper()
	entries, err := os.ReadDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(capture, e.Name(), name)
		if b, err := os.ReadFile(path); err == nil {
			return string(b)
		}
	}
	t.Fatalf("captured %s not found in %s", name, capture)
	return ""
}

func hasPair(args, flag, value string) bool {
	lines := strings.Split(args, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == flag && lines[i+1] == value {
			return true
		}
	}
	return false
}

func hasLine(args, line string) bool {
	for _, got := range strings.Split(args, "\n") {
		if got == line {
			return true
		}
	}
	return false
}

func TestShimInjectsBuildEnvAndRegistryMaps(t *testing.T) {
	for _, command := range [][]string{{"build"}, {"buildx", "build"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			root := t.TempDir()
			ctx := filepath.Join(root, "ctx")
			shared := filepath.Join(root, "shared")
			capture := filepath.Join(root, "capture")
			for _, d := range []string{ctx, filepath.Join(shared, "req"), filepath.Join(shared, "kaniko", ".docker"), capture} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM alpine\nRUN true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			injector := fakeInjector(t, root)
			startFakeSidecar(t, shared, capture)

			maps := base64.StdEncoding.EncodeToString([]byte(
				"index.docker.io=http://docker.internal:8080/docker-group\n" +
					"ghcr.io=http://docker.internal:8080/docker-group\n"))
			env := []string{
				"GRC_SHARED=" + shared,
				"GRC_DOCKERFILE_INJECT=" + injector,
				"GRC_BUILD_ENV_KEYS=GOPROXY,PIP_INDEX_URL",
				"GOPROXY=http://kkrepo/go",
				"PIP_INDEX_URL=http://kkrepo/pypi",
				"GRC_REGISTRY_MAPS_B64=" + maps,
				"GRC_REGISTRY_MAP_SKIP_FALLBACK=1",
			}
			args := append(append([]string{}, command...), "-t", "example:test", ".")
			logs := runShim(t, ctx, shared, injector, env, args...)
			if !strings.Contains(logs, "build env injected: GOPROXY,PIP_INDEX_URL") {
				t.Fatalf("missing injection log:\n%s", logs)
			}

			got := readCaptured(t, capture, "args")
			for _, want := range []string{
				"--registry-mirror=docker.internal:8080/docker-group",
				"--registry-map=ghcr.io=docker.internal:8080/docker-group",
				"--insecure-registry=docker.internal:8080",
				"--skip-default-registry-fallback",
			} {
				if !hasLine(got, want) {
					t.Fatalf("args missing %q:\n%s", want, got)
				}
			}
			if !hasPair(got, "--build-arg", "GOPROXY=http://kkrepo/go") ||
				!hasPair(got, "--build-arg", "PIP_INDEX_URL=http://kkrepo/pypi") {
				t.Fatalf("build args missing:\n%s", got)
			}
			dockerfile := readCaptured(t, capture, "Dockerfile")
			if dockerfile != "FROM alpine\nRUN true\n" {
				t.Fatalf("derived Dockerfile = %q", dockerfile)
			}
			if keys := readCaptured(t, capture, "Dockerfile.keys"); keys != "GOPROXY,PIP_INDEX_URL" {
				t.Fatalf("injector keys = %q", keys)
			}
			if !strings.Contains(got, "--dockerfile="+filepath.Join(shared, "req")) {
				t.Fatalf("derived Dockerfile path missing:\n%s", got)
			}
		})
	}
}

func TestShimExplicitBuildArgWins(t *testing.T) {
	root := t.TempDir()
	ctx := filepath.Join(root, "ctx")
	shared := filepath.Join(root, "shared")
	capture := filepath.Join(root, "capture")
	for _, d := range []string{ctx, filepath.Join(shared, "req"), filepath.Join(shared, "kaniko", ".docker"), capture} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	injector := fakeInjector(t, root)
	startFakeSidecar(t, shared, capture)
	env := []string{
		"GRC_SHARED=" + shared,
		"GRC_DOCKERFILE_INJECT=" + injector,
		"GRC_BUILD_ENV_KEYS=GOPROXY",
		"GOPROXY=http://controller/default",
	}
	runShim(t, ctx, shared, injector, env, "build", "--build-arg", "GOPROXY=explicit", "-t", "example:test", ".")
	got := readCaptured(t, capture, "args")
	if !hasPair(got, "--build-arg", "GOPROXY=explicit") {
		t.Fatalf("explicit build arg missing:\n%s", got)
	}
	if strings.Contains(got, "GOPROXY=http://controller/default") {
		t.Fatalf("controller default overrode explicit build arg:\n%s", got)
	}
}

func TestShimPushReusesInjectedDockerfile(t *testing.T) {
	root := t.TempDir()
	ctx := filepath.Join(root, "ctx")
	shared := filepath.Join(root, "shared")
	capture := filepath.Join(root, "capture")
	for _, d := range []string{ctx, filepath.Join(shared, "req"), filepath.Join(shared, "kaniko", ".docker"), capture} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	injector := fakeInjector(t, root)
	startFakeSidecar(t, shared, capture)
	env := []string{
		"GRC_SHARED=" + shared,
		"GRC_DOCKERFILE_INJECT=" + injector,
		"GRC_BUILD_ENV_KEYS=GOPROXY",
		"GOPROXY=http://kkrepo/go",
	}
	runShim(t, ctx, shared, injector, env, "build", "-t", "example:test", ".")
	if err := os.RemoveAll(capture); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(capture, 0o755); err != nil {
		t.Fatal(err)
	}

	runShim(t, ctx, shared, injector, env, "push", "example:test")
	got := readCaptured(t, capture, "args")
	if !strings.Contains(got, "--dockerfile="+filepath.Join(shared, "req")) {
		t.Fatalf("push did not reuse injected Dockerfile:\n%s", got)
	}
	if !hasPair(got, "--build-arg", "GOPROXY=http://kkrepo/go") {
		t.Fatalf("push lost build args:\n%s", got)
	}
}

func TestShimWithoutBuildEnvKeepsOriginalDockerfile(t *testing.T) {
	root := t.TempDir()
	ctx := filepath.Join(root, "ctx")
	shared := filepath.Join(root, "shared")
	capture := filepath.Join(root, "capture")
	for _, d := range []string{ctx, filepath.Join(shared, "req"), filepath.Join(shared, "kaniko", ".docker"), capture} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dockerfile := filepath.Join(ctx, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	injector := fakeInjector(t, root)
	startFakeSidecar(t, shared, capture)
	env := []string{
		"GRC_SHARED=" + shared,
		"GRC_DOCKERFILE_INJECT=" + injector,
	}
	runShim(t, ctx, shared, injector, env, "build", "-t", "example:test", ".")
	got := readCaptured(t, capture, "args")
	if !hasLine(got, "--dockerfile="+dockerfile) {
		t.Fatalf("original Dockerfile path missing:\n%s", got)
	}
	if strings.Contains(got, "Dockerfile.keys") {
		t.Fatalf("injector ran without buildEnv:\n%s", got)
	}
}
