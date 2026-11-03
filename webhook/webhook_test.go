package webhook

import (
	"testing"
	"time"
)

func TestVerifyRejectsTamperingWrongKeyAndReplays(t *testing.T) {
	now := time.Unix(1762190560, 0)
	body := []byte(`{"event":"oomkill"}`)
	sig := Sign("s3cret", now, body)
	ts := "1762190560"
	if err := Verify("s3cret", ts, sig, body, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("genuine delivery rejected: %v", err)
	}
	for name, err := range map[string]error{
		"tampered body":     Verify("s3cret", ts, sig, []byte(`{"event":"crash"}`), now, 5*time.Minute),
		"wrong secret":      Verify("other", ts, sig, body, now, 5*time.Minute),
		"replayed":          Verify("s3cret", ts, sig, body, now.Add(time.Hour), 5*time.Minute),
		"timestamp swapped": Verify("s3cret", "1762190561", sig, body, now, 5*time.Minute),
		"no signature":      Verify("s3cret", ts, "", body, now, 5*time.Minute),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
