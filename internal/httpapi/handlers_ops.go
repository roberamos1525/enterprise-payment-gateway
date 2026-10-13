package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

// handleReady reports whether the database is reachable.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.Registry.Write(w)
}

// handleTestCards lists the synthetic test PANs and their scenarios. They
// are public documentation numbers, never real cards.
func (s *Server) handleTestCards(w http.ResponseWriter, r *http.Request) {
	cards := card.TestCards()
	writeJSON(w, http.StatusOK, map[string]any{
		"object":      "list",
		"notice":      "Synthetic test card numbers for this simulator only. Never use real card data.",
		"data":        cards,
		"event_types": eventTypes(),
		"has_more":    false,
		"total":       len(cards),
		"note":        "Any other Luhn-valid PAN of a supported brand is approved.",
	})
}
