package config

import (
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"futo-notes-server/internal/hosted/billing/appstore"
	"futo-notes-server/internal/hosted/billing/play"
	"futo-notes-server/internal/hostedenv"
)

// The variables each store provider requires. A store is configured when
// all of its are set and off when none is; anything between is an error.
var (
	appleRequired = []string{
		"APPLE_BUNDLE_ID", "APPLE_APP_APPLE_ID", "APPLE_ISSUER_ID", "APPLE_KEY_ID",
		"APPLE_PRIVATE_KEY", "APPLE_SUBSCRIPTION_PRODUCT_IDS",
	}
	playRequired = []string{
		"PLAY_PACKAGE_NAME", "PLAY_SERVICE_ACCOUNT_JSON", "PLAY_SUBSCRIPTION_PRODUCT_IDS",
		"PLAY_RTDN_AUDIENCE", "PLAY_RTDN_SERVICE_ACCOUNT",
	}
)

// loadStores reads the App Store and Google Play billing providers into
// cfg.AppStore and cfg.Play, leaving either nil when it is not configured.
func loadStores(getenv Getenv, cfg *Config) error {
	on, err := storeConfigured(getenv, "App Store", appleRequired)
	if err != nil {
		return err
	}
	if on {
		if cfg.AppStore, err = loadAppStore(getenv); err != nil {
			return err
		}
	}
	if on, err = storeConfigured(getenv, "Google Play", playRequired); err != nil {
		return err
	}
	if on {
		if cfg.Play, err = loadPlay(getenv); err != nil {
			return err
		}
	}
	return nil
}

func storeConfigured(getenv Getenv, name string, required []string) (bool, error) {
	var missing []string
	for _, key := range required {
		if strings.TrimSpace(getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	switch len(missing) {
	case 0:
		return true, nil
	case len(required):
		return false, nil
	}
	return false, fmt.Errorf("%s billing is partly configured: %s also required", name, strings.Join(missing, ", "))
}

func loadAppStore(getenv Getenv) (*appstore.Config, error) {
	appID, err := strconv.ParseInt(strings.TrimSpace(getenv("APPLE_APP_APPLE_ID")), 10, 64)
	if err != nil || appID <= 0 {
		return nil, fmt.Errorf("APPLE_APP_APPLE_ID must be the app's numeric Apple ID, got %q", getenv("APPLE_APP_APPLE_ID"))
	}
	key, err := appstore.ParsePrivateKey([]byte(getenv("APPLE_PRIVATE_KEY")))
	if err != nil {
		return nil, fmt.Errorf("APPLE_PRIVATE_KEY must be the In-App Purchase key's .p8 file: %w", err)
	}
	acceptSandbox, err := hostedenv.Bool(getenv, "APPLE_ACCEPT_SANDBOX", false)
	if err != nil {
		return nil, err
	}
	products, err := productIDs(getenv, "APPLE_SUBSCRIPTION_PRODUCT_IDS")
	if err != nil {
		return nil, err
	}
	return &appstore.Config{
		BundleID:      strings.TrimSpace(getenv("APPLE_BUNDLE_ID")),
		AppAppleID:    appID,
		IssuerID:      strings.TrimSpace(getenv("APPLE_ISSUER_ID")),
		KeyID:         strings.TrimSpace(getenv("APPLE_KEY_ID")),
		PrivateKey:    key,
		ProductIDs:    products,
		AcceptSandbox: acceptSandbox,
	}, nil
}

func loadPlay(getenv Getenv) (*play.Config, error) {
	sa, err := play.ParseServiceAccount([]byte(getenv("PLAY_SERVICE_ACCOUNT_JSON")))
	if err != nil {
		return nil, fmt.Errorf("PLAY_SERVICE_ACCOUNT_JSON must be a service account key file: %w", err)
	}
	account := strings.TrimSpace(getenv("PLAY_RTDN_SERVICE_ACCOUNT"))
	if _, err := mail.ParseAddress(account); err != nil {
		return nil, fmt.Errorf("PLAY_RTDN_SERVICE_ACCOUNT must be a service account email, got %q", account)
	}
	products, err := productIDs(getenv, "PLAY_SUBSCRIPTION_PRODUCT_IDS")
	if err != nil {
		return nil, err
	}
	return &play.Config{
		PackageName:        strings.TrimSpace(getenv("PLAY_PACKAGE_NAME")),
		ServiceAccount:     sa,
		ProductIDs:         products,
		RTDNAudience:       strings.TrimSpace(getenv("PLAY_RTDN_AUDIENCE")),
		RTDNServiceAccount: account,
	}, nil
}

// productIDs reads key as a comma-separated list of at least one id.
func productIDs(getenv Getenv, key string) ([]string, error) {
	var ids []string
	for id := range strings.SplitSeq(getenv(key), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s must list at least one product id", key)
	}
	return ids, nil
}
