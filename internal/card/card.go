// Package card implements primary-account-number (PAN) handling for the
// simulated network: Luhn validation, BIN-based brand detection, masking,
// expiry/CVC checks and redaction of PAN-like strings.
package card

import (
	"regexp"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
)

// Brand is a card scheme.
type Brand string

// Supported brands.
const (
	Visa       Brand = "visa"
	Mastercard Brand = "mastercard"
	Amex       Brand = "amex"
	Discover   Brand = "discover"
	JCB        Brand = "jcb"
	Diners     Brand = "diners"
	UnionPay   Brand = "unionpay"
	Maestro    Brand = "maestro"
	Unknown    Brand = "unknown"
)

type binRange struct {
	lo, hi  int // inclusive, compared against the first `digits` digits
	digits  int
	brand   Brand
	lengths []int
}

// Ranges are checked in order; more specific prefixes come first.
var binRanges = []binRange{
	{34, 34, 2, Amex, []int{15}},
	{37, 37, 2, Amex, []int{15}},
	{300, 305, 3, Diners, []int{14, 16, 17, 18, 19}},
	{36, 36, 2, Diners, []int{14, 15, 16, 17, 18, 19}},
	{38, 39, 2, Diners, []int{14, 16, 17, 18, 19}},
	{3528, 3589, 4, JCB, []int{16, 17, 18, 19}},
	{6011, 6011, 4, Discover, []int{16, 17, 18, 19}},
	{644, 649, 3, Discover, []int{16, 17, 18, 19}},
	{65, 65, 2, Discover, []int{16, 17, 18, 19}},
	{62, 62, 2, UnionPay, []int{16, 17, 18, 19}},
	{2221, 2720, 4, Mastercard, []int{16}},
	{51, 55, 2, Mastercard, []int{16}},
	{6304, 6304, 4, Maestro, []int{12, 13, 14, 15, 16, 17, 18, 19}},
	{6759, 6759, 4, Maestro, []int{12, 13, 14, 15, 16, 17, 18, 19}},
	{6761, 6763, 4, Maestro, []int{12, 13, 14, 15, 16, 17, 18, 19}},
	{50, 50, 2, Maestro, []int{12, 13, 14, 15, 16, 17, 18, 19}},
	{56, 58, 2, Maestro, []int{12, 13, 14, 15, 16, 17, 18, 19}},
	{4, 4, 1, Visa, []int{13, 16, 19}},
}

// Normalize strips spaces and dashes from a card number.
func Normalize(pan string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, pan)
}

// IsDigits reports whether s is non-empty and only ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// LuhnValid reports whether a digit string passes the Luhn (mod 10) check.
func LuhnValid(pan string) bool {
	if len(pan) < 2 || !IsDigits(pan) {
		return false
	}
	return luhnSum(pan, false)%10 == 0
}

// LuhnCheckDigit computes the digit that makes partial+digit Luhn-valid.
func LuhnCheckDigit(partial string) byte {
	s := luhnSum(partial, true)
	return byte('0' + (10-s%10)%10)
}

// luhnSum sums digits right-to-left, doubling every second digit. When
// forCheckDigit is true the rightmost digit is doubled (it will move one
// position left once the check digit is appended).
func luhnSum(digits string, forCheckDigit bool) int {
	sum := 0
	double := forCheckDigit
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum
}

// DetectBrand returns the brand for a PAN based on its BIN prefix and length.
func DetectBrand(pan string) Brand {
	for _, r := range binRanges {
		if len(pan) < r.digits {
			continue
		}
		p := atoi(pan[:r.digits])
		if p < r.lo || p > r.hi {
			continue
		}
		for _, l := range r.lengths {
			if l == len(pan) {
				return r.brand
			}
		}
		return Unknown
	}
	return Unknown
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// CVCLength returns the expected security-code length for a brand.
func CVCLength(b Brand) int {
	if b == Amex {
		return 4
	}
	return 3
}

// BIN returns the 6-digit bank identification number of a PAN.
func BIN(pan string) string {
	if len(pan) < 6 {
		return pan
	}
	return pan[:6]
}

// Last4 returns the last four digits of a PAN.
func Last4(pan string) string {
	if len(pan) < 4 {
		return pan
	}
	return pan[len(pan)-4:]
}

// Mask returns the PCI-permitted display form: first six and last four
// digits with everything between replaced by '*'.
func Mask(pan string) string {
	if len(pan) < 13 {
		return strings.Repeat("*", len(pan))
	}
	return pan[:6] + strings.Repeat("*", len(pan)-10) + pan[len(pan)-4:]
}

// Input is raw card data as received from a client.
type Input struct {
	Number     string `json:"number"`
	ExpMonth   int    `json:"exp_month"`
	ExpYear    int    `json:"exp_year"`
	CVC        string `json:"cvc"`
	HolderName string `json:"holder_name,omitempty"`
}

// Validated is card data that passed validation. PAN is normalized.
type Validated struct {
	PAN        string
	Brand      Brand
	ExpMonth   int
	ExpYear    int
	CVC        string
	HolderName string
}

// Validate checks a card input at time now and returns normalized data.
// Error messages never echo the card number.
func Validate(in Input, now time.Time) (Validated, error) {
	var v apperr.Validator
	pan := Normalize(in.Number)
	brand := Unknown
	if v.Check(IsDigits(pan) && len(pan) >= 12 && len(pan) <= 19, "card.number", "must be 12-19 digits") {
		if v.Check(LuhnValid(pan), "card.number", "failed the Luhn check") {
			brand = DetectBrand(pan)
			v.Check(brand != Unknown, "card.number", "card brand is not supported")
		}
	}
	v.Range("card.exp_month", int64(in.ExpMonth), 1, 12)
	v.Range("card.exp_year", int64(in.ExpYear), 2000, 2099)
	if in.ExpMonth >= 1 && in.ExpMonth <= 12 && in.ExpYear >= 2000 && in.ExpYear <= 2099 {
		v.Check(!Expired(in.ExpMonth, in.ExpYear, now), "card.exp_year", "card is expired")
	}
	if v.Check(IsDigits(in.CVC), "card.cvc", "must be digits") && brand != Unknown {
		v.Check(len(in.CVC) == CVCLength(brand), "card.cvc", "must be %d digits", CVCLength(brand))
	}
	v.MaxLen("card.holder_name", in.HolderName, 100)
	if err := v.Err(); err != nil {
		return Validated{}, err
	}
	return Validated{
		PAN: pan, Brand: brand, ExpMonth: in.ExpMonth, ExpYear: in.ExpYear,
		CVC: in.CVC, HolderName: strings.TrimSpace(in.HolderName),
	}, nil
}

// Expired reports whether a card expiring at month/year is expired at now.
// Cards are valid through the last day of their expiry month.
func Expired(month, year int, now time.Time) bool {
	firstOfNext := time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC)
	return !now.Before(firstOfNext)
}

var panLike = regexp.MustCompile(`\d(?:[ -]?\d){11,18}`)

// RedactPANs replaces every Luhn-valid 12-19 digit sequence (optionally
// separated by spaces or dashes) in s with its masked form. It is used as a
// defense-in-depth filter on log output.
func RedactPANs(s string) string {
	if !strings.ContainsAny(s, "0123456789") {
		return s
	}
	return panLike.ReplaceAllStringFunc(s, func(m string) string {
		digits := Normalize(m)
		if !LuhnValid(digits) {
			return m
		}
		return Mask(digits)
	})
}
