package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// LastAuditHash returns the hash of the most recent audit entry ("" if none).
func (q *Queries) LastAuditHash(ctx context.Context) (string, error) {
	var h sql.NullString
	err := q.db.QueryRowContext(ctx, `SELECT hash FROM audit_logs ORDER BY seq DESC LIMIT 1`).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return h.String, err
}

// InsertAudit appends an audit entry and returns its sequence number.
func (q *Queries) InsertAudit(ctx context.Context, e model.AuditEntry) (int64, error) {
	md := string(e.Metadata)
	if md == "" {
		md = "{}"
	}
	res, err := q.db.ExecContext(ctx, `INSERT INTO audit_logs (id, created_at, actor_type, actor_id, action,
		resource_type, resource_id, merchant_id, request_id, ip, metadata, prev_hash, hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, ts(e.CreatedAt), e.ActorType, e.ActorID, e.Action, e.ResourceType,
		e.ResourceID, e.MerchantID, e.RequestID, e.IP, md, e.PrevHash, e.Hash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AuditFilter narrows ListAudit.
type AuditFilter struct {
	MerchantID   string
	ResourceType string
	ResourceID   string
	Action       string
	ActorID      string
	Since        *time.Time
	AfterSeq     int64 // exclusive cursor (ascending order)
	Limit        int
}

const auditCols = `seq, id, created_at, actor_type, actor_id, action, resource_type, resource_id, merchant_id,
	request_id, ip, metadata, prev_hash, hash`

func scanAudit(row interface{ Scan(...any) error }) (model.AuditEntry, error) {
	var e model.AuditEntry
	var created int64
	var md string
	err := row.Scan(&e.Seq, &e.ID, &created, &e.ActorType, &e.ActorID, &e.Action, &e.ResourceType, &e.ResourceID,
		&e.MerchantID, &e.RequestID, &e.IP, &md, &e.PrevHash, &e.Hash)
	if err != nil {
		return e, notFound(err)
	}
	e.CreatedAt = fromTS(created)
	e.Metadata = json.RawMessage(md)
	return e, nil
}

// ListAudit returns audit entries in ascending sequence order.
func (q *Queries) ListAudit(ctx context.Context, f AuditFilter) ([]model.AuditEntry, error) {
	where := []string{"seq > ?"}
	args := []any{f.AfterSeq}
	add := func(cond string, v string) {
		if v != "" {
			where, args = append(where, cond), append(args, v)
		}
	}
	add("merchant_id = ?", f.MerchantID)
	add("resource_type = ?", f.ResourceType)
	add("resource_id = ?", f.ResourceID)
	add("action = ?", f.Action)
	add("actor_id = ?", f.ActorID)
	if f.Since != nil {
		where, args = append(where, "created_at >= ?"), append(args, ts(*f.Since))
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := q.db.QueryContext(ctx, `SELECT `+auditCols+` FROM audit_logs`+whereSQL(where)+
		` ORDER BY seq LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditEntry
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
