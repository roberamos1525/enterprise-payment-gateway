package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

const paymentCols = `id, merchant_id, amount, currency, status, capture_method, amount_authorized, amount_captured,
	amount_refunded, card_token, card_brand, card_bin, card_last4, card_country, card_fingerprint, auth_code, auth_ref,
	capture_ref, decline_code, decline_message, risk_score, risk_action, risk_reasons, billing_country, customer_ip,
	challenge_id, description, metadata, cancellation_reason, version, created_at, updated_at, authorized_at,
	captured_at, canceled_at, auth_expires_at`

func scanPayment(row interface{ Scan(...any) error }) (*model.PaymentIntent, error) {
	var p model.PaymentIntent
	var token, brand, bin, last4, country, fp sql.NullString
	var authCode, authRef, captureRef, declineCode, declineMsg sql.NullString
	var riskAction, riskReasons, billing, ip, challenge, cancelReason sql.NullString
	var metadata string
	var created, updated int64
	var authorized, captured, canceled, authExp sql.NullInt64
	err := row.Scan(&p.ID, &p.MerchantID, &p.Amount, &p.Currency, &p.Status, &p.CaptureMethod,
		&p.AmountAuthorized, &p.AmountCaptured, &p.AmountRefunded, &token, &brand, &bin, &last4, &country, &fp,
		&authCode, &authRef, &captureRef, &declineCode, &declineMsg, &p.Risk.Score, &riskAction, &riskReasons,
		&billing, &ip, &challenge, &p.Description, &metadata, &cancelReason, &p.Version, &created, &updated,
		&authorized, &captured, &canceled, &authExp)
	if err != nil {
		return nil, notFound(err)
	}
	if token.Valid {
		p.Card = &model.CardSummary{Token: token.String, Brand: brand.String, BIN: bin.String,
			Last4: last4.String, Country: country.String, Fingerprint: fp.String}
	}
	p.AuthCode, p.AuthRef, p.CaptureRef = authCode.String, authRef.String, captureRef.String
	p.DeclineCode, p.DeclineMessage = declineCode.String, declineMsg.String
	p.Risk.Action = riskAction.String
	if riskReasons.Valid && riskReasons.String != "" {
		if err := json.Unmarshal([]byte(riskReasons.String), &p.Risk.Reasons); err != nil {
			return nil, err
		}
	}
	p.BillingCountry, p.CustomerIP, p.ChallengeID = billing.String, ip.String, challenge.String
	p.CancellationReason = cancelReason.String
	if err := json.Unmarshal([]byte(metadata), &p.Metadata); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = fromTS(created), fromTS(updated)
	p.AuthorizedAt, p.CapturedAt = fromNullTS(authorized), fromNullTS(captured)
	p.CanceledAt, p.AuthExpiresAt = fromNullTS(canceled), fromNullTS(authExp)
	return &p, nil
}

func paymentArgs(p *model.PaymentIntent) ([]any, error) {
	md := p.Metadata
	if md == nil {
		md = map[string]string{}
	}
	mdJSON, err := json.Marshal(md)
	if err != nil {
		return nil, err
	}
	var reasons sql.NullString
	if len(p.Risk.Reasons) > 0 {
		b, err := json.Marshal(p.Risk.Reasons)
		if err != nil {
			return nil, err
		}
		reasons = sql.NullString{String: string(b), Valid: true}
	}
	c := p.Card
	if c == nil {
		c = &model.CardSummary{}
	}
	return []any{p.ID, p.MerchantID, p.Amount, p.Currency, p.Status, p.CaptureMethod, p.AmountAuthorized,
		p.AmountCaptured, p.AmountRefunded, nullStr(c.Token), nullStr(c.Brand), nullStr(c.BIN), nullStr(c.Last4),
		nullStr(c.Country), nullStr(c.Fingerprint), nullStr(p.AuthCode), nullStr(p.AuthRef), nullStr(p.CaptureRef),
		nullStr(p.DeclineCode), nullStr(p.DeclineMessage), p.Risk.Score, nullStr(p.Risk.Action), reasons,
		nullStr(p.BillingCountry), nullStr(p.CustomerIP), nullStr(p.ChallengeID), p.Description, string(mdJSON),
		nullStr(p.CancellationReason), p.Version, ts(p.CreatedAt), ts(p.UpdatedAt), nullTS(p.AuthorizedAt),
		nullTS(p.CapturedAt), nullTS(p.CanceledAt), nullTS(p.AuthExpiresAt)}, nil
}

// InsertPayment stores a new payment intent.
func (q *Queries) InsertPayment(ctx context.Context, p *model.PaymentIntent) error {
	args, err := paymentArgs(p)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO payment_intents (`+paymentCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	return err
}

// UpdatePayment writes p using optimistic locking: the row must still be at
// p.Version, and is bumped to p.Version+1. On success p.Version is updated.
func (q *Queries) UpdatePayment(ctx context.Context, p *model.PaymentIntent) error {
	args, err := paymentArgs(p)
	if err != nil {
		return err
	}
	// args[0] is id; reorder for SET ... WHERE id = ? AND version = ?
	set := args[1:]
	set[28] = p.Version + 1 // version column
	res, err := q.db.ExecContext(ctx, `UPDATE payment_intents SET merchant_id = ?, amount = ?, currency = ?,
		status = ?, capture_method = ?, amount_authorized = ?, amount_captured = ?, amount_refunded = ?,
		card_token = ?, card_brand = ?, card_bin = ?, card_last4 = ?, card_country = ?, card_fingerprint = ?,
		auth_code = ?, auth_ref = ?, capture_ref = ?, decline_code = ?, decline_message = ?, risk_score = ?,
		risk_action = ?, risk_reasons = ?, billing_country = ?, customer_ip = ?, challenge_id = ?, description = ?,
		metadata = ?, cancellation_reason = ?, version = ?, created_at = ?, updated_at = ?, authorized_at = ?,
		captured_at = ?, canceled_at = ?, auth_expires_at = ? WHERE id = ? AND version = ?`,
		append(set, p.ID, p.Version)...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	p.Version++
	return nil
}

// GetPayment loads a payment intent by id.
func (q *Queries) GetPayment(ctx context.Context, id string) (*model.PaymentIntent, error) {
	return scanPayment(q.db.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payment_intents WHERE id = ?`, id))
}

// PaymentFilter narrows ListPayments.
type PaymentFilter struct {
	MerchantID    string
	Status        model.PaymentStatus
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	Page          Page
}

// ListPayments pages through a merchant's payments, newest first.
func (q *Queries) ListPayments(ctx context.Context, f PaymentFilter) ([]*model.PaymentIntent, bool, error) {
	var where []string
	var args []any
	if f.MerchantID != "" {
		where, args = append(where, "merchant_id = ?"), append(args, f.MerchantID)
	}
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.CreatedAfter != nil {
		where, args = append(where, "created_at >= ?"), append(args, ts(*f.CreatedAfter))
	}
	if f.CreatedBefore != nil {
		where, args = append(where, "created_at < ?"), append(args, ts(*f.CreatedBefore))
	}
	where, args, tail := pageClause(f.Page, where, args)
	rows, err := q.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM payment_intents`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []*model.PaymentIntent
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, f.Page)
	return out, more, nil
}

// ListExpiredAuthorizations returns authorized payments whose hold expired.
func (q *Queries) ListExpiredAuthorizations(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id FROM payment_intents
		WHERE status = 'authorized' AND auth_expires_at IS NOT NULL AND auth_expires_at <= ?
		ORDER BY auth_expires_at LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// InsertHistory records a state transition.
func (q *Queries) InsertHistory(ctx context.Context, h model.PaymentHistory) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO payment_history
		(id, payment_id, action, from_status, to_status, amount, detail, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		h.ID, h.PaymentID, h.Action, h.FromStatus, h.ToStatus, h.Amount, h.Detail, ts(h.CreatedAt))
	return err
}

// ListHistory returns a payment's transitions, oldest first.
func (q *Queries) ListHistory(ctx context.Context, paymentID string) ([]model.PaymentHistory, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id, payment_id, action, from_status, to_status, amount, detail, created_at
		FROM payment_history WHERE payment_id = ? ORDER BY id`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PaymentHistory
	for rows.Next() {
		var h model.PaymentHistory
		var created int64
		if err := rows.Scan(&h.ID, &h.PaymentID, &h.Action, &h.FromStatus, &h.ToStatus, &h.Amount, &h.Detail,
			&created); err != nil {
			return nil, err
		}
		h.CreatedAt = fromTS(created)
		out = append(out, h)
	}
	return out, rows.Err()
}

// CountPaymentsByStatus returns payment counts per status (for metrics/ops).
func (q *Queries) CountPaymentsByStatus(ctx context.Context) (map[model.PaymentStatus]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM payment_intents GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[model.PaymentStatus]int64{}
	for rows.Next() {
		var s model.PaymentStatus
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// --- refunds ------------------------------------------------------------------

const refundCols = `id, payment_id, merchant_id, amount, currency, status, reason, acquirer_ref, failure_code, created_at`

func scanRefund(row interface{ Scan(...any) error }) (model.Refund, error) {
	var r model.Refund
	var acqRef, failure sql.NullString
	var created int64
	err := row.Scan(&r.ID, &r.PaymentID, &r.MerchantID, &r.Amount, &r.Currency, &r.Status, &r.Reason, &acqRef,
		&failure, &created)
	if err != nil {
		return r, notFound(err)
	}
	r.AcquirerRef, r.FailureCode = acqRef.String, failure.String
	r.CreatedAt = fromTS(created)
	return r, nil
}

// InsertRefund stores a refund.
func (q *Queries) InsertRefund(ctx context.Context, r model.Refund) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO refunds (`+refundCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.PaymentID, r.MerchantID, r.Amount, r.Currency, r.Status, r.Reason, nullStr(r.AcquirerRef),
		nullStr(r.FailureCode), ts(r.CreatedAt))
	return err
}

// GetRefund loads a refund.
func (q *Queries) GetRefund(ctx context.Context, id string) (model.Refund, error) {
	return scanRefund(q.db.QueryRowContext(ctx, `SELECT `+refundCols+` FROM refunds WHERE id = ?`, id))
}

// ListRefunds returns a payment's refunds, oldest first.
func (q *Queries) ListRefunds(ctx context.Context, paymentID string) ([]model.Refund, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+refundCols+` FROM refunds WHERE payment_id = ? ORDER BY id`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Refund
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
