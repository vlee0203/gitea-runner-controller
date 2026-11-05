package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vlee0203/gitea-runner-controller/internal/config"
	"github.com/vlee0203/gitea-runner-controller/internal/executor"
	"github.com/vlee0203/gitea-runner-controller/internal/giteaapi"
)

type fakeAPI struct {
	waiting, running []giteaapi.Job
	deleted          []int64
	regCalls         int
	statuses         []string
	err              error
}

func (f *fakeAPI) ListJobs(_ context.Context, status string) ([]giteaapi.Job, error) {
	f.statuses = append(f.statuses, status)
	if f.err != nil {
		return nil, f.err
	}
	if status == "queued" {
		return f.waiting, nil
	}
	return f.running, nil
}
func (f *fakeAPI) RegistrationToken(context.Context) (string, error) {
	f.regCalls++
	return "reg-token", nil
}
func (f *fakeAPI) DeleteRunner(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeRegistrar struct {
	id  int64
	err error
}

func (f *fakeRegistrar) Register(context.Context, string, string, []string, bool) (*runnerv1.Runner, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.id++
	return &runnerv1.Runner{Id: f.id, Uuid: "uuid-" + strconv.FormatInt(f.id, 10), Token: "tok", Name: "runner", Labels: []string{"self-hosted"}}, nil
}

type fakePods struct {
	pods    []corev1.Pod
	started []executor.Spec
	deleted []string
	err     error
}

func (f *fakePods) List(context.Context) ([]corev1.Pod, error) { return f.pods, nil }
func (f *fakePods) Start(_ context.Context, s executor.Spec) error {
	if f.err != nil {
		return f.err
	}
	f.started = append(f.started, s)
	return nil
}
func (f *fakePods) Delete(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func testConfig() *config.Config {
	return &config.Config{
		GiteaURL:          "https://gitea.example.com",
		TargetNamespace:   "ci",
		IdleRunnerTimeout: 10 * time.Minute,
		Templates: []config.Template{
			{Name: "standard", Labels: []string{"self-hosted", "ubuntu-latest"}, Image: "runner:1", MaxConcurrent: 2, Timeout: "1h"},
		},
		KanikoImage: "kaniko:1",
	}
}

func newTestController(api *fakeAPI, reg *fakeRegistrar, pods *fakePods) *Controller {
	c := New(testConfig(), api, reg, pods)
	c.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return c
}

func TestReconcileCreatesUpToTemplateMaximum(t *testing.T) {
	api := &fakeAPI{waiting: []giteaapi.Job{
		{ID: 1, Labels: []string{"self-hosted", "ubuntu-latest"}},
		{ID: 2, Labels: []string{"self-hosted", "ubuntu-latest"}},
		{ID: 3, Labels: []string{"self-hosted", "ubuntu-latest"}},
	}}
	reg := &fakeRegistrar{}
	pods := &fakePods{}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.started) != 2 {
		t.Fatalf("started %d runners, want 2", len(pods.started))
	}
	// Gitea's job API uses queued/in_progress; the internal "waiting"/"running"
	// names are not valid HTTP status filters (measured on Gitea 28.1.0).
	for _, want := range []string{"queued", "in_progress"} {
		found := false
		for _, got := range api.statuses {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("statuses = %v, missing %q", api.statuses, want)
		}
	}
	if pods.started[0].Template != "standard" || pods.started[0].Runner.UUID == "" {
		t.Fatalf("spec = %+v", pods.started[0])
	}
}

func TestReconcilePassesKanikoToEveryRunner(t *testing.T) {
	api := &fakeAPI{waiting: []giteaapi.Job{{ID: 1, Labels: []string{"self-hosted", "ubuntu-latest"}}}}
	reg := &fakeRegistrar{}
	pods := &fakePods{}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.started) != 1 || pods.started[0].Template != "standard" || pods.started[0].KanikoImage == "" {
		t.Fatalf("started = %+v", pods.started)
	}
}

func TestReconcileCleansTerminalPodsAndRunnerRecords(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	pods := &fakePods{pods: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "done", Labels: map[string]string{
			executor.TemplateLabel: "standard", executor.RunnerIDLabel: "7",
		}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}}}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "done" {
		t.Fatalf("deleted = %v", pods.deleted)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 7 {
		t.Fatalf("runner records deleted = %v", api.deleted)
	}
}

func TestReconcileDeletesIdleRunner(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	old := metav1.NewTime(time.Unix(1_700_000_000-3600, 0))
	pods := &fakePods{pods: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "idle", CreationTimestamp: old, Labels: map[string]string{
			executor.TemplateLabel: "standard", executor.RunnerIDLabel: "8",
		}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}}}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "idle" || len(api.deleted) != 1 || api.deleted[0] != 8 {
		t.Fatalf("deleted pods=%v runners=%v", pods.deleted, api.deleted)
	}
}

func TestReconcileKeepsAssignedRunnerAndDoesNotOverCreate(t *testing.T) {
	api := &fakeAPI{
		waiting: []giteaapi.Job{{ID: 1, Labels: []string{"self-hosted", "ubuntu-latest"}}},
		running: []giteaapi.Job{{ID: 1, RunnerID: 9}},
	}
	reg := &fakeRegistrar{}
	pods := &fakePods{pods: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "busy", CreationTimestamp: metav1.NewTime(time.Unix(1_700_000_000-3600, 0)), Labels: map[string]string{
			executor.TemplateLabel: "standard", executor.RunnerIDLabel: "9",
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}}}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.deleted) != 0 || len(pods.started) != 0 {
		t.Fatalf("deleted=%v started=%v", pods.deleted, pods.started)
	}
}

func TestReconcileCleansBuildPodWhenRunnerContainerExits(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	pods := &fakePods{pods: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "built", Labels: map[string]string{
			executor.TemplateLabel: "build", executor.RunnerIDLabel: "12",
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: executor.RunnerContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "kaniko", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}}}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "built" || len(api.deleted) != 1 || api.deleted[0] != 12 {
		t.Fatalf("deleted pods=%v runners=%v", pods.deleted, api.deleted)
	}
}

func TestReconcileCleansPodWithTerminalStartFailure(t *testing.T) {
	api := &fakeAPI{}
	reg := &fakeRegistrar{}
	pods := &fakePods{pods: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Labels: map[string]string{
			executor.TemplateLabel: "standard", executor.RunnerIDLabel: "11",
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}}}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "bad" || len(api.deleted) != 1 || api.deleted[0] != 11 {
		t.Fatalf("deleted pods=%v runners=%v", pods.deleted, api.deleted)
	}
}

func TestReconcileRollsBackRunnerWhenPodCreationFails(t *testing.T) {
	api := &fakeAPI{waiting: []giteaapi.Job{{ID: 1, Labels: []string{"self-hosted", "ubuntu-latest"}}}}
	reg := &fakeRegistrar{}
	pods := &fakePods{err: errors.New("quota")}
	if err := newTestController(api, reg, pods).reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 1 {
		t.Fatalf("rollback = %v", api.deleted)
	}
}
