package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func signature(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestWorkflowJobTriggersReconcile(t *testing.T) {
	calls := 0
	h := Handler("secret", func() { calls++ })
	body := `{"action":"queued","workflow_job":{"id":1}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(signatureHeader, signature("secret", body))
	req.Header.Set(eventHeader, "workflow_job")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("code=%d calls=%d", rec.Code, calls)
	}
}

func TestRejectsBadSignatureAndUnknownEvent(t *testing.T) {
	calls := 0
	h := Handler("secret", func() { calls++ })
	body := "{}"
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(signatureHeader, "deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("bad signature: code=%d calls=%d", rec.Code, calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(signatureHeader, signature("secret", body))
	req.Header.Set(eventHeader, "push")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || calls != 0 {
		t.Fatalf("other event: code=%d calls=%d", rec.Code, calls)
	}
}

func TestUnsignedWebhookIsDisabledWithoutSecret(t *testing.T) {
	h := Handler("", func() {})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}
