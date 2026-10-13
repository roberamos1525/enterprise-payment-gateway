package store

import (
	"context"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// --- idempotency ----------------------------------------------------------------

// GetIdempotency loads an idempotency record.
func (q *Queries) GetIdempotency(ctx context.Context, merchantID, key string) (model.IdempotencyRecord, error) {
	var r model.IdempotencyRecord
	var status string
	var respStatus *int64
	var created, expires, locked int64
	err := q.db.QueryRowContext(ctx, `SELECT merchant_id, key, method, path, fingerprint, status, response_status,
		response_body, created_at, expires_at, locked_until FROM idempotency_keys WHERE merchant_id = ? AND key = ?`,
		merchantID, key).Scan(&r.MerchantID, &r.Key, &r.Method, &r.Path, &r.Fingerprint, &status, &respStatus,
		&r.ResponseBody, &created, &expires, &locked)
	if err != nil {
		return r, notFound(err)
	}
	r.Completed = status == "completed"
	if respStatus != nil {
		r.ResponseStatus = int(*respStatus)
	}
	r.CreatedAt, r.ExpiresAt, r.LockedUntil = fromTS(created), fromTS(expires), fromTS(locked)
	return r, nil
}

// UpsertIdempotencyLock inserts an in-progress record, or replaces an expired
// one. It returns ErrConflict if a live record already exists.
func (q *Queries) UpsertIdempotencyLock(ctx context.Context, r model.IdempotencyRecord) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE merchant_id = ? AND key = ? AND expires_at <= ?`,
		r.MerchantID, r.Key, ts(r.CreatedAt)); err != nil {
		return err
	}
	_, err := q.db.ExecContext(ctx, `INSERT INTO idempotency_keys (merchant_id, key, method, path, fingerprint,
		status, created_at, expires_at, locked_until) VALUES (?,?,?,?,?,'in_progress',?,?,?)`,
		r.MerchantID, r.Key, r.Method, r.Path, r.Fingerprint, ts(r.CreatedAt), ts(r.ExpiresAt), ts(r.LockedUntil))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// RelockIdempotency takes over an in-progress record whose lock lapsed
// (e.g. the original request's process crashed).
func (q *Queries) RelockIdempotency(ctx context.Context, merchantID, key string, now, until time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE idempotency_keys SET locked_until = ?
		WHERE merchant_id = ? AND key = ? AND status = 'in_progress' AND locked_until <= ?`,
		ts(until), merchantID, key, ts(now))
	if err := affectedOne(res, err); err == ErrNotFound {
		return ErrConflict
	} else if err != nil {
		return err
	}
	return nil
}

// CompleteIdempotency stores the final response for replay.
func (q *Queries) CompleteIdempotency(ctx context.Context, merchantID, key string, status int, body []byte) error {
	res, err := q.db.ExecContext(ctx, `UPDATE idempotency_keys SET status = 'completed', response_status = ?,
		response_body = ?, locked_until = 0 WHERE merchant_id = ? AND key = ?`, status, body, merchantID, key)
	return affectedOne(res, err)
}

// DeleteIdempotency removes a record (used when a request must not be cached,
// e.g. it failed with a retryable server error).
func (q *Queries) DeleteIdempotency(ctx context.Context, merchantID, key string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE merchant_id = ? AND key = ?`, merchantID, key)
	return err
}

// PurgeIdempotency deletes expired records and returns how many were removed.
func (q *Queries) PurgeIdempotency(ctx context.Context, now time.Time) (int64, error) {
	res, err := q.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= ?`, ts(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- fraud blocklist -------------------------------------------------------------

// InsertBlockedBIN adds a BIN to the blocklist (ErrConflict if present).
func (q *Queries) InsertBlockedBIN(ctx context.Context, b model.BlockedBIN) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO fraud_bin_blocklist (bin, reason, created_by, created_at) VALUES (?,?,?,?)`,
		b.BIN, b.Reason, b.CreatedBy, ts(b.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// DeleteBlockedBIN removes a BIN from the blocklist.
func (q *Queries) DeleteBlockedBIN(ctx context.Context, bin string) error {
	res, err := q.db.ExecContext(ctx, `DELETE FROM fraud_bin_blocklist WHERE bin = ?`, bin)
	return affectedOne(res, err)
}

// ListBlockedBINs returns the whole blocklist.
func (q *Queries) ListBlockedBINs(ctx context.Context) ([]model.BlockedBIN, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT bin, reason, created_by, created_at FROM fraud_bin_blocklist ORDER BY bin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BlockedBIN
	for rows.Next() {
		var b model.BlockedBIN
		var created int64
		if err := rows.Scan(&b.BIN, &b.Reason, &b.CreatedBy, &created); err != nil {
			return nil, err
		}
		b.CreatedAt = fromTS(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBlockedBIN looks up one blocklist entry.
func (q *Queries) GetBlockedBIN(ctx context.Context, bin string) (model.BlockedBIN, error) {
	var b model.BlockedBIN
	var created int64
	err := q.db.QueryRowContext(ctx, `SELECT bin, reason, created_by, created_at FROM fraud_bin_blocklist WHERE bin = ?`,
		bin).Scan(&b.BIN, &b.Reason, &b.CreatedBy, &created)
	if err != nil {
		return b, notFound(err)
	}
	b.CreatedAt = fromTS(created)
	return b, nil
}
