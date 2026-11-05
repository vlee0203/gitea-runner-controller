// Command gitea-runner-controller runs Gitea Actions jobs as one-shot Kubernetes
// Pods. The controller reconciles Gitea's queued jobs against the runner Pods it
// owns; each Pod runs one pre-registered, ephemeral runner and talks to Gitea
// directly.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/vlee0203/gitea-runner-controller/internal/config"
	"github.com/vlee0203/gitea-runner-controller/internal/controller"
	"github.com/vlee0203/gitea-runner-controller/internal/executor"
	"github.com/vlee0203/gitea-runner-controller/internal/giteaapi"
	"github.com/vlee0203/gitea-runner-controller/internal/giteaclient"
	"github.com/vlee0203/gitea-runner-controller/internal/webhook"
)

const leaseName = "gitea-runner-controller"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gitea-runner-controller controller")
	}
	switch args[0] {
	case "controller":
		return runController(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cs, err := kubeClient()
	if err != nil {
		return err
	}
	var caPEM []byte
	if cfg.ExecutorCAPath != "" {
		caPEM, err = config.ReadCA(cfg.ExecutorCAPath)
		if err != nil {
			return fmt.Errorf("EXECUTOR_CA_PATH %s: %w", cfg.ExecutorCAPath, err)
		}
		log.Printf("controller and executor pods will trust CA material from %s (%d bytes)", cfg.ExecutorCAPath, len(caPEM))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hc, err := httpClient(caPEM)
	if err != nil {
		return err
	}
	api := giteaapi.New(cfg.GiteaURL, cfg.GiteaAPIToken, hc)
	registrar := giteaclient.New(cfg.GiteaURL, hc)
	exec := executor.New(cs, cfg.TargetNamespace, caPEM)
	ctrl := controller.New(cfg, api, registrar, exec)

	trigger := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.Handle("/webhook", webhook.Handler(cfg.WebhookSecret, func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("webhook server: %v", err)
			stop()
		}
	}()
	log.Printf("controller: gitea=%s ns=%s templates=%d listen=%s reconcile=%s",
		cfg.GiteaURL, cfg.TargetNamespace, len(cfg.Templates), cfg.ListenAddr, cfg.ReconcileInterval)

	identity, err := os.Hostname()
	if err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: cfg.TargetNamespace},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				ctrl.Run(leaderCtx, trigger)
			},
			OnStoppedLeading: func() { log.Printf("controller: leadership lost") },
		},
	})

	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return nil
}

// httpClient builds the controller's Gitea client. The custom CA is appended to
// the system roots, never replacing them: the controller may need to reach an
// intranet Gitea (self-signed) and public endpoints from the same process.
func httpClient(caPEM []byte) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected default HTTP transport %T", http.DefaultTransport)
	}
	tr := base.Clone()
	if len(caPEM) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("EXECUTOR_CA_PATH contains no usable certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}
