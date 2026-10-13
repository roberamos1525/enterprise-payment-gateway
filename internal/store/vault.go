package store

import (
	"context"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

const cardCols = `token, merchant_id, fingerprint, key_id, ciphertext, brand, bin, last4, exp_month, exp_year,
	country, funding, created_at`

func scanCard(row interface{ Scan(...any) error }) (model.VaultCard, error) {
	var c model.VaultCard
	var created int64
	err := row.Scan(&c.Token, &c.MerchantID, &c.Fingerprint, &c.KeyID, &c.Ciphertext, &c.Brand, &c.BIN, &c.Last4,
		&c.ExpMonth, &c.ExpYear, &c.Country, &c.Funding, &created)
	if err != nil {
		return c, notFound(err)
	}
	c.CreatedAt = fromTS(created)
	return c, nil
}

// InsertCard stores a sealed card.
func (q *Queries) InsertCard(ctx context.Context, c model.VaultCard) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO vault_cards (`+cardCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Token, c.MerchantID, c.Fingerprint, c.KeyID, c.Ciphertext, c.Brand, c.BIN, c.Last4, c.ExpMonth,
		c.ExpYear, c.Country, c.Funding, ts(c.CreatedAt))
	return err
}

// GetCard loads a vault card by token.
func (q *Queries) GetCard(ctx context.Context, token string) (model.VaultCard, error) {
	return scanCard(q.db.QueryRowContext(ctx, `SELECT `+cardCols+` FROM vault_cards WHERE token = ?`, token))
}

// ListCardsNotUsingKey returns up to limit cards sealed with a key other than keyID.
func (q *Queries) ListCardsNotUsingKey(ctx context.Context, keyID string, limit int) ([]model.VaultCard, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+cardCols+` FROM vault_cards WHERE key_id <> ? ORDER BY token LIMIT ?`,
		keyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.VaultCard
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCardCiphertext replaces a card's sealed PAN (key rotation).
func (q *Queries) UpdateCardCiphertext(ctx context.Context, token, keyID string, ciphertext []byte) error {
	res, err := q.db.ExecContext(ctx, `UPDATE vault_cards SET key_id = ?, ciphertext = ? WHERE token = ?`,
		keyID, ciphertext, token)
	return affectedOne(res, err)
}

// CountCardsByKey returns the number of cards sealed under each key id.
func (q *Queries) CountCardsByKey(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT key_id, COUNT(*) FROM vault_cards GROUP BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
