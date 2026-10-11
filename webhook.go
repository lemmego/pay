package pay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how far a delivery's timestamp may be from now.
//
// Five minutes, which is Stripe's own default. The tolerance is what stops a
// signature captured off the wire from being replayed next week; without it
// the signature proves only that the gateway signed the payload at some
// point, which is a much weaker claim than it looks.
const DefaultTolerance = 5 * time.Minute

// Event is one webhook delivery.
type Event struct {
	// ID is the gateway's event id. Unique per event and stable across
	// retries, which is what makes it the deduplication key.
	ID string `json:"id"`
	// Type is the event name, e.g. "customer.subscription.updated".
	Type string `json:"type"`
	// CreatedAt is when the gateway generated the event.
	//
	// Not when it was delivered. Stripe does not guarantee order, and a
	// retried event can arrive after a newer one, so this is the only field
	// that can order two deliveries against each other.
	CreatedAt time.Time `json:"created_at"`
	// Livemode distinguishes a real event from a test one. Worth storing: a
	// test-mode event applied to a production row is a free subscription, and
	// the only thing that distinguishes them is this flag.
	Livemode bool `json:"livemode"`
	// Subscription is the subscription the event concerns, when it concerns
	// one.
	Subscription *Subscription `json:"subscription,omitempty"`
	// CustomerID and ClientReference carry the attribution from a completed
	// checkout: the gateway's customer and this application's own id for the
	// buyer.
	CustomerID      string `json:"customer_id,omitempty"`
	ClientReference string `json:"client_reference,omitempty"`
	// Data is the raw object, for an application that needs a field this
	// module does not model. It is the gateway's JSON and nothing more.
	Data []byte `json:"-"`
}

// SignatureHeader is the header a Stripe delivery carries its signature in.
const SignatureHeader = "Stripe-Signature"

// VerifySignature checks a Stripe-style signature header against the payload.
//
// The scheme: the header is a comma-separated list of key/value pairs, one
// `t=` holding a unix timestamp and one or more `v1=` holding a hex
// HMAC-SHA256 of "<t>.<payload>" keyed by the endpoint secret. A delivery
// verifies if any v1 matches under any configured secret and the timestamp is
// within tolerance.
//
// Several secrets are accepted because rotating one otherwise means an
// interval during which either the old or the new endpoint is rejecting
// events — and a rejected event is money moving with nothing recording it.
// Several v1 values appear in one header for the same reason, from the other
// side.
//
// payload must be the bytes as received. A JSON document that has been decoded
// and re-encoded has the same meaning and a different signature, which is why
// the handler below reads the body itself and passes it through untouched.
func VerifySignature(payload []byte, header string, secrets []string, now time.Time, tolerance time.Duration) error {
	if len(secrets) == 0 {
		return fmt.Errorf("%w: no signing secret is configured", ErrBadSignature)
	}
	if header == "" {
		return fmt.Errorf("%w: no signature header", ErrBadSignature)
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}

	var timestamp string
	var provided []string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			provided = append(provided, value)
		}
		// Any other scheme is ignored rather than rejected. v0 exists for
		// Stripe's CLI and future schemes will appear; a verifier that fails
		// on an unknown pair breaks the moment one is added, and ignoring it
		// is safe because a v1 is still required below.
	}
	if timestamp == "" || len(provided) == 0 {
		return fmt.Errorf("%w: the signature header is malformed", ErrBadSignature)
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: the signature timestamp is not a number", ErrBadSignature)
	}
	signedAt := time.Unix(seconds, 0)
	if drift := now.Sub(signedAt); drift > tolerance || drift < -tolerance {
		return fmt.Errorf("%w: signed %s away from now, outside the %s tolerance",
			ErrBadSignature, drift.Round(time.Second), tolerance)
	}

	// The signed payload is the timestamp and the body joined by a dot. The
	// timestamp is inside the MAC, which is what binds the time check to the
	// signature: a replayed delivery cannot have its `t` moved forward
	// without invalidating the v1 that covers it.
	signed := make([]byte, 0, len(timestamp)+1+len(payload))
	signed = append(signed, timestamp...)
	signed = append(signed, '.')
	signed = append(signed, payload...)

	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(signed)
		expected := mac.Sum(nil)
		for _, candidate := range provided {
			decoded, err := hex.DecodeString(candidate)
			if err != nil {
				continue
			}
			// Constant time, and it has to be: a byte-at-a-time comparison
			// leaks how much of a forged signature was right, which is enough
			// to find the rest one request at a time.
			if hmac.Equal(decoded, expected) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: no signature matched", ErrBadSignature)
}

// Store is where webhook-driven state lands.
//
// The application implements it. This module owns no schema and no
// connection, for the same reason seo owns no sitemap table: the rows belong
// next to the user they describe, in whatever the application's database
// already is.
type Store interface {
	// Seen records that an event has arrived and reports whether this
	// delivery is the first.
	//
	// Stripe retries a delivery until it is acknowledged, and it retries on
	// its own schedule even after a success it did not hear about — so an
	// event will be delivered twice, and the second one must do nothing. An
	// implementation is a unique index on the event id and nothing cleverer.
	Seen(ctx Context, event *Event) (first bool, err error)

	// Apply writes the event's subscription state.
	//
	// It must be safe to call out of order. Stripe makes no ordering promise
	// and delivers concurrently, so an implementation compares the event's
	// CreatedAt against what the stored row was last written by and lets the
	// older one lose. Doing that as a condition on the write rather than a
	// read followed by a write is the difference between a guard and a race.
	Apply(ctx Context, event *Event) error
}
