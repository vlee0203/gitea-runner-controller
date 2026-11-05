// Package giteaclient registers ephemeral runners with Gitea's RunnerService.
// The returned identity is written into the Pod's .runner file; the runner
// process then talks to Gitea directly.
package giteaclient

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
)

// Client registers runners.
type Client struct {
	svc runnerv1connect.RunnerServiceClient
}

// New builds a client for the given Gitea base URL.
func New(giteaURL string, hc connect.HTTPClient) *Client {
	return &Client{svc: runnerv1connect.NewRunnerServiceClient(hc, giteaURL+"/api/actions")}
}

// Register registers one ephemeral runner and returns its identity.
func (c *Client) Register(ctx context.Context, name, regToken string, labels []string, ephemeral bool) (*runnerv1.Runner, error) {
	resp, err := c.svc.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{
		Name: name, Token: regToken, Labels: labels, Ephemeral: ephemeral,
		Capabilities: []string{"cancelling"},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Runner, nil
}

var _ connect.HTTPClient = (*http.Client)(nil)
