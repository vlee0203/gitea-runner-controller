// Package giteaapi is the controller-side REST client for the Gitea Actions
// admin API: it lists queued/running jobs, creates runner registration tokens,
// and deletes runner records the controller itself registered.
package giteaapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const pageSize = 50

// Job is the subset of an ActionWorkflowJob the controller needs.
type Job struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	Labels     []string `json:"labels"`
	RunnerID   int64    `json:"runner_id"`
	RunnerName string   `json:"runner_name"`
}

type jobsResponse struct {
	Jobs       []Job `json:"jobs"`
	TotalCount int64 `json:"total_count"`
}

type registrationTokenResponse struct {
	Token string `json:"token"`
}

// Client is a Gitea admin API client.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New creates a client for the Gitea base URL (for example
// https://gitea.example.com) and an admin API token.
func New(base, token string, hc *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound && method == http.MethodDelete {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListJobs returns every job with the given status. Gitea's admin endpoint is
// paginated; the controller needs the full waiting set to enforce concurrency.
func (c *Client) ListJobs(ctx context.Context, status string) ([]Job, error) {
	var out []Job
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("status", status)
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(pageSize))
		var r jobsResponse
		if err := c.do(ctx, http.MethodGet, "/admin/actions/jobs?"+q.Encode(), &r); err != nil {
			return nil, err
		}
		out = append(out, r.Jobs...)
		if len(r.Jobs) < pageSize || int64(len(out)) >= r.TotalCount {
			return out, nil
		}
	}
}

// RegistrationToken returns a token that can register a new runner.
func (c *Client) RegistrationToken(ctx context.Context) (string, error) {
	var r registrationTokenResponse
	if err := c.do(ctx, http.MethodPost, "/admin/actions/runners/registration-token", &r); err != nil {
		return "", err
	}
	if r.Token == "" {
		return "", fmt.Errorf("gitea returned an empty runner registration token")
	}
	return r.Token, nil
}

// DeleteRunner removes a runner record by ID. A missing record is not an error:
// Gitea may already have cleaned up an ephemeral runner.
func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/admin/actions/runners/"+strconv.FormatInt(id, 10), nil)
}
