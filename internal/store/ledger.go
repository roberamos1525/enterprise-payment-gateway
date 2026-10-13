package store

import (
	"context"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// InsertJournal writes a journal and its entries. A journal with the same
// (kind, ref) already present yields ErrConflict, which makes postings
// idempotent per business event.
func (q *Queries) InsertJournal(ctx context.Context, j model.Journal) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO ledger_journals (id, kind, ref, merchant_id, currency, created_at)
		VALUES (?,?,?,?,?,?)`, j.ID, j.Kind, j.Ref, j.MerchantID, j.Currency, ts(j.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	for _, e := range j.Entries {
		if _, err := q.db.ExecContext(ctx, `INSERT INTO ledger_entries
			(journal_id, account, merchant_id, currency, debit, credit, created_at) VALUES (?,?,?,?,?,?,?)`,
			j.ID, e.Account, j.MerchantID, j.Currency, e.Debit, e.Credit, ts(j.CreatedAt)); err != nil {
			return err
		}
	}
	return nil
}

// AccountBalances aggregates entries per account for a merchant.
func (q *Queries) AccountBalances(ctx context.Context, merchantID string) ([]model.AccountBalance, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT account, merchant_id, currency, SUM(debit), SUM(credit)
		FROM ledger_entries WHERE merchant_id = ? GROUP BY account, merchant_id, currency ORDER BY account, currency`,
		merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AccountBalance
	for rows.Next() {
		var b model.AccountBalance
		if err := rows.Scan(&b.Account, &b.MerchantID, &b.Currency, &b.Debits, &b.Credits); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TrialBalance returns total debits and credits per currency across the
// whole ledger; in a healthy ledger they are equal.
func (q *Queries) TrialBalance(ctx context.Context) (map[string][2]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT currency, SUM(debit), SUM(credit) FROM ledger_entries GROUP BY currency`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var cur string
		var d, c int64
		if err := rows.Scan(&cur, &d, &c); err != nil {
			return nil, err
		}
		out[cur] = [2]int64{d, c}
	}
	return out, rows.Err()
}

// ListJournals returns journals for a merchant within [from, to), oldest first.
func (q *Queries) ListJournals(ctx context.Context, merchantID string, kinds []string, from, to time.Time) ([]model.Journal, error) {
	query := `SELECT id, kind, ref, merchant_id, currency, created_at FROM ledger_journals
		WHERE created_at >= ? AND created_at < ?`
	args := []any{ts(from), ts(to)}
	if merchantID != "" {
		query += ` AND merchant_id = ?`
		args = append(args, merchantID)
	}
	if len(kinds) > 0 {
		query += ` AND kind IN (?` + repeatPlaceholders(len(kinds)-1) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := q.db.QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	var out []model.Journal
	for rows.Next() {
		var j model.Journal
		var created int64
		if err := rows.Scan(&j.ID, &j.Kind, &j.Ref, &j.MerchantID, &j.Currency, &created); err != nil {
			rows.Close()
			return nil, err
		}
		j.CreatedAt = fromTS(created)
		out = append(out, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		entries, err := q.journalEntries(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Entries = entries
	}
	return out, nil
}

func (q *Queries) journalEntries(ctx context.Context, journalID string) ([]model.JournalEntry, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT account, debit, credit FROM ledger_entries WHERE journal_id = ? ORDER BY id`,
		journalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.JournalEntry
	for rows.Next() {
		var e model.JournalEntry
		if err := rows.Scan(&e.Account, &e.Debit, &e.Credit); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func repeatPlaceholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += ",?"
	}
	return s
}
