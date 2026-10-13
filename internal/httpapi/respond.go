package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// errorBody is the consistent error envelope returned by every endpoint.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Type        string              `json:"type"`
	Message     string              `json:"message"`
	Fields      []apperr.FieldError `json:"fields,omitempty"`
	DeclineCode string              `json:"decline_code,omitempty"`
	RequestID   string              `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError renders err as the error envelope. Internal errors are logged
// with their cause and returned with a generic message.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	e := apperr.From(err)
	status := e.HTTPStatus()
	if status >= 500 {
		s.log.Error("request failed", "request_id", reqctx.RequestID(r.Context()), "method", r.Method,
			"path", r.URL.Path, "error", err)
	} else {
		s.log.Debug("request rejected", "request_id", reqctx.RequestID(r.Context()), "code", e.Code, "error", err)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{
		Type: string(e.Code), Message: e.Message, Fields: e.Fields, DeclineCode: e.DeclineCode,
		RequestID: reqctx.RequestID(r.Context()),
	}})
}

// decode reads a strict JSON body into dst: correct content type, bounded
// size (enforced by the body-limit middleware), no unknown fields and no
// trailing data. An empty body decodes as {} when allowEmpty is set.
func decode(r *http.Request, dst any, allowEmpty bool) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return bodyReadError(err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if allowEmpty {
			return nil
		}
		return apperr.New(apperr.CodeInvalidRequest, "request body is required")
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return apperr.New(apperr.CodeUnsupportedMediaType, "Content-Type must be application/json")
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return jsonError(err)
	}
	if dec.More() {
		return apperr.New(apperr.CodeInvalidRequest, "request body must contain a single JSON object")
	}
	return nil
}

// bodyReadError maps a failed body read to a client error.
func bodyReadError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return apperr.New(apperr.CodePayloadTooLarge, "request body exceeds %d bytes", mbe.Limit)
	}
	return apperr.New(apperr.CodeInvalidRequest, "could not read request body")
}

func jsonError(err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return apperr.New(apperr.CodeInvalidRequest, "malformed JSON at offset %d", syn.Offset)
	case errors.As(err, &typ):
		var v apperr.Validator
		v.Add(typ.Field, "must be of type %s", typ.Type.String())
		return v.Err()
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return apperr.New(apperr.CodeInvalidRequest, "%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return apperr.New(apperr.CodeInvalidRequest, "invalid JSON body")
}

// pageFrom parses limit/starting_after query parameters.
func pageFrom(r *http.Request) (store.Page, error) {
	q := r.URL.Query()
	p := store.Page{Limit: 20, StartingAfter: q.Get("starting_after")}
	var v apperr.Validator
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if v.Check(err == nil && n >= 1 && n <= 100, "limit", "must be an integer between 1 and 100") {
			p.Limit = n
		}
	}
	v.MaxLen("starting_after", p.StartingAfter, 64)
	return p, v.Err()
}

// dateParam parses a required YYYY-MM-DD query or body value.
func dateParam(field, s string) (time.Time, error) {
	var v apperr.Validator
	if !v.Required(field, s) {
		return time.Time{}, v.Err()
	}
	t, err := clock.ParseDate(s)
	if err != nil {
		v.Add(field, "must be a date in YYYY-MM-DD format")
		return time.Time{}, v.Err()
	}
	return t, nil
}

// timeParam parses an optional RFC 3339 query value.
func timeParam(r *http.Request, field string, v *apperr.Validator) *time.Time {
	s := r.URL.Query().Get(field)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if !v.Check(err == nil, field, "must be an RFC 3339 timestamp") {
		return nil
	}
	return &t
}

// logAttrs is a helper for structured access logging.
func logAttrs(r *http.Request, status int, d time.Duration) []any {
	return []any{
		slog.String("request_id", reqctx.RequestID(r.Context())),
		slog.String("method", r.Method),
		slog.String("route", routeOf(r)),
		slog.Int("status", status),
		slog.Duration("duration", d),
		slog.String("client_ip", reqctx.ClientIP(r.Context())),
	}
}

func routeOf(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}
