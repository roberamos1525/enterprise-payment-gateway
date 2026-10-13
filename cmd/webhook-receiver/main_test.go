package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

func TestReceiverVerifiesAndStores(t *testing.T) {
	var sink bytes.Buffer
	h := &handler{secret: "whsec_r", tolerance: time.Minute, sink: &sink, log: logging.Discard()}
	body := []byte(`{"id":"evt_1","type":"payment_intent.captured","data":{}}`)

	send := func(sig string, method string, b []byte) int {
		req := httptest.NewRequest(method, "/", bytes.NewReader(b))
		req.Header.Set(webhook.HeaderSignature, sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	good := webhook.SignatureHeader(time.Now().Unix(), body, "whsec_r")
	if code := send(good, http.MethodPost, body); code != http.StatusNoContent {
		t.Fatalf("valid webhook: %d", code)
	}
	if !strings.Contains(sink.String(), `"evt_1"`) {
		t.Fatalf("event not stored: %q", sink.String())
	}
	bad := webhook.SignatureHeader(time.Now().Unix(), body, "whsec_other")
	if code := send(bad, http.MethodPost, body); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	old := webhook.SignatureHeader(time.Now().Add(-time.Hour).Unix(), body, "whsec_r")
	if code := send(old, http.MethodPost, body); code != http.StatusUnauthorized {
		t.Fatalf("replayed old webhook: %d", code)
	}
	notJSON := []byte("not json")
	if code := send(webhook.SignatureHeader(time.Now().Unix(), notJSON, "whsec_r"), http.MethodPost, notJSON); code != http.StatusBadRequest {
		t.Fatalf("non-JSON body: %d", code)
	}
	if code := send(good, http.MethodGet, nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", code)
	}
	if strings.Count(sink.String(), "\n") != 1 {
		t.Fatalf("only the verified event may be stored: %q", sink.String())
	}
}
