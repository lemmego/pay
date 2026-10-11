package stripe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lemmego/pay"
	"github.com/lemmego/pay/stripe"
)

// subscriptionsServer serves a paged subscription listing, recording the
// Stripe-Account header and the cursor it was asked for.
func subscriptionsServer(t *testing.T, total int) (*stripe.Revenue, *[]*http.Request) {
	t.Helper()
	var requests []*http.Request

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)

		if strings.HasPrefix(r.URL.Path, "/v1/accounts/") {
			_, _ = w.Write([]byte(`{"id":"acct_1"}`))
			return
		}

		// Page from the cursor, 100 at a time, the way Stripe does.
		start := 0
		if after := r.URL.Query().Get("starting_after"); after != "" {
			_, _ = fmt.Sscanf(after, "sub_%d", &start)
			start++
		}
		end := start + 100
		if end > total {
			end = total
		}

		data := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			data = append(data, map[string]any{
				"id": fmt.Sprintf("sub_%d", i), "customer": "cus_1", "status": "active",
				"items": map[string]any{"data": []map[string]any{{
					"quantity": 1,
					"price": map[string]any{"id": "price_1", "unit_amount": 1000, "currency": "usd",
						"recurring": map[string]any{"interval": "month", "interval_count": 1}},
				}}},
			})
		}
		body, _ := json.Marshal(map[string]any{"data": data, "has_more": end < total})
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	gateway := stripe.New(stripe.Config{SecretKey: "sk_test_abc", BaseURL: server.URL})
	return stripe.NewRevenue(gateway, "ca_test", "sk_test_abc"), &requests
}

func TestSubscriptionsPagesAndCarriesTheAccount(t *testing.T) {
	revenue, requests := subscriptionsServer(t, 250)

	total, err := pay.Total(context.Background(), revenue, "acct_1", nowish())
	if err != nil {
		t.Fatal(err)
	}
	// 250 subscriptions at $10/mo.
	if got := total.In("usd").Amount; got != 250000 {
		t.Errorf("total = %d, want 250000", got)
	}
	if total.Counted != 250 {
		t.Errorf("counted %d, want 250", total.Counted)
	}
	if len(*requests) != 3 {
		t.Fatalf("made %d requests, want 3 pages", len(*requests))
	}

	for i, request := range *requests {
		// This header is the only difference between reading our own books
		// and reading somebody else's. A page that forgot it would silently
		// sum the platform's own subscriptions.
		if got := request.Header.Get("Stripe-Account"); got != "acct_1" {
			t.Errorf("page %d: Stripe-Account = %q", i, got)
		}
		// status=all, so Revenue can report what it skipped rather than
		// having the gateway filter invisibly.
		if got := request.URL.Query().Get("status"); got != "all" {
			t.Errorf("page %d: status = %q, want all", i, got)
		}
	}
	if got := (*requests)[1].URL.Query().Get("starting_after"); got != "sub_99" {
		t.Errorf("the second page started after %q, want sub_99", got)
	}
}

// TestAnEndlessListingStopsItself guards the loop that has no other exit: a
// has_more that never goes false is an infinite loop holding a worker and
// hammering somebody else's API quota.
func TestAnEndlessListingStopsItself(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]any, 0, 100)
		for i := 0; i < 100; i++ {
			data = append(data, map[string]any{
				"id": fmt.Sprintf("sub_%d", i), "customer": "cus_1", "status": "canceled",
				"items": map[string]any{"data": []map[string]any{}},
			})
		}
		body, _ := json.Marshal(map[string]any{"data": data, "has_more": true})
		_, _ = w.Write(body)
	}))
	defer server.Close()

	gateway := stripe.New(stripe.Config{SecretKey: "sk_test_abc", BaseURL: server.URL})
	revenue := stripe.NewRevenue(gateway, "ca_test", "sk_test_abc")

	err := revenue.Subscriptions(context.Background(), "acct_1", func(*pay.Subscription) error { return nil })
	if err == nil {
		t.Fatal("an endless listing never returned")
	}
	if !strings.Contains(err.Error(), "cap") && err != pay.ErrTooManyRecords {
		t.Errorf("err = %v, want ErrTooManyRecords", err)
	}
}

func TestExchangeRefusesWriteAccess(t *testing.T) {
	cases := map[string]struct {
		body    string
		wantErr string
	}{
		"read_only is accepted": {
			body: `{"stripe_user_id":"acct_1","scope":"read_only"}`,
		},
		"read_write is refused": {
			// An authorisation granted before the scope was narrowed, or a
			// flow started elsewhere, can come back read_write. Storing it as
			// though it were read-only would mean quietly holding write
			// access to somebody's payments.
			body:    `{"stripe_user_id":"acct_1","scope":"read_write"}`,
			wantErr: "read_only",
		},
		"an error is reported": {
			body:    `{"error":"invalid_grant","error_description":"code already used"}`,
			wantErr: "invalid_grant",
		},
		"no account is refused": {
			body:    `{"scope":"read_only"}`,
			wantErr: "names no account",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					t.Errorf("the exchange went to %q", r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			gateway := stripe.New(stripe.Config{SecretKey: "sk_test_abc", BaseURL: server.URL})
			revenue := stripe.NewRevenue(gateway, "ca_test", "sk_test_abc")

			account, err := revenue.Exchange(context.Background(), "ac_code")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if account != "acct_1" {
					t.Errorf("account = %q", account)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, returning %q", account)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestAuthorizeURLAsksForReadOnlyAndNeedsAState(t *testing.T) {
	gateway := stripe.New(stripe.Config{SecretKey: "sk_test_abc"})
	revenue := stripe.NewRevenue(gateway, "ca_test", "sk_test_abc")

	// A state this package invented would be bound to nothing, which is a
	// CSRF hole wearing the name of its own defence.
	if _, err := revenue.AuthorizeURL("", "https://productshore.test/cb"); err == nil {
		t.Fatal("a flow with no state was allowed to start")
	}

	url, err := revenue.AuthorizeURL("st_123", "https://productshore.test/cb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "scope=read_only") {
		t.Errorf("url = %q, want read_only", url)
	}
	if strings.Contains(url, "read_write") {
		t.Errorf("url asks for write access: %q", url)
	}
	if !strings.Contains(url, "state=st_123") {
		t.Errorf("url = %q, want the state", url)
	}
}

func TestAccountReportsRevokedAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"account_invalid",
		  "message":"The provided key does not have access to account acct_1."}}`))
	}))
	defer server.Close()

	gateway := stripe.New(stripe.Config{SecretKey: "sk_test_abc", BaseURL: server.URL})
	revenue := stripe.NewRevenue(gateway, "ca_test", "sk_test_abc")

	// Access withdrawn in the founder's own dashboard. The badge has to come
	// down rather than keep showing a figure nobody can check.
	if err := revenue.Account(context.Background(), "acct_1"); err == nil {
		t.Fatal("a revoked account reported healthy")
	}
}

func nowish() time.Time { return time.Unix(1700000000, 0).UTC() }

// TestRevenueSatisfiesTheInterfaces is a compile-time check that matters more
// than it looks: the application resolves the OAuth half by asserting
// pay.Authorizer, so a signature drifting here would turn the connect flow
// into a 503 at runtime rather than a build failure.
func TestRevenueSatisfiesTheInterfaces(t *testing.T) {
	var _ pay.RevenueReader = (*stripe.Revenue)(nil)
	var _ pay.Authorizer = (*stripe.Revenue)(nil)
	var _ pay.Gateway = (*stripe.Gateway)(nil)
}
