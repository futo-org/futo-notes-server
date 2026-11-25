package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"slices"
	"strings"
	"testing"

	"futo-notes-server/internal/hosted/config"
)

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func appleEnv(t *testing.T) map[string]string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"APPLE_BUNDLE_ID":                "tech.futo.notes",
		"APPLE_APP_APPLE_ID":             "1234567890",
		"APPLE_ISSUER_ID":                "57246542-96fe-1a63-e053-0824d011072a",
		"APPLE_KEY_ID":                   "2X9R4HXF34",
		"APPLE_PRIVATE_KEY":              pkcs8PEM(t, key),
		"APPLE_SUBSCRIPTION_PRODUCT_IDS": " notes.monthly, ,notes.yearly ",
	}
}

func playEnv(t *testing.T) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "play-api@futo-notes.iam.gserviceaccount.com",
		"private_key":  pkcs8PEM(t, key),
		"token_uri":    "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"PLAY_PACKAGE_NAME":             "tech.futo.notes",
		"PLAY_SERVICE_ACCOUNT_JSON":     string(sa),
		"PLAY_SUBSCRIPTION_PRODUCT_IDS": "notes_monthly",
		"PLAY_RTDN_AUDIENCE":            "https://notes.example.com/api/billing/webhooks/play",
		"PLAY_RTDN_SERVICE_ACCOUNT":     "rtdn-push@futo-notes.iam.gserviceaccount.com",
	}
}

func TestStoresOffByDefault(t *testing.T) {
	env := validEnv(t)
	env["APPLE_ACCEPT_SANDBOX"] = "true"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppStore != nil || cfg.Play != nil {
		t.Fatalf("stores = %+v, %+v", cfg.AppStore, cfg.Play)
	}
}

func TestLoadStores(t *testing.T) {
	env := validEnv(t)
	maps.Copy(env, appleEnv(t))
	maps.Copy(env, playEnv(t))
	env["APPLE_ACCEPT_SANDBOX"] = "true"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.AppStore
	if a == nil || a.BundleID != "tech.futo.notes" || a.AppAppleID != 1234567890 || a.IssuerID != "57246542-96fe-1a63-e053-0824d011072a" ||
		a.KeyID != "2X9R4HXF34" || a.PrivateKey == nil || !a.AcceptSandbox || !slices.Equal(a.ProductIDs, []string{"notes.monthly", "notes.yearly"}) {
		t.Fatalf("app store = %+v", a)
	}
	p := cfg.Play
	if p == nil || p.PackageName != "tech.futo.notes" || p.ServiceAccount == nil || p.ServiceAccount.Email != "play-api@futo-notes.iam.gserviceaccount.com" ||
		!slices.Equal(p.ProductIDs, []string{"notes_monthly"}) || p.RTDNAudience != "https://notes.example.com/api/billing/webhooks/play" ||
		p.RTDNServiceAccount != "rtdn-push@futo-notes.iam.gserviceaccount.com" {
		t.Fatalf("play = %+v", p)
	}

	delete(env, "APPLE_ACCEPT_SANDBOX")
	if cfg, err = config.Load(getenv(env)); err != nil || cfg.AppStore.AcceptSandbox {
		t.Fatalf("sandbox default: %+v, %v", cfg.AppStore, err)
	}
}

func TestLoadStoresRejects(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		base   func(*testing.T) map[string]string
		change map[string]string
		want   string
	}{
		"apple partial":          {appleEnv, map[string]string{"APPLE_KEY_ID": "", "APPLE_PRIVATE_KEY": " "}, "APPLE_KEY_ID, APPLE_PRIVATE_KEY also required"},
		"apple app id":           {appleEnv, map[string]string{"APPLE_APP_APPLE_ID": "tech.futo.notes"}, "APPLE_APP_APPLE_ID"},
		"apple key not pem":      {appleEnv, map[string]string{"APPLE_PRIVATE_KEY": "MIGTAgEAMBMGByqGSM49"}, "APPLE_PRIVATE_KEY"},
		"apple key rsa":          {appleEnv, map[string]string{"APPLE_PRIVATE_KEY": pkcs8PEM(t, rsaKey)}, "APPLE_PRIVATE_KEY"},
		"apple no products":      {appleEnv, map[string]string{"APPLE_SUBSCRIPTION_PRODUCT_IDS": " , "}, "APPLE_SUBSCRIPTION_PRODUCT_IDS"},
		"apple sandbox flag":     {appleEnv, map[string]string{"APPLE_ACCEPT_SANDBOX": "sometimes"}, "APPLE_ACCEPT_SANDBOX"},
		"play partial":           {playEnv, map[string]string{"PLAY_RTDN_AUDIENCE": ""}, "PLAY_RTDN_AUDIENCE also required"},
		"play service account":   {playEnv, map[string]string{"PLAY_SERVICE_ACCOUNT_JSON": `{"type":"authorized_user"}`}, "PLAY_SERVICE_ACCOUNT_JSON"},
		"play rtdn account":      {playEnv, map[string]string{"PLAY_RTDN_SERVICE_ACCOUNT": "not an email"}, "PLAY_RTDN_SERVICE_ACCOUNT"},
		"play no products":       {playEnv, map[string]string{"PLAY_SUBSCRIPTION_PRODUCT_IDS": ","}, "PLAY_SUBSCRIPTION_PRODUCT_IDS"},
		"apple only one var set": {func(*testing.T) map[string]string { return map[string]string{"APPLE_BUNDLE_ID": "tech.futo.notes"} }, nil, "App Store billing is partly configured"},
		"play only one var set":  {func(*testing.T) map[string]string { return map[string]string{"PLAY_PACKAGE_NAME": "tech.futo.notes"} }, nil, "Google Play billing is partly configured"},
	} {
		env := validEnv(t)
		maps.Copy(env, tc.base(t))
		maps.Copy(env, tc.change)
		_, err := config.Load(getenv(env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestStoreKitXcode(t *testing.T) {
	env := validEnv(t)
	env["DEV_UI"] = "true"
	env["APPLE_STOREKIT_XCODE"] = "true"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.AppStore
	if a == nil || !a.AcceptXcode || a.BundleID != "com.futo.notes.dev" || a.PrivateKey != nil ||
		!slices.Equal(a.ProductIDs, []string{"com.futo.notes.sync.monthly"}) {
		t.Fatalf("xcode only: app store = %+v", a)
	}

	maps.Copy(env, appleEnv(t))
	if cfg, err = config.Load(getenv(env)); err != nil {
		t.Fatal(err)
	}
	if a = cfg.AppStore; !a.AcceptXcode || a.BundleID != "tech.futo.notes" || a.PrivateKey == nil {
		t.Fatalf("with an app: app store = %+v", a)
	}

	delete(env, "APPLE_STOREKIT_XCODE")
	if cfg, err = config.Load(getenv(env)); err != nil || cfg.AppStore.AcceptXcode {
		t.Fatalf("default: %+v, %v", cfg.AppStore, err)
	}

	for name, change := range map[string]map[string]string{
		"without DEV_UI": {"DEV_UI": ""},
		"DEV_UI false":   {"DEV_UI": "false"},
	} {
		env := validEnv(t)
		env["APPLE_STOREKIT_XCODE"] = "true"
		maps.Copy(env, change)
		if _, err := config.Load(getenv(env)); err == nil || !strings.Contains(err.Error(), "APPLE_STOREKIT_XCODE") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	env = validEnv(t)
	env["DEV_UI"] = "true"
	env["APPLE_STOREKIT_XCODE"] = "maybe"
	if _, err := config.Load(getenv(env)); err == nil || !strings.Contains(err.Error(), "APPLE_STOREKIT_XCODE") {
		t.Errorf("bad flag: err = %v", err)
	}
}
