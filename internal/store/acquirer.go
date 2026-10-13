package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// InsertAcquirerTxn records a transaction on the simulated acquirer's books.
func (q *Queries) InsertAcquirerTxn(ctx context.Context, t model.AcquirerTxn) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO acquirer_transactions (ref, kind, parent_ref, merchant_id, amount,
		currency, approved, response, created_at) VALUES (?,?,?,?,?,?,?,?,?)`, t.Ref, t.Kind, nullStr(t.ParentRef),
		t.MerchantID, t.Amount, t.Currency, boolInt(t.Approved), t.Response, ts(t.CreatedAt))
	return err
}

func scanAcqTxn(row interface{ Scan(...any) error }) (model.AcquirerTxn, error) {
	var t model.AcquirerTxn
	var parent sql.NullString
	var approved int
	var created int64
	err := row.Scan(&t.Ref, &t.Kind, &parent, &t.MerchantID, &t.Amount, &t.Currency, &approved, &t.Response, &created)
	if err != nil {
		return t, notFound(err)
	}
	t.ParentRef = parent.String
	t.Approved = approved == 1
	t.CreatedAt = fromTS(created)
	return t, nil
}

const acqCols = `ref, kind, parent_ref, merchant_id, amount, currency, approved, response, created_at`

// GetAcquirerTxn loads an acquirer transaction by reference.
func (q *Queries) GetAcquirerTxn(ctx context.Context, ref string) (model.AcquirerTxn, error) {
	return scanAcqTxn(q.db.QueryRowContext(ctx, `SELECT `+acqCols+` FROM acquirer_transactions WHERE ref = ?`, ref))
}

// SumAcquirerChildren totals approved child transactions (captures/refunds)
// of a parent reference.
func (q *Queries) SumAcquirerChildren(ctx context.Context, parentRef, kind string) (int64, error) {
	var n sql.NullInt64
	err := q.db.QueryRowContext(ctx, `SELECT SUM(amount) FROM acquirer_transactions
		WHERE parent_ref = ? AND kind = ? AND approved = 1`, parentRef, kind).Scan(&n)
	return n.Int64, err
}

// ListAcquirerSettled returns approved captures and refunds in [from, to),
// i.e. the rows the acquirer includes in its daily settlement file.
func (q *Queries) ListAcquirerSettled(ctx context.Context, from, to time.Time) ([]model.AcquirerTxn, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+acqCols+` FROM acquirer_transactions
		WHERE kind IN ('capture','refund') AND approved = 1 AND created_at >= ? AND created_at < ?
		ORDER BY created_at, ref`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AcquirerTxn
	for rows.Next() {
		t, err := scanAcqTxn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- reconciliation ---------------------------------------------------------------

// InsertReconciliation stores a run with its discrepancies.
func (q *Queries) InsertReconciliation(ctx context.Context, r model.ReconciliationRun) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO reconciliation_runs (id, settlement_date, source, internal_count,
		acquirer_count, matched_count, discrepancy_count, status, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Date, r.Source, r.InternalCount, r.AcquirerCount, r.MatchedCount, r.DiscrepancyCount, r.Status,
		ts(r.CreatedAt))
	if err != nil {
		return err
	}
	for _, d := range r.Discrepancies {
		if _, err := q.db.ExecContext(ctx, `INSERT INTO reconciliation_discrepancies (id, run_id, kind, acquirer_ref,
			merchant_id, internal_amount, acquirer_amount, currency, detail) VALUES (?,?,?,?,?,?,?,?,?)`,
			d.ID, r.ID, d.Kind, d.AcquirerRef, d.MerchantID, d.InternalAmount, d.AcquirerAmount, d.Currency,
			d.Detail); err != nil {
			return err
		}
	}
	return nil
}

const reconCols = `id, settlement_date, source, internal_count, acquirer_count, matched_count, discrepancy_count,
	status, created_at`

func scanRecon(row interface{ Scan(...any) error }) (model.ReconciliationRun, error) {
	var r model.ReconciliationRun
	var created int64
	err := row.Scan(&r.ID, &r.Date, &r.Source, &r.InternalCount, &r.AcquirerCount, &r.MatchedCount,
		&r.DiscrepancyCount, &r.Status, &created)
	if err != nil {
		return r, notFound(err)
	}
	r.CreatedAt = fromTS(created)
	return r, nil
}

// GetReconciliation loads a run including its discrepancies.
func (q *Queries) GetReconciliation(ctx context.Context, id string) (model.ReconciliationRun, error) {
	r, err := scanRecon(q.db.QueryRowContext(ctx, `SELECT `+reconCols+` FROM reconciliation_runs WHERE id = ?`, id))
	if err != nil {
		return r, err
	}
	rows, err := q.db.QueryContext(ctx, `SELECT id, run_id, kind, acquirer_ref, merchant_id, internal_amount,
		acquirer_amount, currency, detail FROM reconciliation_discrepancies WHERE run_id = ? ORDER BY kind, acquirer_ref`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var d model.Discrepancy
		var ia, aa sql.NullInt64
		if err := rows.Scan(&d.ID, &d.RunID, &d.Kind, &d.AcquirerRef, &d.MerchantID, &ia, &aa, &d.Currency,
			&d.Detail); err != nil {
			return r, err
		}
		if ia.Valid {
			d.InternalAmount = &ia.Int64
		}
		if aa.Valid {
			d.AcquirerAmount = &aa.Int64
		}
		r.Discrepancies = append(r.Discrepancies, d)
	}
	return r, rows.Err()
}

// ListReconciliations pages through runs, newest first.
func (q *Queries) ListReconciliations(ctx context.Context, page Page) ([]model.ReconciliationRun, bool, error) {
	where, args, tail := pageClause(page, nil, nil)
	rows, err := q.db.QueryContext(ctx, `SELECT `+reconCols+` FROM reconciliation_runs`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.ReconciliationRun
	for rows.Next() {
		r, err := scanRecon(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, page)
	return out, more, nil
}
