package httpapi

import (
	"net/http"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/auth"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/view"
)

func merchantOf(r *http.Request) model.Merchant {
	p, _ := auth.MerchantFrom(r.Context())
	return p.Merchant
}

func (s *Server) handleTokenize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Card card.Input `json:"card"`
	}
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	vc, err := s.payments.Tokenize(r.Context(), merchantOf(r), in.Card)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view.VaultCard(vc))
}

func (s *Server) handleCreatePayment(w http.ResponseWriter, r *http.Request) {
	var in payment.CreateInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Create(r.Context(), merchantOf(r), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view.PaymentIntent(p))
}

func (s *Server) handleListPayments(w http.ResponseWriter, r *http.Request) {
	s.listPayments(w, r, merchantOf(r).ID)
}

func (s *Server) listPayments(w http.ResponseWriter, r *http.Request, merchantID string) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var v apperr.Validator
	f := store.PaymentFilter{
		MerchantID: merchantID, Status: model.PaymentStatus(r.URL.Query().Get("status")), Page: page,
		CreatedAfter: timeParam(r, "created_after", &v), CreatedBefore: timeParam(r, "created_before", &v),
	}
	if err := v.Err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	ps, more, err := s.payments.List(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(view.PaymentList(ps), more, func(p view.Payment) string { return p.ID }))
}

func (s *Server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.payments.Get(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.PaymentIntent(p))
}

func (s *Server) handlePaymentHistory(w http.ResponseWriter, r *http.Request) {
	h, err := s.payments.History(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(h, false, func(x model.PaymentHistory) string { return x.ID }))
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var in payment.AuthorizeInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Authorize(r.Context(), merchantOf(r), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

func (s *Server) handleComplete3DS(w http.ResponseWriter, r *http.Request) {
	var in payment.ChallengeInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Complete3DS(r.Context(), merchantOf(r), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	var in payment.CaptureInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Capture(r.Context(), merchantOf(r), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

func (s *Server) handleVoid(w http.ResponseWriter, r *http.Request) {
	var in payment.VoidInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Void(r.Context(), merchantOf(r), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var in payment.VoidInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Cancel(r.Context(), merchantOf(r), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

// paymentResult writes a payment intent. Declines and fraud blocks are
// business outcomes, not errors: they return 200 with the status and
// decline_code on the object so clients always receive the full resource.
func (s *Server) paymentResult(w http.ResponseWriter, r *http.Request, p *model.PaymentIntent, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.PaymentIntent(p))
}

func (s *Server) handleRefund(w http.ResponseWriter, r *http.Request) {
	var in payment.RefundInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	ref, p, err := s.payments.Refund(r.Context(), merchantOf(r), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		view.Refund
		PaymentIntent view.Payment `json:"payment_intent"`
	}{view.RefundOf(ref), view.PaymentIntent(p)})
}

func (s *Server) handleListRefunds(w http.ResponseWriter, r *http.Request) {
	refs, err := s.payments.Refunds(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]view.Refund, len(refs))
	for i, x := range refs {
		out[i] = view.RefundOf(x)
	}
	writeJSON(w, http.StatusOK, view.NewList(out, false, func(x view.Refund) string { return x.ID }))
}

func (s *Server) handleGetRefund(w http.ResponseWriter, r *http.Request) {
	ref, err := s.payments.GetRefund(r.Context(), merchantOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.RefundOf(ref))
}
