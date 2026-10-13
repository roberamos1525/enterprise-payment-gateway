package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

const merchantCols = `id, name, email, country, default_currency, status, created_at, updated_at`

func scanMerchant(row interface{ Scan(...any) error }) (model.Merchant, error) {
	var m model.Merchant
	var created, updated int64
	err := row.Scan(&m.ID, &m.Name, &m.Email, &m.Country, &m.DefaultCurrency, &m.Status, &created, &updated)
	if err != nil {
		return m, notFound(err)
	}
	m.CreatedAt, m.UpdatedAt = fromTS(created), fromTS(updated)
	return m, nil
}

// InsertMerchant creates a merchant. Duplicate emails yield ErrConflict.
func (q *Queries) InsertMerchant(ctx context.Context, m model.Merchant) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO merchants (`+merchantCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, m.Name, m.Email, m.Country, m.DefaultCurrency, m.Status, ts(m.CreatedAt), ts(m.UpdatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetMerchant loads a merchant by id.
func (q *Queries) GetMerchant(ctx context.Context, id string) (model.Merchant, error) {
	return scanMerchant(q.db.QueryRowContext(ctx, `SELECT `+merchantCols+` FROM merchants WHERE id = ?`, id))
}

// UpdateMerchantStatus changes a merchant's status.
func (q *Queries) UpdateMerchantStatus(ctx context.Context, id string, status model.MerchantStatus, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE merchants SET status = ?, updated_at = ? WHERE id = ?`, status, ts(at), id)
	return affectedOne(res, err)
}

// UpdateMerchantProfile changes the editable profile fields.
func (q *Queries) UpdateMerchantProfile(ctx context.Context, m model.Merchant) error {
	res, err := q.db.ExecContext(ctx, `UPDATE merchants SET name = ?, email = ?, updated_at = ? WHERE id = ?`,
		m.Name, m.Email, ts(m.UpdatedAt), m.ID)
	if isUnique(err) {
		return ErrConflict
	}
	return affectedOne(res, err)
}

// MerchantFilter narrows ListMerchants.
type MerchantFilter struct {
	Status model.MerchantStatus
	Page   Page
}

// ListMerchants pages through merchants, newest first.
func (q *Queries) ListMerchants(ctx context.Context, f MerchantFilter) ([]model.Merchant, bool, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	where, args, tail := pageClause(f.Page, where, args)
	rows, err := q.db.QueryContext(ctx, `SELECT `+merchantCols+` FROM merchants`+whereSQL(where)+tail, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []model.Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	out, more := trimPage(out, f.Page)
	return out, more, nil
}

// ListMerchantIDs returns every merchant id (used by batch jobs).
func (q *Queries) ListMerchantIDs(ctx context.Context) ([]string, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id FROM merchants ORDER BY id`)
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

// --- fee schedules ---------------------------------------------------------

const feeCols = `id, merchant_id, percent_bps, fixed_minor, refund_fixed_minor, return_fee_on_refund, effective_from, created_at`

func scanFee(row interface{ Scan(...any) error }) (model.FeeSchedule, error) {
	var f model.FeeSchedule
	var ret int
	var eff, created int64
	err := row.Scan(&f.ID, &f.MerchantID, &f.PercentBps, &f.FixedMinor, &f.RefundFixedMinor, &ret, &eff, &created)
	if err != nil {
		return f, notFound(err)
	}
	f.ReturnFeeOnRefund = ret == 1
	f.EffectiveFrom, f.CreatedAt = fromTS(eff), fromTS(created)
	return f, nil
}

// InsertFeeSchedule adds a new fee schedule version.
func (q *Queries) InsertFeeSchedule(ctx context.Context, f model.FeeSchedule) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO fee_schedules (`+feeCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		f.ID, f.MerchantID, f.PercentBps, f.FixedMinor, f.RefundFixedMinor, boolInt(f.ReturnFeeOnRefund),
		ts(f.EffectiveFrom), ts(f.CreatedAt))
	return err
}

// EffectiveFeeSchedule returns the schedule in force at time at.
func (q *Queries) EffectiveFeeSchedule(ctx context.Context, merchantID string, at time.Time) (model.FeeSchedule, error) {
	return scanFee(q.db.QueryRowContext(ctx, `SELECT `+feeCols+` FROM fee_schedules
		WHERE merchant_id = ? AND effective_from <= ? ORDER BY effective_from DESC, id DESC LIMIT 1`,
		merchantID, ts(at)))
}

// ListFeeSchedules returns every schedule version, newest effective first.
func (q *Queries) ListFeeSchedules(ctx context.Context, merchantID string) ([]model.FeeSchedule, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+feeCols+` FROM fee_schedules WHERE merchant_id = ?
		ORDER BY effective_from DESC, id DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FeeSchedule
	for rows.Next() {
		f, err := scanFee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- API keys ----------------------------------------------------------------

const keyCols = `id, merchant_id, prefix, hash, label, status, rotated_from, created_at, expires_at, revoked_at, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (model.APIKey, error) {
	var k model.APIKey
	var rotated sql.NullString
	var created int64
	var expires, revoked, used sql.NullInt64
	err := row.Scan(&k.ID, &k.MerchantID, &k.Prefix, &k.Hash, &k.Label, &k.Status, &rotated,
		&created, &expires, &revoked, &used)
	if err != nil {
		return k, notFound(err)
	}
	k.RotatedFrom = rotated.String
	k.CreatedAt = fromTS(created)
	k.ExpiresAt, k.RevokedAt, k.LastUsedAt = fromNullTS(expires), fromNullTS(revoked), fromNullTS(used)
	return k, nil
}

// InsertAPIKey stores a new key (hash only).
func (q *Queries) InsertAPIKey(ctx context.Context, k model.APIKey) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO api_keys (`+keyCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID, k.MerchantID, k.Prefix, k.Hash, k.Label, k.Status, nullStr(k.RotatedFrom),
		ts(k.CreatedAt), nullTS(k.ExpiresAt), nullTS(k.RevokedAt), nullTS(k.LastUsedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetAPIKey loads a key by id.
func (q *Queries) GetAPIKey(ctx context.Context, id string) (model.APIKey, error) {
	return scanKey(q.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id = ?`, id))
}

// GetAPIKeyByPrefix loads a key by its public prefix.
func (q *Queries) GetAPIKeyByPrefix(ctx context.Context, prefix string) (model.APIKey, error) {
	return scanKey(q.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE prefix = ?`, prefix))
}

// ListAPIKeys lists a merchant's keys, newest first.
func (q *Queries) ListAPIKeys(ctx context.Context, merchantID string) ([]model.APIKey, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE merchant_id = ? ORDER BY id DESC`, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAPIKeyExpiry sets (or shortens) a key's expiry.
func (q *Queries) SetAPIKeyExpiry(ctx context.Context, id string, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE id = ?`, ts(at), id)
	return affectedOne(res, err)
}

// RevokeAPIKey marks a key revoked.
func (q *Queries) RevokeAPIKey(ctx context.Context, id string, at time.Time) error {
	res, err := q.db.ExecContext(ctx, `UPDATE api_keys SET status = 'revoked', revoked_at = ? WHERE id = ? AND status = 'active'`,
		ts(at), id)
	return affectedOne(res, err)
}

// TouchAPIKey records the last time a key was used.
func (q *Queries) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	_, err := q.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, ts(at), id)
	return err
}

func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	switch n {
	case 0:
		return ErrNotFound
	case 1:
		return nil
	}
	return fmt.Errorf("store: expected 1 row affected, got %d", n)
}
