package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The App Store Server API's base URLs.
const (
	ProductionURL = "https://api.storekit.itunes.apple.com"
	SandboxURL    = "https://api.storekit-sandbox.itunes.apple.com"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 1 << 20
	// tokenLifetime is well under the 60 minutes Apple allows.
	tokenLifetime = 5 * time.Minute
)

// errNotFound reports that the App Store has no such transaction in the
// environment asked.
var errNotFound = errors.New("app store: transaction not found")

// ParsePrivateKey parses an App Store Connect In-App Purchase key: the .p8
// file, a PKCS #8 PEM block holding a P-256 key.
func ParsePrivateKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("not a PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve.Params().Name != "P-256" {
		return nil, errors.New("not a P-256 key")
	}
	return ec, nil
}

// apiClient calls the App Store Server API.
type apiClient struct {
	http     *http.Client
	issuerID string
	keyID    string
	bundleID string
	key      *ecdsa.PrivateKey
}

// token is the bearer token Apple requires on every call
// (https://developer.apple.com/documentation/appstoreserverapi/generating-json-web-tokens-for-api-requests).
func (c *apiClient) token() (string, error) {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.issuerID,
		"iat": now.Unix(),
		"exp": now.Add(tokenLifetime).Unix(),
		"aud": "appstoreconnect-v1",
		"bid": c.bundleID,
	})
	t.Header["kid"] = c.keyID
	return t.SignedString(c.key)
}

// statusResponse is Get All Subscription Statuses' StatusResponse.
type statusResponse struct {
	Environment string `json:"environment"`
	BundleID    string `json:"bundleId"`
	AppAppleID  int64  `json:"appAppleId"`
	Data        []struct {
		LastTransactions []lastTransaction `json:"lastTransactions"`
	} `json:"data"`
}

type lastTransaction struct {
	OriginalTransactionID string `json:"originalTransactionId"`
	Status                int    `json:"status"`
	SignedTransactionInfo string `json:"signedTransactionInfo"`
	SignedRenewalInfo     string `json:"signedRenewalInfo"`
}

// apiError is an error response from the App Store Server API.
type apiError struct {
	StatusCode int
	Code       int64  `json:"errorCode"`
	Message    string `json:"errorMessage"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("app store server api: %d %d %s", e.StatusCode, e.Code, e.Message)
}

// subscriptionStatuses calls Get All Subscription Statuses
// (GET /inApps/v1/subscriptions/{transactionId}) at base. It returns
// errNotFound when that environment does not know the transaction.
func (c *apiClient) subscriptionStatuses(ctx context.Context, base, transactionID string) (statusResponse, error) {
	token, err := c.token()
	if err != nil {
		return statusResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/inApps/v1/subscriptions/"+url.PathEscape(transactionID), http.NoBody)
	if err != nil {
		return statusResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return statusResponse{}, fmt.Errorf("app store server api: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return statusResponse{}, fmt.Errorf("app store server api: reading response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		e := &apiError{StatusCode: res.StatusCode}
		_ = json.Unmarshal(data, e)
		// OriginalTransactionIdNotFoundError and TransactionIdNotFoundError:
		// the transaction belongs to the other environment, or to no one.
		if res.StatusCode == http.StatusNotFound && (e.Code == 4040005 || e.Code == 4040010) {
			return statusResponse{}, fmt.Errorf("%w: %w", errNotFound, e)
		}
		return statusResponse{}, e
	}
	var out statusResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return statusResponse{}, fmt.Errorf("app store server api: %w", err)
	}
	return out, nil
}
