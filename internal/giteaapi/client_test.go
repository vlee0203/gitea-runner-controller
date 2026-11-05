package giteaapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestListJobsPaginatesAndAuthenticates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token api" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/admin/actions/jobs" || r.URL.Query().Get("status") != "queued" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages = page
		n := pageSize
		if page == 2 {
			n = 1
		}
		jobs := make([]Job, n)
		for i := range jobs {
			jobs[i] = Job{ID: int64((page-1)*pageSize + i + 1), Status: "queued", Labels: []string{"ubuntu-latest"}}
		}
		_ = json.NewEncoder(w).Encode(jobsResponse{Jobs: jobs, TotalCount: 51})
	}))
	defer srv.Close()

	got, err := New(srv.URL, "api", srv.Client()).ListJobs(context.Background(), "queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 51 || pages != 2 || got[50].ID != 51 {
		t.Fatalf("len=%d pages=%d last=%d", len(got), pages, got[len(got)-1].ID)
	}
}

func TestRegistrationTokenAndDeleteRunner(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/actions/runners/registration-token":
			_, _ = w.Write([]byte(`{"token":"reg"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/admin/actions/runners/"):
			deleted = strings.TrimPrefix(r.URL.Path, "/api/v1/admin/actions/runners/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "api", srv.Client())
	tok, err := c.RegistrationToken(context.Background())
	if err != nil || tok != "reg" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	if err := c.DeleteRunner(context.Background(), 382); err != nil {
		t.Fatal(err)
	}
	if deleted != "382" {
		t.Fatalf("deleted = %q", deleted)
	}
}

func TestHTTPErrorIncludesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "api", srv.Client()).ListJobs(context.Background(), "queued")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}
