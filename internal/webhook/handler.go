// Package webhook receives Gitea system webhooks. It deliberately does only
// two things: verify the HMAC, and wake the reconciler. The Gitea job list is
// the source of truth, so a dropped or duplicate webhook cannot lose a job.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"
)

const (
	signatureHeader = "X-Gitea-Signature"
	eventHeader     = "X-Gitea-Event"
	deliveryHeader  = "X-Gitea-Delivery"
)

// Handler returns an http.Handler that verifies Gitea's HMAC and calls trigger
// for workflow_job events. trigger must be non-blocking.
func Handler(secret string, trigger func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if secret == "" {
			http.Error(w, "webhook secret is not configured", http.StatusServiceUnavailable)
			return
		}
		if !validSignature(secret, body, r.Header.Get(signatureHeader)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		event := r.Header.Get(eventHeader)
		if event != "workflow_job" {
			// Acknowledge other events: the webhook may be shared with other consumers.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		log.Printf("webhook: workflow_job delivery=%s", r.Header.Get(deliveryHeader))
		trigger()
		w.WriteHeader(http.StatusAccepted)
	})
}

func validSignature(secret string, body []byte, got string) bool {
	got = strings.TrimSpace(got)
	if got == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(got)), []byte(want))
}
