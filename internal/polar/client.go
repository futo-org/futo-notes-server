package polar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// requestTimeout caps one call to Polar. These calls happen on the checkout
// and portal routes, which a person is waiting on; nothing on the sync path
// ever reaches Polar, so a slow answer here delays a sign-up and nothing else.
const requestTimeout = 10 * time.Second

// errorBodyLimit is how much of a refusal is quoted back in the error. Polar
// answers a validation failure with a list of field errors, and the first few
// hundred bytes are what an operator needs to see in the log.
const errorBodyLimit = 512

// httpClient is the transport every Polar call shares, so connections are
// reused across checkouts rather than dialed per request.
var httpClient = &http.Client{Timeout: requestTimeout}

// Client calls Polar's HTTP API with an organization access token. It covers
// the three calls sign-up needs — create a customer, open a checkout, and
// open a customer portal session — over the standard library, with no Polar
// SDK.
//
// BaseURL selects the environment: https://sandbox-api.polar.sh for dev and
// CI, https://api.polar.sh in production.
type Client struct {
	BaseURL     string
	AccessToken string
}

// CheckoutRequest is one hosted checkout: which product, at what discount,
// for which customer, and where to send the browser afterwards. DiscountID
// is optional and omitted from the call when empty, which is the launch flip
// away from the internal MVP's 100% discount.
type CheckoutRequest struct {
	ProductID  string
	DiscountID string
	CustomerID string
	SuccessURL string
}

// CreateCustomer registers the account with Polar and returns Polar's id for
// it. externalID is how Polar will name the account back to this server, in
// the webhook's external customer id, so it is the account's OIDC sub.
func (c *Client) CreateCustomer(ctx context.Context, externalID, email string) (string, error) {
	body := map[string]any{"external_id": externalID, "email": email}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/v1/customers/", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// CreateCheckout opens a checkout session and returns the URL to open a
// browser at.
func (c *Client) CreateCheckout(ctx context.Context, request CheckoutRequest) (string, error) {
	body := map[string]any{
		"products":    []string{request.ProductID},
		"customer_id": request.CustomerID,
		"success_url": request.SuccessURL,
	}
	if request.DiscountID != "" {
		body["discount_id"] = request.DiscountID
	}
	var created struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/checkouts/", body, &created); err != nil {
		return "", err
	}
	return created.URL, nil
}

// CreateCustomerSession opens a pre-authenticated customer portal session and
// returns the URL to open a browser at. The portal is where a person cancels
// or changes a card, so the server implements none of that itself.
func (c *Client) CreateCustomerSession(ctx context.Context, customerID string) (string, error) {
	body := map[string]any{"customer_id": customerID}
	var created struct {
		CustomerPortalURL string `json:"customer_portal_url"`
	}
	if err := c.post(ctx, "/v1/customer-sessions/", body, &created); err != nil {
		return "", err
	}
	return created.CustomerPortalURL, nil
}

// post sends one JSON call and decodes a successful answer into result. A
// non-2xx is an error carrying the status and what Polar said, because the
// useful part of a Polar refusal is in the body rather than the code.
func (c *Client) post(ctx context.Context, path string, body, result any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the %s request: %w", path, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.AccessToken)
	request.Header.Set("Content-Type", "application/json")

	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("calling Polar %s: %w", path, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		refusal, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
		return fmt.Errorf("Polar %s answered %d: %s", path, response.StatusCode, refusal)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("decoding the Polar %s answer: %w", path, err)
	}
	return nil
}
