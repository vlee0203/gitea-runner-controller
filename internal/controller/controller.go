// Package controller reconciles Gitea's waiting jobs with one-shot runner Pods.
// Gitea's job list is the source of truth for demand; the Pods in the target
// namespace are the source of truth for capacity.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vlee0203/gitea-runner-controller/internal/config"
	"github.com/vlee0203/gitea-runner-controller/internal/executor"
	"github.com/vlee0203/gitea-runner-controller/internal/giteaapi"
)

// API is the subset of the Gitea admin API the controller uses.
type API interface {
	ListJobs(ctx context.Context, status string) ([]giteaapi.Job, error)
	RegistrationToken(ctx context.Context) (string, error)
	DeleteRunner(ctx context.Context, id int64) error
}

// Registrar registers a runner identity. *giteaclient.Client implements it.
type Registrar interface {
	Register(ctx context.Context, name, regToken string, labels []string, ephemeral bool) (*runnerv1.Runner, error)
}

// Pods is the subset of executor.Manager the controller uses.
type Pods interface {
	List(ctx context.Context) ([]corev1.Pod, error)
	Start(ctx context.Context, spec executor.Spec) error
	Delete(ctx context.Context, name string) error
}

// Controller is the reconciler.
type Controller struct {
	cfg       *config.Config
	api       API
	registrar Registrar
	pods      Pods
	now       func() time.Time
}

// New wires a controller.
func New(cfg *config.Config, api API, registrar Registrar, pods Pods) *Controller {
	return &Controller{cfg: cfg, api: api, registrar: registrar, pods: pods, now: time.Now}
}

// Run reconciles immediately and then on every trigger or interval tick.
func (c *Controller) Run(ctx context.Context, trigger <-chan struct{}) {
	ticker := time.NewTicker(c.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		if err := c.reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller: reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	// Gitea's job API uses GitHub-compatible statuses: queued / in_progress /
	// completed. The model's internal "waiting" name is not the API filter.
	queued, err := c.api.ListJobs(ctx, "queued")
	if err != nil {
		return fmt.Errorf("list queued jobs: %w", err)
	}
	running, err := c.api.ListJobs(ctx, "in_progress")
	if err != nil {
		return fmt.Errorf("list in-progress jobs: %w", err)
	}
	pods, err := c.pods.List(ctx)
	if err != nil {
		return fmt.Errorf("list runner pods: %w", err)
	}

	activeRunnerIDs := make(map[int64]bool, len(running))
	for _, j := range running {
		if j.RunnerID != 0 {
			activeRunnerIDs[j.RunnerID] = true
		}
	}

	active := map[string]int{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || runnerFinished(*p) {
			// A build Pod's kaniko sidecar never exits, so the Pod phase stays
			// Running after the runner container is done. The runner container's
			// termination is the real completion signal.
			c.cleanupPod(ctx, p, "terminal")
			continue
		}
		if reason := startFailure(*p); reason != "" {
			log.Printf("controller: pod %s cannot start: %s", p.Name, reason)
			c.cleanupPod(ctx, p, "start-failure")
			continue
		}
		template := p.Labels[executor.TemplateLabel]
		runnerID, _ := strconv.ParseInt(p.Labels[executor.RunnerIDLabel], 10, 64)
		if template == "" || runnerID == 0 {
			// A Pod from the old facade architecture. It cannot report anywhere
			// useful now; remove it rather than let it hang.
			c.cleanupPod(ctx, p, "legacy")
			continue
		}
		if !activeRunnerIDs[runnerID] && c.now().Sub(p.CreationTimestamp.Time) > c.cfg.IdleRunnerTimeout {
			c.cleanupPod(ctx, p, "idle")
			continue
		}
		active[template]++
	}

	desired := map[string]int{}
	for _, job := range queued {
		t := c.cfg.TemplateFor(job.Labels)
		if t == nil {
			log.Printf("controller: job %d labels=%v match no template", job.ID, job.Labels)
			continue
		}
		desired[t.Name]++
	}

	created := 0
	for i := range c.cfg.Templates {
		t := &c.cfg.Templates[i]
		n := desired[t.Name]
		if n > t.MaxConcurrent {
			n = t.MaxConcurrent
		}
		for active[t.Name] < n {
			if err := c.startRunner(ctx, t); err != nil {
				log.Printf("controller: template %s: %v", t.Name, err)
				break
			}
			active[t.Name]++
			created++
		}
	}
	if len(queued) > 0 || len(pods) > 0 || created > 0 {
		log.Printf("controller: queued=%d running=%d pods=%d created=%d", len(queued), len(running), len(pods), created)
	}
	return nil
}

func (c *Controller) cleanupPod(ctx context.Context, p *corev1.Pod, why string) {
	runnerID, _ := strconv.ParseInt(p.Labels[executor.RunnerIDLabel], 10, 64)
	if err := c.pods.Delete(ctx, p.Name); err != nil {
		log.Printf("controller: delete %s pod %s: %v", why, p.Name, err)
	}
	if runnerID != 0 {
		if err := c.api.DeleteRunner(ctx, runnerID); err != nil {
			log.Printf("controller: delete runner %d for %s: %v", runnerID, p.Name, err)
		}
	}
}

func runnerFinished(p corev1.Pod) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == executor.RunnerContainerName && cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

// startFailure returns a terminal container-start reason. Those Pods will never
// run a job, so keeping them until the idle timeout only wastes a slot.
func startFailure(p corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

func (c *Controller) startRunner(ctx context.Context, t *config.Template) error {
	name := podName(t.Name)
	regToken, err := c.api.RegistrationToken(ctx)
	if err != nil {
		return fmt.Errorf("get registration token: %w", err)
	}
	reg, err := c.registrar.Register(ctx, name, regToken, t.Labels, true)
	if err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	spec := executor.Spec{
		Name:      name,
		Template:  t.Name,
		Image:     t.Image,
		Labels:    t.Labels,
		Resources: resources(t.Resources),
		Runner: executor.Runner{
			ID: reg.Id, UUID: reg.Uuid, Token: reg.Token,
			Name: reg.Name, Address: c.cfg.GiteaURL, Labels: t.Labels,
		},
		RunnerConfig:                c.cfg.CacheConfigYAML(),
		ProxyEnv:                    c.cfg.ProxyEnv(),
		JobEnv:                      c.cfg.TemplateEnv(*t),
		BuildEnv:                    c.cfg.TemplateBuildEnv(*t),
		RegistryMaps:                c.cfg.TemplateRegistryMaps(*t),
		SkipDefaultRegistryFallback: c.cfg.SkipDefaultRegistryFallback,
		ImagePullSecret:             c.cfg.ImagePullSecret,
		ActiveDeadlineSeconds:       timeoutSeconds(t.Timeout),
		// Every runner carries the kaniko sidecar: standard workflows that build
		// images must keep working without a platform-specific runs-on label.
		KanikoImage: c.cfg.KanikoImage,
	}
	if err := c.pods.Start(ctx, spec); err != nil {
		if derr := c.api.DeleteRunner(ctx, reg.Id); derr != nil {
			log.Printf("controller: rollback runner %d after pod failure: %v", reg.Id, derr)
		}
		return fmt.Errorf("create pod: %w", err)
	}
	log.Printf("controller: template=%s runner=%d pod=%s", t.Name, reg.Id, name)
	return nil
}

func podName(template string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	slug := strings.ToLower(template)
	slug = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, slug)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "runner"
	}
	suffix := hex.EncodeToString(b[:])
	maxSlug := 63 - len("grc-") - 1 - len(suffix)
	if len(slug) > maxSlug {
		slug = strings.TrimRight(slug[:maxSlug], "-")
	}
	return "grc-" + slug + "-" + suffix
}

func resources(r config.ResourceLimits) corev1.ResourceRequirements {
	limits := corev1.ResourceList{}
	if r.CPU != "" {
		if q, err := resource.ParseQuantity(r.CPU); err == nil {
			limits[corev1.ResourceCPU] = q
		}
	}
	if r.Memory != "" {
		if q, err := resource.ParseQuantity(r.Memory); err == nil {
			limits[corev1.ResourceMemory] = q
		}
	}
	if r.EphemeralStorage != "" {
		if q, err := resource.ParseQuantity(r.EphemeralStorage); err == nil {
			limits[corev1.ResourceEphemeralStorage] = q
		}
	}
	if len(limits) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: limits, Requests: limits}
}

func timeoutSeconds(s string) int64 {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}
