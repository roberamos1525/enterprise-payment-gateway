// Command webhook-receiver is a small local endpoint for trying out gateway
// webhooks: it verifies each request's signature and appends verified events
// to an NDJSON file (and stdout).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "listen address")
	out := flag.String("out", "webhooks.ndjson", "file to append verified events to")
	tolerance := flag.Duration("tolerance", 5*time.Minute, "maximum signature age")
	flag.Parse()
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "webhook-receiver: WEBHOOK_SECRET must be set to the endpoint's signing secret")
		os.Exit(2)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhook-receiver:", err)
		os.Exit(1)
	}
	defer f.Close()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	h := &handler{secret: secret, tolerance: *tolerance, sink: f, log: log}

	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("webhook receiver listening", "addr", *addr, "out", *out)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "webhook-receiver:", err)
		os.Exit(1)
	}
}

type handler struct {
	mu        sync.Mutex
	secret    string
	tolerance time.Duration
	sink      io.Writer
	log       *slog.Logger
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := webhook.Verify(r.Header.Get(webhook.HeaderSignature), body, h.secret, h.tolerance, time.Now()); err != nil {
		h.log.Warn("rejected webhook", "error", err, "delivery_id", r.Header.Get(webhook.HeaderDeliveryID))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var env webhook.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	_, err = h.sink.Write(append(body, '\n'))
	h.mu.Unlock()
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.log.Info("verified webhook", "event_id", env.ID, "type", env.Type,
		"delivery_id", r.Header.Get(webhook.HeaderDeliveryID), "attempt", r.Header.Get(webhook.HeaderAttempt))
	w.WriteHeader(http.StatusNoContent)
}
