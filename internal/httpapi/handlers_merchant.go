package httpapi

import (
	"net/http"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/auth"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/view"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

func (s *Server) handleSelf(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, merchantOf(r))
}

func (s *Server) handleSelfBalance(w http.ResponseWriter, r *http.Request) {
	s.writeBalance(w, r, merchantOf(r).ID)
}

func (s *Server) writeBalance(w http.ResponseWriter, r *http.Request, merchantID string) {
	b, err := ledger.Balances(r.Context(), s.db.Q(), merchantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "balance", "merchant_id": merchantID, "accounts": nonNil(b)})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) handleSelfKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.merchants.ListKeys(r.Context(), merchantOf(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(keys, false, func(k model.APIKey) string { return k.ID }))
}

type rotateInput struct {
	GracePeriodSeconds *int64 `json:"grace_period_seconds,omitempty"`
}

func (in rotateInput) grace() *time.Duration {
	if in.GracePeriodSeconds == nil {
		return nil
	}
	d := time.Duration(*in.GracePeriodSeconds) * time.Second
	return &d
}

// handleSelfRotateKey rotates the key used to make this request.
func (s *Server) handleSelfRotateKey(w http.ResponseWriter, r *http.Request) {
	var in rotateInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, _ := auth.MerchantFrom(r.Context())
	k, err := s.merchants.RotateKey(r.Context(), p.Merchant.ID, p.Key.ID, in.grace())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

// --- webhook endpoints ---------------------------------------------------------

func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	var in merchant.EndpointInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	ep, err := s.merchants.CreateEndpoint(r.Context(), merchantOf(r).ID, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ep)
}

func (s *Server) handleListEndpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := s.merchants.ListEndpoints(r.Context(), merchantOf(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(eps, false, func(e model.WebhookEndpoint) string { return e.ID }))
}

func (s *Server) handleGetEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := s.merchants.GetEndpoint(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) handleUpdateEndpoint(w http.ResponseWriter, r *http.Request) {
	var in merchant.EndpointInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	ep, err := s.merchants.UpdateEndpoint(r.Context(), merchantOf(r).ID, r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	ep, err := s.merchants.RotateSecret(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) handlePingEndpoint(w http.ResponseWriter, r *http.Request) {
	d, err := s.outbox.Ping(r.Context(), s.db, merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (s *Server) handleEndpointDeliveries(w http.ResponseWriter, r *http.Request) {
	m := merchantOf(r)
	if _, err := s.merchants.GetEndpoint(r.Context(), m.ID, r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := model.DeliveryStatus(r.URL.Query().Get("status"))
	if status != "" {
		var v apperr.Validator
		v.OneOf("status", string(status), string(model.DeliveryPending), string(model.DeliverySucceeded), string(model.DeliveryFailed))
		if err := v.Err(); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	ds, more, err := s.db.Q().ListDeliveries(r.Context(), store.DeliveryFilter{
		MerchantID: m.ID, EndpointID: r.PathValue("id"), Status: status, Page: page,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(ds, more, func(d model.Delivery) string { return d.ID }))
}

func (s *Server) handleGetDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.db.Q().GetDelivery(r.Context(), r.PathValue("id"))
	if err != nil || d.MerchantID != merchantOf(r).ID {
		if err == nil || err == store.ErrNotFound {
			err = apperr.NotFound("webhook delivery", r.PathValue("id"))
		}
		s.writeError(w, r, err)
		return
	}
	attempts, err := s.db.Q().ListAttempts(r.Context(), d.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		model.Delivery
		AttemptLog []model.DeliveryAttempt `json:"attempt_log"`
	}{d, nonNil(attempts)})
}

// --- events ------------------------------------------------------------------------

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	evs, more, err := s.db.Q().ListEvents(r.Context(), store.EventFilter{
		MerchantID: merchantOf(r).ID, Type: q.Get("type"), ResourceID: q.Get("resource_id"), Page: page,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(evs, more, func(e model.Event) string { return e.ID }))
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.db.Q().GetEvent(r.Context(), r.PathValue("id"))
	if err != nil || ev.MerchantID != merchantOf(r).ID {
		if err == nil || err == store.ErrNotFound {
			err = apperr.NotFound("event", r.PathValue("id"))
		}
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleReplayEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EndpointID string `json:"endpoint_id"`
	}
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	ds, err := s.outbox.Replay(r.Context(), s.db, merchantOf(r).ID, r.PathValue("id"), in.EndpointID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view.NewList(ds, false, func(d model.Delivery) string { return d.ID }))
}

// --- settlements -----------------------------------------------------------------

func (s *Server) handleListSettlements(w http.ResponseWriter, r *http.Request) {
	s.listSettlements(w, r, merchantOf(r).ID)
}

func (s *Server) listSettlements(w http.ResponseWriter, r *http.Request, merchantID string) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	date := r.URL.Query().Get("date")
	if date != "" {
		if _, err := dateParam("date", date); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	bs, more, err := s.settlements.List(r.Context(), store.BatchFilter{MerchantID: merchantID, Date: date, Page: page})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(bs, more, func(b model.SettlementBatch) string { return b.ID }))
}

func (s *Server) handleGetSettlement(w http.ResponseWriter, r *http.Request) {
	b, err := s.settlements.Batch(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleSettlementReport(w http.ResponseWriter, r *http.Request) {
	s.settlementReport(w, r, merchantOf(r).ID)
}

func (s *Server) settlementReport(w http.ResponseWriter, r *http.Request, merchantID string) {
	rep, err := s.settlements.Report(r.Context(), merchantID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+rep.Batch.ID+`.csv"`)
		if err := rep.WriteCSV(w); err != nil {
			s.log.Error("write settlement csv", "error", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleListPayouts(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ps, more, err := s.settlements.Payouts(r.Context(), merchantOf(r).ID, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(ps, more, func(p model.Payout) string { return p.ID }))
}

// eventTypes is used by the ops handler to document subscriptions.
func eventTypes() []string { return webhook.EventTypes() }
