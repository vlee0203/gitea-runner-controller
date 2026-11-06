package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testSpec() Spec {
	return Spec{
		Name:      "grc-standard-abc",
		Template:  "standard",
		Image:     "runner:test",
		Labels:    []string{"self-hosted", "ubuntu-latest"},
		Resources: corev1.ResourceRequirements{},
		Runner: Runner{
			ID: 382, UUID: "uuid-382", Token: "runner-token", Name: "grc-standard-abc",
			Address: "https://gitea.example.com", Labels: []string{"self-hosted", "ubuntu-latest"},
		},
		ProxyEnv: map[string]string{"NO_PROXY": ".svc"},
		JobEnv:   map[string]string{"PIP_INDEX_URL": "https://kkrepo/pypi"},
	}
}

func TestStartWritesRegistrationFileAndPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ci", nil)
	if err := m.Start(context.Background(), testSpec()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pod, err := cs.CoreV1().Pods("ci").Get(context.Background(), "grc-standard-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels[ManagedByLabel] != ManagedByValue || pod.Labels[TemplateLabel] != "standard" || pod.Labels[RunnerIDLabel] != "382" {
		t.Fatalf("labels = %v", pod.Labels)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("runner Pod must not mount a service account token")
	}
	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == RunnerContainerName {
			runner = &pod.Spec.Containers[i]
		}
	}
	if runner == nil {
		t.Fatal("runner container missing")
	}
	env := map[string]string{}
	for _, e := range runner.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/data" || env["PIP_INDEX_URL"] != "https://kkrepo/pypi" {
		t.Fatalf("env = %v", env)
	}
	if env["GRC_BUILD_ENV_KEYS"] != "" || env["GRC_REGISTRY_MAPS_B64"] != "" {
		t.Fatalf("empty build config should not set controller env: %v", env)
	}
	raw, err := base64.StdEncoding.DecodeString(env["GRC_RUNNER_FILE"])
	if err != nil {
		t.Fatalf("decode GRC_RUNNER_FILE: %v", err)
	}
	var reg registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("decode .runner: %v", err)
	}
	if reg.ID != 382 || reg.UUID != "uuid-382" || reg.Token != "runner-token" || reg.Address != "https://gitea.example.com" {
		t.Fatalf("registration = %+v", reg)
	}
	if len(runner.Command) != 3 || runner.Command[2] == "" {
		t.Fatalf("command = %v", runner.Command)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "runner-files" {
		t.Fatalf("init containers = %+v", pod.Spec.InitContainers)
	}
	if n := len(cs.Actions()); n == 0 {
		t.Fatal("expected Pod creation action")
	}
	secs, err := cs.CoreV1().Secrets("ci").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secs.Items) != 0 {
		t.Fatalf("executor must not create Secrets: %+v", secs.Items)
	}
}

func TestBuildTemplateAddsKanikoSidecarAndSharedContext(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ci", nil)
	s := testSpec()
	s.Template = "build"
	s.KanikoImage = "kaniko:test"
	if err := m.Start(context.Background(), s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pod, err := cs.CoreV1().Pods("ci").Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var runner, kaniko *corev1.Container
	for i := range pod.Spec.Containers {
		switch pod.Spec.Containers[i].Name {
		case RunnerContainerName:
			runner = &pod.Spec.Containers[i]
		case "kaniko":
			kaniko = &pod.Spec.Containers[i]
		}
	}
	if runner == nil || kaniko == nil {
		t.Fatalf("containers = %+v", pod.Spec.Containers)
	}
	if !hasMount(runner, "runner-data") || !hasMount(kaniko, "runner-data") || !hasMount(kaniko, "shared") {
		t.Fatalf("missing shared mounts: runner=%v kaniko=%v", runner.VolumeMounts, kaniko.VolumeMounts)
	}
	if !hasEnv(runner, "GRC_SHARED", "/shared") {
		t.Fatalf("runner GRC_SHARED missing: %v", runner.Env)
	}
}

func TestBuildEnvAndRegistryMaps(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ci", nil)
	s := testSpec()
	s.KanikoImage = "kaniko:test"
	s.BuildEnv = map[string]string{
		"GOPROXY":             "http://kkrepo/go",
		"PIP_INDEX_URL":       "http://kkrepo/pypi",
		"NPM_CONFIG_REGISTRY": "http://kkrepo/npm",
	}
	s.RegistryMaps = map[string]string{
		"index.docker.io": "https://docker.example",
		"ghcr.io":         "https://docker.example",
	}
	s.SkipDefaultRegistryFallback = true
	if err := m.Start(context.Background(), s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pod, err := cs.CoreV1().Pods("ci").Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var runner, kaniko *corev1.Container
	for i := range pod.Spec.Containers {
		switch pod.Spec.Containers[i].Name {
		case RunnerContainerName:
			runner = &pod.Spec.Containers[i]
		case "kaniko":
			kaniko = &pod.Spec.Containers[i]
		}
	}
	if runner == nil || kaniko == nil {
		t.Fatalf("containers = %+v", pod.Spec.Containers)
	}
	env := map[string]string{}
	for _, e := range runner.Env {
		env[e.Name] = e.Value
	}
	// JobEnv (template env) wins over the shared buildEnv default, and the shim
	// reads this effective value when it builds the --build-arg.
	if env["GOPROXY"] != "http://kkrepo/go" || env["PIP_INDEX_URL"] != "https://kkrepo/pypi" {
		t.Fatalf("build env not exported: %v", env)
	}
	if env["GRC_BUILD_ENV_KEYS"] != "GOPROXY,NPM_CONFIG_REGISTRY,PIP_INDEX_URL" {
		t.Fatalf("GRC_BUILD_ENV_KEYS = %q", env["GRC_BUILD_ENV_KEYS"])
	}
	raw, err := base64.StdEncoding.DecodeString(env["GRC_REGISTRY_MAPS_B64"])
	if err != nil {
		t.Fatalf("decode registry maps: %v", err)
	}
	wantMaps := "ghcr.io=https://docker.example\nindex.docker.io=https://docker.example\n"
	if string(raw) != wantMaps {
		t.Fatalf("registry maps = %q, want %q", raw, wantMaps)
	}
	if env["GRC_REGISTRY_MAP_SKIP_FALLBACK"] != "1" {
		t.Fatalf("skip fallback = %q", env["GRC_REGISTRY_MAP_SKIP_FALLBACK"])
	}
	if hasEnv(kaniko, "GRC_BUILD_ENV_KEYS", env["GRC_BUILD_ENV_KEYS"]) {
		t.Fatal("kaniko sidecar must not receive build env keys")
	}
}

func TestCAPassedThroughEnvAndBundle(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ci", []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	s := testSpec()
	s.KanikoImage = "kaniko:test"
	if err := m.Start(context.Background(), s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pod, _ := cs.CoreV1().Pods("ci").Get(context.Background(), s.Name, metav1.GetOptions{})
	for _, name := range []string{RunnerContainerName, "kaniko"} {
		var c *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == name {
				c = &pod.Spec.Containers[i]
			}
		}
		if c == nil {
			t.Fatalf("container %s missing", name)
		}
		if !hasEnv(c, "SSL_CERT_FILE", "/etc/grc-ca/ca-certificates.crt") {
			t.Fatalf("SSL_CERT_FILE missing on %s: %v", name, c.Env)
		}
		if !hasMount(c, caBundleVol) {
			t.Fatalf("CA bundle mount missing on %s: %v", name, c.VolumeMounts)
		}
	}
	var caInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "ca-bundle" {
			caInit = &pod.Spec.InitContainers[i]
		}
	}
	if caInit == nil {
		t.Fatal("ca-bundle init container missing")
	}
	if len(caInit.Env) == 0 || caInit.Env[0].Name != "GRC_CA_PEM" || caInit.Env[0].Value == "" {
		t.Fatalf("ca env = %+v", caInit.Env)
	}
	if !hasMount(caInit, caBundleVol) {
		t.Fatalf("ca-bundle mount missing: %+v", caInit.VolumeMounts)
	}
}

func TestDeleteRemovesPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ci", nil)
	s := testSpec()
	if err := m.Start(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), s.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods("ci").Get(context.Background(), s.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("pod still exists")
	}
}

func hasMount(c *corev1.Container, name string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

func hasEnv(c *corev1.Container, name, value string) bool {
	for _, e := range c.Env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}
