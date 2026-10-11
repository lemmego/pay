package pay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/lemmego/pay"
)

func sign(secret string, payload []byte, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(payload)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1700000000, 0)
	payload := []byte(`{"id":"evt_1","type":"customer.subscription.updated"}`)
	header := sign("whsec_live", payload, now)

	if err := pay.VerifySignature(payload, header, []string{"whsec_live"}, now, 0); err != nil {
		t.Fatalf("a good delivery did not verify: %v", err)
	}

	t.Run("the wrong secret", func(t *testing.T) {
		err := pay.VerifySignature(payload, header, []string{"whsec_other"}, now, 0)
		if !errors.Is(err, pay.ErrBadSignature) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a tampered payload", func(t *testing.T) {
		// One byte changed: the amount. This is the attack the signature
		// exists for, so it is the one worth writing down.
		tampered := []byte(`{"id":"evt_1","type":"customer.subscription.created"}`)
		err := pay.VerifySignature(tampered, header, []string{"whsec_live"}, now, 0)
		if !errors.Is(err, pay.ErrBadSignature) {
			t.Fatalf("a tampered payload verified: %v", err)
		}
	})

	t.Run("a replay from last week", func(t *testing.T) {
		// The signature is still cryptographically valid. Only the tolerance
		// refuses it, which is why the tolerance is not optional.
		later := now.Add(7 * 24 * time.Hour)
		err := pay.VerifySignature(payload, header, []string{"whsec_live"}, later, 0)
		if !errors.Is(err, pay.ErrBadSignature) {
			t.Fatalf("a week-old delivery verified: %v", err)
		}
	})

	t.Run("a timestamp in the future", func(t *testing.T) {
		// Refused in both directions. A delivery signed an hour from now is
		// either a clock problem or somebody extending a captured
		// signature's life, and neither should be applied.
		earlier := now.Add(-time.Hour)
		err := pay.VerifySignature(payload, header, []string{"whsec_live"}, earlier, 0)
		if !errors.Is(err, pay.ErrBadSignature) {
			t.Fatalf("a future delivery verified: %v", err)
		}
	})

	t.Run("within tolerance", func(t *testing.T) {
		if err := pay.VerifySignature(payload, header, []string{"whsec_live"},
			now.Add(4*time.Minute), 0); err != nil {
			t.Fatalf("a four-minute-old delivery was refused: %v", err)
		}
	})

	t.Run("a rotation", func(t *testing.T) {
		// Signed with the new secret, verified against a list holding both:
		// the interval during which two endpoint secrets are live must not be
		// an interval during which events are dropped.
		rotated := sign("whsec_new", payload, now)
		if err := pay.VerifySignature(payload, rotated,
			[]string{"whsec_live", "whsec_new"}, now, 0); err != nil {
			t.Fatalf("the new secret was refused during rotation: %v", err)
		}
	})

	t.Run("several v1 values", func(t *testing.T) {
		// Stripe sends one per endpoint secret during its own rotation.
		both := header + ",v1=" + hex.EncodeToString([]byte("not a signature"))
		if err := pay.VerifySignature(payload, both, []string{"whsec_live"}, now, 0); err != nil {
			t.Fatalf("a header with an extra v1 was refused: %v", err)
		}
	})

	t.Run("an unknown scheme is ignored", func(t *testing.T) {
		// v0 appears in Stripe CLI deliveries and more schemes will follow. A
		// verifier that rejects an unknown pair breaks on their schedule.
		withV0 := "v0=deadbeef," + header
		if err := pay.VerifySignature(payload, withV0, []string{"whsec_live"}, now, 0); err != nil {
			t.Fatalf("an unknown scheme broke verification: %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for name, header := range map[string]string{
			"no header":      "",
			"no timestamp":   "v1=" + hex.EncodeToString([]byte("x")),
			"no signature":   "t=1700000000",
			"a word for a t": "t=yesterday,v1=abcd",
			"not hex":        "t=1700000000,v1=zzzz",
			"nonsense":       "garbage",
		} {
			if err := pay.VerifySignature(payload, header, []string{"whsec_live"}, now, 0); !errors.Is(err, pay.ErrBadSignature) {
				t.Errorf("%s: got %v, want ErrBadSignature", name, err)
			}
		}

		// No secret configured refuses everything, rather than accepting
		// everything — which is the failure mode worth being certain about.
		if err := pay.VerifySignature(payload, header, nil, now, 0); !errors.Is(err, pay.ErrBadSignature) {
			t.Errorf("with no secret: got %v, want ErrBadSignature", err)
		}
		if err := pay.VerifySignature(payload, header, []string{""}, now, 0); !errors.Is(err, pay.ErrBadSignature) {
			t.Errorf("with an empty secret: got %v, want ErrBadSignature", err)
		}
	})
}
