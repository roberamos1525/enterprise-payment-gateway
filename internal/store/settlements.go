package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// UnsettledCaptures returns a merchant's captures in [from, to) that are not
// yet part of any settlement batch.
func (q *Queries) UnsettledCaptures(ctx context.Context, merchantID string, from, to time.Time) ([]model.SettleableCapture, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT p.id, p.merchant_id, COALESCE(p.capture_ref,''), p.amount_captured,
		p.currency, p.captured_at FROM payment_intents p
		WHERE p.merchant_id = ? AND p.captured_at >= ? AND p.captured_at < ? AND p.amount_captured > 0
		AND NOT EXISTS (SELECT 1 FROM settlement_items s WHERE s.kind = 'capture' AND s.ref_id = p.id)
		ORDER BY p.captured_at, p.id`, merchantID, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SettleableCapture
	for rows.Next() {
		var c model.SettleableCapture
		var at int64
		if err := rows.Scan(&c.PaymentID, &c.MerchantID, &c.CaptureRef, &c.Amount, &c.Currency, &at); err != nil {
			return nil, err
		}
		c.CapturedAt = fromTS(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UnsettledRefunds returns a merchant's succeeded refunds in [from, to) that
// are not yet settled.
func (q *Queries) UnsettledRefunds(ctx context.Context, merchantID string, from, to time.Time) ([]model.SettleableRefund, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT r.id, r.payment_id, r.merchant_id, COALESCE(r.acquirer_ref,''), r.amount,
		r.currency, r.created_at FROM refunds r
		WHERE r.merchant_id = ? AND r.status = 'succeeded' AND r.created_at >= ? AND r.created_at < ?
		AND NOT EXISTS (SELECT 1 FROM settlement_items s WHERE s.kind = 'refund' AND s.ref_id = r.id)
		ORDER BY r.created_at, r.id`, merchantID, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SettleableRefund
	for rows.Next() {
		var r model.SettleableRefund
		var at int64
		if err := rows.Scan(&r.RefundID, &r.PaymentID, &r.MerchantID, &r.AcquirerRef, &r.Amount, &r.Currency, &at); err != nil {
			return nil, err
		}
		r.CreatedAt = fromTS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

const batchCols = `b.id, b.merchant_id, b.settlement_date, b.currency, b.gross_captured, b.refunded, b.fees, b.net,
	b.capture_count, b.refund_count, b.fee_schedule_id, b.status, b.created_at, COALESCE(p.id,'')`

func scanBatch(row interface{ Scan(...any) error }) (model.SettlementBatch, error) {
	var b model.SettlementBatch
	var created int64
	err := row.Scan(&b.ID, &b.MerchantID, &b.Date, &b.Currency, &b.GrossCaptured, &b.Refunded, &b.Fees, &b.Net,
		&b.CaptureCount, &b.RefundCount, &b.FeeScheduleID, &b.Status, &created, &b.PayoutID)
	if err != nil {
		return b, notFound(err)
	}
	b.CreatedAt = fromTS(created)
	return b, nil
}

// InsertBatch stores a settlement batch with its items. An item that is
// already part of another batch yields ErrConflict (items settle once).
func (q *Queries) InsertBatch(ctx context.Context, b model.SettlementBatch, items []model.SettlementItem) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO settlement_batches (id, merchant_id, settlement_date, currency,
		gross_captured, refunded, fees, net, capture_count, refund_count, fee_schedule_id, status, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, b.ID, b.MerchantID, b.Date, b.Currency, b.GrossCaptured, b.Refunded,
		b.Fees, b.Net, b.CaptureCount, b.RefundCount, b.FeeScheduleID, b.Status, ts(b.CreatedAt))
	if err != nil {
		return err
	}
	for _, it := range items {
		_, err := q.db.ExecContext(ctx, `INSERT INTO settlement_items (batch_id, kind, ref_id, payment_id, amount, fee,
			occurred_at) VALUES (?,?,?,?,?,?,?)`, b.ID, it.Kind, it.RefID, it.PaymentID, it.Amount, it.Fee, ts(it.OccurredAt))
		if isUnique(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// GetBatch loads a batch (with its payout id, if any).
func (q *Queries) GetBatch(ctx context.Context, id string) (model.SettlementBatch, error) {
	return scanBatch(q.db.QueryRowContext(ctx, `SELECT `+batchCols+` FROM settlement_batches b
		LEFT JOIN payouts p ON p.batch_id = b.id WHERE b.id = ?`, id))
}

// BatchFilter narrows ListBatches.
type BatchFilter struct {
	MerchantID string
	Date       string
	Page       Page
}

// ListBatches pages through settlement batches, newest first.
func (q *Queries) ListBatches(ctx context.Context, f BatchFilter) ([]model.SettlementBatch, bool, error) {
	var where []string
	var args []any
	if f.MerchantID != "" {
		where, args = append(where, "b.merchant_id = ?"), append(args, f.MerchantID)
	}
	if f.Date != "" {
		where, args = append(where, "b.settlement_date = ?"), append(args, f.Date)
	}
	if f.Page.StartingAfter != "" {
		where, args = append(where, "b.id < ?"), append(args, f.Page.StartingAfter)
	}
	args = append(args, f.Page.limit()+1)
	rows, err := q.db.QueryContext(ctx, `SELECT `+batchCols+` FROM settlement_batches b
		LEFT JOIN payouts p ON p.batch_id = b.id`+whereSQL(where)+` ORDER BY b.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.SettlementBatch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, f.Page)
	return out, more, nil
}

// ListBatchItems returns a batch's items in occurrence order.
func (q *Queries) ListBatchItems(ctx context.Context, batchID string) ([]model.SettlementItem, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT batch_id, kind, ref_id, payment_id, amount, fee, occurred_at
		FROM settlement_items WHERE batch_id = ? ORDER BY occurred_at, ref_id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SettlementItem
	for rows.Next() {
		var it model.SettlementItem
		var at int64
		if err := rows.Scan(&it.BatchID, &it.Kind, &it.RefID, &it.PaymentID, &it.Amount, &it.Fee, &at); err != nil {
			return nil, err
		}
		it.OccurredAt = fromTS(at)
		out = append(out, it)
	}
	return out, rows.Err()
}

// InsertPayout stores a payout.
func (q *Queries) InsertPayout(ctx context.Context, p model.Payout) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO payouts (id, batch_id, merchant_id, amount, currency, status,
		scheduled_for, created_at, paid_at) VALUES (?,?,?,?,?,?,?,?,?)`, p.ID, p.BatchID, p.MerchantID, p.Amount,
		p.Currency, p.Status, p.ScheduledFor, ts(p.CreatedAt), nullTS(p.PaidAt))
	return err
}

func scanPayout(row interface{ Scan(...any) error }) (model.Payout, error) {
	var p model.Payout
	var created int64
	var paid sql.NullInt64
	err := row.Scan(&p.ID, &p.BatchID, &p.MerchantID, &p.Amount, &p.Currency, &p.Status, &p.ScheduledFor, &created, &paid)
	if err != nil {
		return p, notFound(err)
	}
	p.CreatedAt, p.PaidAt = fromTS(created), fromNullTS(paid)
	return p, nil
}

const payoutCols = `id, batch_id, merchant_id, amount, currency, status, scheduled_for, created_at, paid_at`

// GetPayout loads a payout.
func (q *Queries) GetPayout(ctx context.Context, id string) (model.Payout, error) {
	return scanPayout(q.db.QueryRowContext(ctx, `SELECT `+payoutCols+` FROM payouts WHERE id = ?`, id))
}

// ListPayouts returns a merchant's payouts, newest first.
func (q *Queries) ListPayouts(ctx context.Context, merchantID string, page Page) ([]model.Payout, bool, error) {
	where, args, tail := pageClause(page, []string{"merchant_id = ?"}, []any{merchantID})
	rows, err := q.db.QueryContext(ctx, `SELECT `+payoutCols+` FROM payouts`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, page)
	return out, more, nil
}

// DuePayouts returns scheduled payouts whose date is on or before day.
func (q *Queries) DuePayouts(ctx context.Context, day string) ([]model.Payout, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+payoutCols+` FROM payouts WHERE status = 'scheduled'
		AND scheduled_for <= ? ORDER BY id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPayoutPaid flips a scheduled payout to paid.
func (q *Queries) MarkPayoutPaid(ctx context.Context, id string, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE payouts SET status = 'paid', paid_at = ? WHERE id = ? AND status = 'scheduled'`,
		ts(at), id)
	return affectedOne(res, err)
}
