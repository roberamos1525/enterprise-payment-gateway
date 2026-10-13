// Package apperr defines the typed errors used across the gateway and maps
// them onto HTTP status codes and the public error envelope.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier exposed in API responses.
type Code string

// Error codes. They are part of the public API contract (see API.md).
const (
	CodeInvalidRequest        Code = "invalid_request"
	CodeValidation            Code = "validation_failed"
	CodeUnauthenticated       Code = "unauthenticated"
	CodeForbidden             Code = "forbidden"
	CodeNotFound              Code = "not_found"
	CodeConflict              Code = "conflict"
	CodeInvalidState          Code = "invalid_state_transition"
	CodeIdempotencyMismatch   Code = "idempotency_key_reused"
	CodeIdempotencyInProgress Code = "idempotency_key_in_progress"
	CodeRateLimited           Code = "rate_limited"
	CodeCardDeclined          Code = "card_declined"
	CodePayloadTooLarge       Code = "payload_too_large"
	CodeUnsupportedMediaType  Code = "unsupported_media_type"
	CodeMethodNotAllowed      Code = "method_not_allowed"
	CodeUnavailable           Code = "service_unavailable"
	CodeInternal              Code = "internal_error"
)

var statusByCode = map[Code]int{
	CodeInvalidRequest:        http.StatusBadRequest,
	CodeValidation:            http.StatusUnprocessableEntity,
	CodeUnauthenticated:       http.StatusUnauthorized,
	CodeForbidden:             http.StatusForbidden,
	CodeNotFound:              http.StatusNotFound,
	CodeConflict:              http.StatusConflict,
	CodeInvalidState:          http.StatusConflict,
	CodeIdempotencyMismatch:   http.StatusUnprocessableEntity,
	CodeIdempotencyInProgress: http.StatusConflict,
	CodeRateLimited:           http.StatusTooManyRequests,
	CodeCardDeclined:          http.StatusPaymentRequired,
	CodePayloadTooLarge:       http.StatusRequestEntityTooLarge,
	CodeUnsupportedMediaType:  http.StatusUnsupportedMediaType,
	CodeMethodNotAllowed:      http.StatusMethodNotAllowed,
	CodeUnavailable:           http.StatusServiceUnavailable,
	CodeInternal:              http.StatusInternalServerError,
}

// FieldError describes a validation problem with a single input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is the gateway's typed error. Message is safe to show to API clients;
// the wrapped Err (if any) is for logs only.
type Error struct {
	Code        Code
	Message     string
	Fields      []FieldError
	DeclineCode string
	Err         error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// HTTPStatus returns the HTTP status associated with the error code.
func (e *Error) HTTPStatus() int {
	if s, ok := statusByCode[e.Code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// New builds an error with a formatted, client-safe message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches an underlying cause that is never shown to clients.
func Wrap(err error, code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// NotFound reports a missing resource.
func NotFound(resource, id string) *Error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf("%s %q not found", resource, id)}
}

// InvalidState reports a forbidden state-machine transition.
func InvalidState(resource, from, action string) *Error {
	return &Error{
		Code:    CodeInvalidState,
		Message: fmt.Sprintf("cannot %s a %s in status %q", action, resource, from),
	}
}

// Declined reports a card decline with a network decline code.
func Declined(declineCode, message string) *Error {
	return &Error{Code: CodeCardDeclined, Message: message, DeclineCode: declineCode}
}

// Internal hides an unexpected error behind a generic message.
func Internal(err error) *Error {
	return &Error{Code: CodeInternal, Message: "internal error", Err: err}
}

// As extracts an *Error from err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CodeOf returns the code of err, or CodeInternal for untyped errors.
func CodeOf(err error) Code {
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeInternal
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool {
	return err != nil && CodeOf(err) == code
}

// From converts any error into an *Error, hiding untyped errors as internal.
func From(err error) *Error {
	if e, ok := As(err); ok {
		return e
	}
	return Internal(err)
}
