package acquirer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
)

// Record types in the settlement file.
const (
	RecordCapture = "CAPTURE"
	RecordRefund  = "REFUND"
)

// FileHeader is the CSV header of a settlement file.
var FileHeader = []string{"record_type", "acquirer_ref", "parent_ref", "merchant_id", "amount", "currency", "transaction_time"}

// FileRow is one line of the acquirer's daily settlement file.
type FileRow struct {
	RecordType string    `json:"record_type"`
	Ref        string    `json:"acquirer_ref"`
	ParentRef  string    `json:"parent_ref"`
	MerchantID string    `json:"merchant_id"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	Time       time.Time `json:"transaction_time"`
}

// SettlementFile builds the acquirer's settlement file for a UTC day from
// its own books. With a non-zero DiscrepancyRate some rows are perturbed
// deterministically (dropped, amount changed, duplicated, or accompanied by
// a phantom row) so reconciliation has something to find.
func (a *Acquirer) SettlementFile(ctx context.Context, day time.Time) ([]FileRow, error) {
	from := clock.StartOfDay(day)
	txns, err := a.db.Q().ListAcquirerSettled(ctx, from, from.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	rows := make([]FileRow, 0, len(txns))
	for _, t := range txns {
		rt := RecordCapture
		if t.Kind == "refund" {
			rt = RecordRefund
		}
		row := FileRow{RecordType: rt, Ref: t.Ref, ParentRef: t.ParentRef, MerchantID: t.MerchantID,
			Amount: t.Amount, Currency: t.Currency, Time: t.CreatedAt}
		rows = append(rows, a.perturb(row)...)
	}
	return rows, nil
}

// perturb applies the configured discrepancy injection to one row.
func (a *Acquirer) perturb(row FileRow) []FileRow {
	if a.cfg.DiscrepancyRate <= 0 {
		return []FileRow{row}
	}
	sum := sha256.Sum256([]byte(a.cfg.DiscrepancySeed + "|" + row.Ref))
	u := float64(binary.BigEndian.Uint64(sum[:8])>>11) / float64(1<<53)
	if u >= a.cfg.DiscrepancyRate {
		return []FileRow{row}
	}
	switch sum[8] % 4 {
	case 0: // the acquirer has no record of this transaction
		return nil
	case 1: // the acquirer settled a different amount
		row.Amount += int64(sum[9]%100) + 1
		return []FileRow{row}
	case 2: // the acquirer reported the transaction twice
		return []FileRow{row, row}
	default: // the acquirer reports a transaction the gateway never saw
		phantom := row
		phantom.Ref = "acq_unknown_" + strings.TrimPrefix(row.Ref, "acq_")
		phantom.ParentRef = ""
		return []FileRow{row, phantom}
	}
}

// WriteCSV renders rows as a settlement file.
func WriteCSV(w io.Writer, rows []FileRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(FileHeader); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{r.RecordType, r.Ref, r.ParentRef, r.MerchantID, strconv.FormatInt(r.Amount, 10),
			r.Currency, r.Time.UTC().Format(time.RFC3339Nano)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ParseError points at a bad line in an uploaded settlement file.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// MaxFileRows bounds uploaded settlement files.
const MaxFileRows = 200_000

// ParseCSV parses and validates a settlement file. It rejects files that
// contain anything resembling a PAN, which must never appear in such files.
func ParseCSV(r io.Reader) ([]FileRow, error) {
	cr := csv.NewReader(bufio.NewReader(r))
	cr.FieldsPerRecord = len(FileHeader)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		return nil, &ParseError{Line: 1, Msg: "missing or malformed header"}
	}
	for i, h := range FileHeader {
		if strings.TrimSpace(strings.ToLower(header[i])) != h {
			return nil, &ParseError{Line: 1, Msg: fmt.Sprintf("column %d must be %q", i+1, h)}
		}
	}
	var rows []FileRow
	line := 1
	for {
		rec, err := cr.Read()
		line++
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &ParseError{Line: line, Msg: "malformed CSV record"}
		}
		if len(rows) >= MaxFileRows {
			return nil, &ParseError{Line: line, Msg: fmt.Sprintf("file exceeds %d rows", MaxFileRows)}
		}
		row, msg := parseRow(rec)
		if msg != "" {
			return nil, &ParseError{Line: line, Msg: msg}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseRow(rec []string) (FileRow, string) {
	for i, f := range rec {
		if i != 4 && card.RedactPANs(f) != f { // the amount column is numeric by design
			return FileRow{}, "field contains a card number; settlement files must not carry PANs"
		}
	}
	row := FileRow{
		RecordType: strings.ToUpper(strings.TrimSpace(rec[0])), Ref: strings.TrimSpace(rec[1]),
		ParentRef: strings.TrimSpace(rec[2]), MerchantID: strings.TrimSpace(rec[3]),
	}
	if row.RecordType != RecordCapture && row.RecordType != RecordRefund {
		return row, "record_type must be CAPTURE or REFUND"
	}
	if row.Ref == "" || len(row.Ref) > 64 {
		return row, "acquirer_ref is required (max 64 chars)"
	}
	amt, err := strconv.ParseInt(strings.TrimSpace(rec[4]), 10, 64)
	if err != nil || amt <= 0 {
		return row, "amount must be a positive integer in minor units"
	}
	row.Amount = amt
	cur, err := money.NormalizeCurrency(rec[5])
	if err != nil {
		return row, "currency must be a supported ISO 4217 code"
	}
	row.Currency = cur
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rec[6]))
	if err != nil {
		return row, "transaction_time must be RFC 3339"
	}
	row.Time = t.UTC()
	return row, ""
}
