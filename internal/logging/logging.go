// Package logging configures structured JSON logging with a PAN-redaction
// filter applied to every string attribute and message.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
)

// New returns a JSON (or text) logger at the given level whose output is
// scrubbed of anything that looks like a card number.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("logging: invalid level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl, ReplaceAttr: redactAttr}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: invalid format %q (json|text)", format)
	}
	return slog.New(&redactingHandler{next: h}), nil
}

// Discard returns a logger that drops everything (tests).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// redactAttr masks PAN-like sequences in string attribute values and
// normalises the record timestamp to UTC.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindTime:
		if a.Key == slog.TimeKey && len(groups) == 0 {
			return slog.Time(a.Key, a.Value.Time().UTC())
		}
	case slog.KindString:
		if s := a.Value.String(); card.RedactPANs(s) != s {
			return slog.String(a.Key, card.RedactPANs(s))
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, card.RedactPANs(err.Error()))
		}
	}
	return a
}

// redactingHandler also scrubs the log message itself, which ReplaceAttr
// does not cover.
type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	if msg := card.RedactPANs(r.Message); msg != r.Message {
		nr := slog.NewRecord(r.Time, r.Level, msg, r.PC)
		r.Attrs(func(a slog.Attr) bool { nr.AddAttrs(a); return true })
		r = nr
	}
	return h.next.Handle(ctx, r)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactingHandler{next: h.next.WithAttrs(attrs)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}
