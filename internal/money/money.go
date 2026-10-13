// Package money represents monetary amounts as int64 minor units plus an
// ISO 4217 currency. Floating point is never used.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Currency describes an ISO 4217 currency supported by the gateway.
type Currency struct {
	Code     string
	Numeric  string
	Exponent int // number of minor-unit digits
}

// supported lists the currencies the simulated acquirer accepts. Exponents
// follow ISO 4217 (JPY/KRW have none, the dinars have three).
var supported = map[string]Currency{
	"AUD": {"AUD", "036", 2}, "BHD": {"BHD", "048", 3}, "BRL": {"BRL", "986", 2},
	"CAD": {"CAD", "124", 2}, "CHF": {"CHF", "756", 2}, "CLP": {"CLP", "152", 0},
	"CZK": {"CZK", "203", 2}, "DKK": {"DKK", "208", 2}, "EUR": {"EUR", "978", 2},
	"GBP": {"GBP", "826", 2}, "HKD": {"HKD", "344", 2}, "HUF": {"HUF", "348", 2},
	"INR": {"INR", "356", 2}, "ISK": {"ISK", "352", 0}, "JOD": {"JOD", "400", 3},
	"JPY": {"JPY", "392", 0}, "KRW": {"KRW", "410", 0}, "KWD": {"KWD", "414", 3},
	"MXN": {"MXN", "484", 2}, "NOK": {"NOK", "578", 2}, "NZD": {"NZD", "554", 2},
	"OMR": {"OMR", "512", 3}, "PLN": {"PLN", "985", 2}, "SEK": {"SEK", "752", 2},
	"SGD": {"SGD", "702", 2}, "USD": {"USD", "840", 2}, "ZAR": {"ZAR", "710", 2},
}

// Errors returned by the package.
var (
	ErrUnknownCurrency  = errors.New("money: unsupported currency")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: amount overflow")
	ErrInvalidDecimal   = errors.New("money: invalid decimal amount")
)

// LookupCurrency returns the currency for a code (case-insensitive).
func LookupCurrency(code string) (Currency, error) {
	c, ok := supported[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

// NormalizeCurrency validates a currency code and returns it upper-cased.
func NormalizeCurrency(code string) (string, error) {
	c, err := LookupCurrency(code)
	if err != nil {
		return "", err
	}
	return c.Code, nil
}

// Amount is a value in minor units of a currency.
type Amount struct {
	Minor    int64  `json:"amount"`
	Currency string `json:"currency"`
}

// New validates the currency and builds an amount.
func New(minor int64, currency string) (Amount, error) {
	code, err := NormalizeCurrency(currency)
	if err != nil {
		return Amount{}, err
	}
	return Amount{Minor: minor, Currency: code}, nil
}

// Must is New for compile-time-known values; it panics on error.
func Must(minor int64, currency string) Amount {
	a, err := New(minor, currency)
	if err != nil {
		panic(err)
	}
	return a
}

// Zero returns a zero amount in the currency.
func Zero(currency string) Amount { return Amount{Currency: strings.ToUpper(currency)} }

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.Minor == 0 }

// IsNegative reports whether the amount is below zero.
func (a Amount) IsNegative() bool { return a.Minor < 0 }

// Add returns a+b, failing on currency mismatch or int64 overflow.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.Currency != b.Currency {
		return Amount{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	s, ok := AddInt64(a.Minor, b.Minor)
	if !ok {
		return Amount{}, ErrOverflow
	}
	return Amount{Minor: s, Currency: a.Currency}, nil
}

// Sub returns a-b.
func (a Amount) Sub(b Amount) (Amount, error) {
	if b.Minor == math.MinInt64 {
		return Amount{}, ErrOverflow
	}
	return a.Add(Amount{Minor: -b.Minor, Currency: b.Currency})
}

// Cmp compares two amounts of the same currency (-1, 0, 1).
func (a Amount) Cmp(b Amount) (int, error) {
	if a.Currency != b.Currency {
		return 0, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.Currency, b.Currency)
	}
	switch {
	case a.Minor < b.Minor:
		return -1, nil
	case a.Minor > b.Minor:
		return 1, nil
	}
	return 0, nil
}

// String renders the amount as a decimal with its currency, e.g. "12.34 USD".
func (a Amount) String() string {
	return FormatMinor(a.Minor, a.Currency) + " " + a.Currency
}

// AddInt64 adds with overflow detection.
func AddInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// FormatMinor renders minor units as a decimal string using the currency's
// exponent ("1234","USD" -> "12.34"; "1234","JPY" -> "1234").
func FormatMinor(minor int64, currency string) string {
	exp := 2
	if c, err := LookupCurrency(currency); err == nil {
		exp = c.Exponent
	}
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = uint64(-(minor + 1)) + 1 // safe for MinInt64
	}
	digits := strconv.FormatUint(u, 10)
	if exp > 0 {
		if len(digits) <= exp {
			digits = strings.Repeat("0", exp-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-exp] + "." + digits[len(digits)-exp:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// ParseDecimal converts a decimal string ("12.3", "-0.05") into minor units of
// the currency. More fractional digits than the exponent allows is an error;
// no rounding is ever applied to client input.
func ParseDecimal(s, currency string) (Amount, error) {
	c, err := LookupCurrency(currency)
	if err != nil {
		return Amount{}, err
	}
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") || len(frac) > c.Exponent {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	frac += strings.Repeat("0", c.Exponent-len(frac))
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
		}
	}
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	if neg {
		v = -v
	}
	return Amount{Minor: v, Currency: c.Code}, nil
}

// SupportedCurrencies returns the supported currency codes (unordered).
func SupportedCurrencies() []string {
	out := make([]string, 0, len(supported))
	for k := range supported {
		out = append(out, k)
	}
	return out
}
