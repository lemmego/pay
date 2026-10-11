package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/lemmego/pay"
)

// ConnectBase is where the OAuth token exchange happens. A different host
// from the API, which is easy to miss and fails with a 404 that says nothing.
const ConnectBase = "https://connect.stripe.com"

// pageSize is how many subscriptions one listing request asks for.
//
// Stripe's maximum. The listing exists to be summed, so fewer round trips is
// the only consideration — and an account with four thousand subscriptions is
// forty requests rather than four hundred.
const pageSize = 100

// Revenue reads subscriptions on accounts that authorised us.
//
// A separate type from Gateway, built from the same key, because
// pay.RevenueReader is a separate interface for a reason: this reads somebody
// else's business. Keeping it separate means an application can hold a
// Gateway without holding this, and the two capabilities are granted
// independently in the provider.
type Revenue struct {
	gateway *Gateway
	// ClientSecret is the Connect client secret for the OAuth exchange. The
	// platform's own secret key works for Stripe's current API, but the
	// documented credential for the token endpoint is this one, and sending
	// the right credential to the right endpoint is cheaper than finding out
	// later which one they stopped accepting.
	clientID     string
	clientSecret string
}

// NewRevenue returns the read-only revenue reader.
func NewRevenue(gateway *Gateway, clientID, clientSecret string) *Revenue {
	if clientSecret == "" {
		clientSecret = gateway.cfg.SecretKey
	}
	return &Revenue{gateway: gateway, clientID: clientID, clientSecret: clientSecret}
}

// AuthorizeURL is where a founder is sent to grant read-only access.
//
// read_only and not read_write, which is the single most important character
// of this feature. A badge needs to add up subscriptions; it never needs to
// create a charge, issue a refund, or move a payout. Asking for write access
// to display a number would be asking someone to hand over their livelihood
// for a graphic.
func (r *Revenue) AuthorizeURL(state, redirectURI string) (string, error) {
	if r.clientID == "" {
		return "", errors.New("stripe: no Connect client id is configured")
	}
	if state == "" {
		// Refused, not generated here. The state has to be bound to the
		// session that started the flow in order to do its job, and a value
		// this package invented would be bound to nothing — which is a CSRF
		// hole wearing the name of its own defence.
		return "", errors.New("stripe: AuthorizeURL needs a state bound to the session")
	}
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", r.clientID)
	query.Set("scope", "read_only")
	query.Set("state", state)
	if redirectURI != "" {
		query.Set("redirect_uri", redirectURI)
	}
	return ConnectBase + "/oauth/authorize?" + query.Encode(), nil
}

// Exchange turns an authorisation code into a connected account id.
//
// Only the account id is kept. Stripe also returns an access token, and the
// temptation is to store it; there is no need, because a platform acts on a
// connected account with its own key plus the Stripe-Account header. Not
// storing it means there is no third-party credential in this database to
// leak, and revocation works the way the founder expects — they revoke access
// in their own dashboard and our next call fails, rather than a stored token
// continuing to work until it expires.
func (r *Revenue) Exchange(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("stripe: Exchange needs an authorisation code")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ConnectBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	if base := r.gateway.cfg.BaseURL; base != "" {
		// Tests point the whole gateway at one httptest server, so the token
		// endpoint follows the override rather than reaching the internet
		// from a unit test.
		request.URL, err = url.Parse(strings.TrimSuffix(base, "/") + "/oauth/token")
		if err != nil {
			return "", err
		}
	}
	request.Header.Set("Authorization", "Bearer "+r.clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := r.gateway.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("stripe: the Connect token exchange failed: %w", err)
	}
	defer response.Body.Close()

	var out struct {
		StripeUserID string `json:"stripe_user_id"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("stripe: the Connect token response is not JSON: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("stripe: the Connect token exchange was refused: %s: %s",
			out.Error, out.Description)
	}
	if out.StripeUserID == "" {
		return "", errors.New("stripe: the Connect token response names no account")
	}
	// The scope that comes back is checked rather than assumed. A flow that
	// was started elsewhere, or an authorisation granted before the scope was
	// narrowed, can return read_write — and storing that account as though it
	// were read-only would mean this application quietly holds write access
	// to somebody's payments.
	if out.Scope != "" && out.Scope != "read_only" {
		return "", fmt.Errorf("stripe: the authorisation came back with scope %q, not read_only", out.Scope)
	}
	return out.StripeUserID, nil
}

// Account reports whether the authorisation on an account is still good.
func (r *Revenue) Account(ctx context.Context, account string) error {
	if account == "" {
		return errors.New("stripe: no account")
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := r.gateway.get(ctx, "/v1/accounts/"+url.PathEscape(account), nil, account, &out); err != nil {
		return err
	}
	if out.ID == "" {
		return errors.New("stripe: the account response names no account")
	}
	return nil
}

// Subscriptions calls each with every subscription on the account.
//
// Every one, including cancelled: status=all. Filtering server-side to the
// statuses that earn would be fewer bytes, and would also make the figure
// unexplainable — Revenue reports how many subscriptions it skipped, and it
// can only do that if it saw them. The statuses that count are decided in one
// place, pay.Status.Earning, rather than half here and half there.
func (r *Revenue) Subscriptions(ctx context.Context, account string, each func(*pay.Subscription) error) error {
	if account == "" {
		return errors.New("stripe: no account")
	}

	after := ""
	for page := 0; ; page++ {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("status", "all")
		if after != "" {
			query.Set("starting_after", after)
		}

		var out struct {
			Data    []subscription `json:"data"`
			HasMore bool           `json:"has_more"`
		}
		if err := r.gateway.get(ctx, "/v1/subscriptions", query, account, &out); err != nil {
			return err
		}
		if len(out.Data) == 0 {
			return nil
		}

		for i := range out.Data {
			if err := each(out.Data[i].convert()); err != nil {
				return err
			}
		}

		if !out.HasMore {
			return nil
		}
		after = out.Data[len(out.Data)-1].ID

		// A page count cap as well as the caller's record cap. has_more
		// staying true forever — which a bug on either side can produce — is
		// an infinite loop holding a worker and hammering somebody's API
		// quota, and the loop has no other exit.
		if page > pay.MaxAccountSubscriptions/pageSize {
			return pay.ErrTooManyRecords
		}
	}
}
