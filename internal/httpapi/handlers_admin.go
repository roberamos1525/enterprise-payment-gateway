package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/settlement"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/view"
)

// --- merchants ---------------------------------------------------------------

func (s *Server) handleOnboard(w http.ResponseWriter, r *http.Request) {
	var in merchant.OnboardInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.merchants.Onboard(r.Context(), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleListMerchants(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := model.MerchantStatus(r.URL.Query().Get("status"))
	if status != "" {
		var v apperr.Validator
		v.OneOf("status", string(status), string(model.MerchantPending), string(model.MerchantActive),
			string(model.MerchantSuspended), string(model.MerchantClosed))
		if err := v.Err(); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	ms, more, err := s.merchants.List(r.Context(), store.MerchantFilter{Status: status, Page: page})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(ms, more, func(m model.Merchant) string { return m.ID }))
}

func (s *Server) handleGetMerchant(w http.ResponseWriter, r *http.Request) {
	m, err := s.merchants.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleUpdateMerchant(w http.ResponseWriter, r *http.Request) {
	var in merchant.UpdateProfileInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.merchants.UpdateProfile(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMerchantStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.merchants.SetStatus(r.Context(), r.PathValue("id"), model.MerchantStatus(in.Status), in.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleListFees(w http.ResponseWriter, r *http.Request) {
	fs, err := s.merchants.FeeSchedules(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(fs, false, func(f model.FeeSchedule) string { return f.ID }))
}

func (s *Server) handleSetFees(w http.ResponseWriter, r *http.Request) {
	var in merchant.FeeInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.merchants.SetFeeSchedule(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.merchants.ListKeys(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(keys, false, func(k model.APIKey) string { return k.ID }))
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var in merchant.CreateKeyInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	k, err := s.merchants.CreateKey(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	var in rotateInput
	if err := decode(r, &in, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	k, err := s.merchants.RotateKey(r.Context(), r.PathValue("id"), r.PathValue("key_id"), in.grace())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.merchants.RevokeKey(r.Context(), r.PathValue("id"), r.PathValue("key_id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMerchantBalance(w http.ResponseWriter, r *http.Request) {
	if _, err := s.merchants.Get(r.Context(), r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeBalance(w, r, r.PathValue("id"))
}

// --- payments (operator view) -------------------------------------------------------

func (s *Server) handleAdminListPayments(w http.ResponseWriter, r *http.Request) {
	s.listPayments(w, r, r.URL.Query().Get("merchant_id"))
}

func (s *Server) handleAdminGetPayment(w http.ResponseWriter, r *http.Request) {
	p, err := s.payments.Get(r.Context(), "", r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.PaymentIntent(p))
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	var in payment.ReviewInput
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.payments.Review(r.Context(), r.PathValue("id"), in)
	s.paymentResult(w, r, p, err)
}

// --- settlement & reconciliation ------------------------------------------------------

func (s *Server) handleRunSettlement(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Date         string `json:"date"`
		MerchantID   string `json:"merchant_id"`
		AllowOpenDay bool   `json:"allow_open_day"`
	}
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	day, err := dateParam("date", in.Date)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.settlements.Run(r.Context(), settlement.RunInput{Day: day, MerchantID: in.MerchantID, AllowOpenDay: in.AllowOpenDay})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAdminListSettlements(w http.ResponseWriter, r *http.Request) {
	s.listSettlements(w, r, r.URL.Query().Get("merchant_id"))
}

func (s *Server) handleSettlementSummary(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if _, err := dateParam("date", date); err != nil {
		s.writeError(w, r, err)
		return
	}
	sum, err := s.settlements.Summary(r.Context(), r.URL.Query().Get("merchant_id"), date)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleAdminSettlementReport(w http.ResponseWriter, r *http.Request) {
	s.settlementReport(w, r, "")
}

func (s *Server) handlePayDue(w http.ResponseWriter, r *http.Request) {
	n, err := s.settlements.PayDuePayouts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"payouts_paid": n})
}

func (s *Server) handleAcquirerFile(w http.ResponseWriter, r *http.Request) {
	day, err := dateParam("date", r.URL.Query().Get("date"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, err := s.acquirer.SettlementFile(r.Context(), day)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := acquirer.WriteCSV(&buf, rows); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="acquirer-settlement-`+day.Format(clock.DateLayout)+`.csv"`)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Date string `json:"date"`
	}
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	day, err := dateParam("date", in.Date)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.recon.RunFromSimulator(r.Context(), day)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// handleReconcileUpload accepts a settlement file as a text/csv body.
func (s *Server) handleReconcileUpload(w http.ResponseWriter, r *http.Request) {
	day, err := dateParam("date", r.URL.Query().Get("date"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		s.writeError(w, r, apperr.New(apperr.CodeUnsupportedMediaType, "Content-Type must be text/csv"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, r, bodyReadError(err))
		return
	}
	run, err := s.recon.RunFromCSV(r.Context(), day, bytes.NewReader(body))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) handleListReconciliations(w http.ResponseWriter, r *http.Request) {
	page, err := pageFrom(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	runs, more, err := s.recon.List(r.Context(), page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(runs, more, func(x model.ReconciliationRun) string { return x.ID }))
}

func (s *Server) handleGetReconciliation(w http.ResponseWriter, r *http.Request) {
	run, err := s.recon.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// --- audit, fraud, vault, ledger, jobs ---------------------------------------------------

func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var v apperr.Validator
	f := store.AuditFilter{
		MerchantID: q.Get("merchant_id"), ResourceType: q.Get("resource_type"), ResourceID: q.Get("resource_id"),
		Action: q.Get("action"), ActorID: q.Get("actor_id"), Since: timeParam(r, "since", &v), Limit: 100,
	}
	if after := q.Get("after_seq"); after != "" {
		n, err := strconv.ParseInt(after, 10, 64)
		if v.Check(err == nil && n >= 0, "after_seq", "must be a non-negative integer") {
			f.AfterSeq = n
		}
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if v.Check(err == nil && n >= 1 && n <= 1000, "limit", "must be between 1 and 1000") {
			f.Limit = n
		}
	}
	if err := v.Err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	entries, err := s.db.Q().ListAudit(r.Context(), f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	more := len(entries) == f.Limit
	l := view.NewList(entries, more, func(e model.AuditEntry) string { return strconv.FormatInt(e.Seq, 10) })
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	res, err := audit.Verify(r.Context(), s.db.Q())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListBlocklist(w http.ResponseWriter, r *http.Request) {
	bins, err := s.blocklist.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view.NewList(bins, false, func(b model.BlockedBIN) string { return b.BIN }))
}

func (s *Server) handleAddBlocklist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BIN    string `json:"bin"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &in, false); err != nil {
		s.writeError(w, r, err)
		return
	}
	b, err := s.blocklist.Add(r.Context(), strings.TrimSpace(in.BIN), strings.TrimSpace(in.Reason))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleRemoveBlocklist(w http.ResponseWriter, r *http.Request) {
	if err := s.blocklist.Remove(r.Context(), r.PathValue("bin")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFraudRules(w http.ResponseWriter, r *http.Request) {
	cfg := s.fraud.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"rules": s.fraud.RuleNames(), "review_score": cfg.ReviewScore, "block_score": cfg.BlockScore,
		"amount_review_minor": cfg.AmountReviewMinor, "amount_block_minor": cfg.AmountBlockMinor,
		"card_velocity":       map[string]any{"max": cfg.CardVelocityMax, "window": cfg.CardVelocityWindow.String()},
		"ip_velocity":         map[string]any{"max": cfg.IPVelocityMax, "window": cfg.IPVelocityWindow.String()},
		"merchant_velocity":   map[string]any{"max": cfg.MerchantMax, "window": cfg.MerchantWindow.String()},
		"high_risk_countries": nonNil(cfg.HighRiskCountries),
	})
}

func (s *Server) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	counts, err := s.db.Q().CountCardsByKey(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"primary_key": s.vault.Keyring().PrimaryID(), "keys": s.vault.Keyring().KeyIDs(), "cards_by_key": counts,
	})
}

func (s *Server) handleVaultRewrap(w http.ResponseWriter, r *http.Request) {
	res, err := s.vault.Rewrap(r.Context(), s.db, 200)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTrialBalance(w http.ResponseWriter, r *http.Request) {
	res, err := ledger.TrialBalance(r.Context(), s.db.Q())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	job, ok := s.jobs[name]
	if !ok {
		s.writeError(w, r, apperr.NotFound("job", name))
		return
	}
	res, err := job(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": name, "result": res})
}
