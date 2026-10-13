package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header names used on webhook requests.
const (
	HeaderSignature  = "Gateway-Signature"
	HeaderEventID    = "Gateway-Event-Id"
	HeaderEventType  = "Gateway-Event-Type"
	HeaderDeliveryID = "Gateway-Delivery-Id"
	HeaderAttempt    = "Gateway-Delivery-Attempt"
)

// Verification errors.
var (
	ErrSignatureMissing   = errors.New("webhook: signature header missing or malformed")
	ErrSignatureMismatch  = errors.New("webhook: no matching signature")
	ErrSignatureTimestamp = errors.New("webhook: timestamp outside tolerance")
)

// Sign computes the v1 signature: hex(HMAC-SHA256(secret, "<unix>.<body>")).
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader builds "t=<unix>,v1=<sig>[,v1=<sig>...]". Multiple secrets
// produce multiple v1 entries so receivers keep working while a secret is
// being rotated.
func SignatureHeader(timestamp int64, body []byte, secrets ...string) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(timestamp, 10))
	for _, s := range secrets {
		b.WriteString(",v1=")
		b.WriteString(Sign(s, timestamp, body))
	}
	return b.String()
}

// Verify checks a signature header against body using secret. The timestamp
// must be within tolerance of now to defeat replays of captured requests.
func Verify(header string, body []byte, secret string, tolerance time.Duration, now time.Time) error {
	var ts int64 = -1
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return ErrSignatureMissing
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts < 0 || len(sigs) == 0 {
		return ErrSignatureMissing
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(ts, 0))
		if age > tolerance || age < -tolerance {
			return ErrSignatureTimestamp
		}
	}
	want := []byte(Sign(secret, ts, body))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
