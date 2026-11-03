// Package webhook signs and verifies lastwords webhook deliveries.
//
// Every delivery carries two headers when a secret is configured:
//
//	X-Lastwords-Timestamp: 1762190560
//	X-Lastwords-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
//
// Receivers should recompute the HMAC over the raw body, compare in constant
// time, and reject timestamps too far from now so a captured delivery cannot
// be replayed later.
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

const (
	TimestampHeader = "X-Lastwords-Timestamp"
	SignatureHeader = "X-Lastwords-Signature"
)

// Sign returns the signature header value for body sent at ts.
func Sign(secret string, ts time.Time, body []byte) string {
	return "sha256=" + hex.EncodeToString(mac(secret, strconv.FormatInt(ts.Unix(), 10), body))
}

// Verify checks a delivery's headers against its raw body. tolerance bounds
// how old (or how far in the future) the timestamp may be.
func Verify(secret, timestamp, signature string, body []byte, now time.Time, tolerance time.Duration) error {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("missing or malformed timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside tolerance")
	}
	got, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return errors.New("missing or malformed signature")
	}
	raw, err := hex.DecodeString(got)
	if err != nil || !hmac.Equal(raw, mac(secret, timestamp, body)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func mac(secret, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}
