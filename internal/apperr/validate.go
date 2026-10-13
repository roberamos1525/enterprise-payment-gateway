package apperr

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Validator accumulates field errors so a request can report every problem
// at once instead of failing on the first one.
type Validator struct {
	fields []FieldError
}

// Add records a field error unconditionally.
func (v *Validator) Add(field, format string, args ...any) {
	v.fields = append(v.fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Check records a field error when ok is false. It returns ok for chaining.
func (v *Validator) Check(ok bool, field, format string, args ...any) bool {
	if !ok {
		v.Add(field, format, args...)
	}
	return ok
}

// Required checks that a string field is non-blank.
func (v *Validator) Required(field, value string) bool {
	return v.Check(strings.TrimSpace(value) != "", field, "is required")
}

// MaxLen checks a string's length in runes.
func (v *Validator) MaxLen(field, value string, n int) bool {
	return v.Check(utf8.RuneCountInString(value) <= n, field, "must be at most %d characters", n)
}

// OneOf checks that value is one of the allowed options.
func (v *Validator) OneOf(field, value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	v.Add(field, "must be one of %s", strings.Join(allowed, ", "))
	return false
}

// Range checks an integer bound (inclusive).
func (v *Validator) Range(field string, value, lo, hi int64) bool {
	return v.Check(value >= lo && value <= hi, field, "must be between %d and %d", lo, hi)
}

// Email checks a bare email address (no display name).
func (v *Validator) Email(field, value string) bool {
	addr, err := mail.ParseAddress(value)
	return v.Check(err == nil && addr.Address == value, field, "must be a valid email address")
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

// Country checks an ISO 3166-1 alpha-2 code (format only).
func (v *Validator) Country(field, value string) bool {
	return v.Check(countryRe.MatchString(value), field, "must be an ISO 3166-1 alpha-2 country code")
}

// Metadata validates free-form string metadata: bounded key count and sizes.
func (v *Validator) Metadata(field string, md map[string]string) bool {
	ok := v.Check(len(md) <= 20, field, "must have at most 20 keys")
	for k, val := range md {
		ok = v.Check(k != "" && len(k) <= 40, field+"."+k, "key must be 1-40 characters") && ok
		ok = v.Check(len(val) <= 500, field+"."+k, "value must be at most 500 characters") && ok
	}
	return ok
}

// Merge appends the errors of another validator.
func (v *Validator) Merge(other *Validator) {
	v.fields = append(v.fields, other.fields...)
}

// Valid reports whether no errors were recorded.
func (v *Validator) Valid() bool { return len(v.fields) == 0 }

// Err returns a validation error, or nil when everything checked out.
func (v *Validator) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &Error{Code: CodeValidation, Message: "request validation failed", Fields: v.fields}
}
