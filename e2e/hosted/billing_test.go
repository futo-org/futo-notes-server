package hosted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"futo-notes-server/internal/mockresend"
)

// billingState is the body of GET /api/billing.
type billingState struct {
	Entitled bool    `json:"entitled"`
	Status   *string `json:"status"`
	Source   *string `json:"source"`
}

func (d *device) billing() billingState {
	d.t.Helper()
	var b billingState
	d.expectJSON(http.MethodGet, "/api/billing", nil, &b)
	return b
}

// buySubscription subscribes through the mock web checkout, as the apps'
// test harness does: ask for the checkout, then post its URL as is.
func (d *device) buySubscription() {
	d.t.Helper()
	var checkout struct {
		URL string `json:"url"`
	}
	d.expectJSON(http.MethodPost, "/api/billing/checkout", nil, &checkout)
	target, err := url.Parse(checkout.URL)
	if err != nil || !strings.HasPrefix(checkout.URL, d.s.apiURL+"/dev/billing/checkout?") {
		d.t.Fatalf("checkout URL %q (%v)", checkout.URL, err)
	}
	form := target.Query().Encode()
	target.RawQuery = ""
	resp := d.do(request{method: http.MethodPost, url: target.String(), body: strings.NewReader(form),
		length: int64(len(form)), contentType: "application/x-www-form-urlencoded"})
	d.expect("completing the mock checkout", resp, http.StatusOK)
	if !d.billing().Entitled {
		d.t.Fatal("not entitled after the mock checkout")
	}
}

// dev calls a DEV_UI route as the device.
func (d *device) dev(path string, out any) {
	d.t.Helper()
	d.expectJSON(http.MethodPost, path, nil, out)
}

const subscriptionRequired = `{"code":"subscription_required","error":"subscription required"}` + "\n"

// TestHostedBillingScenario subscribes with the mock web provider and
// writes; lapses, so writes get 402, reads keep working and uploads get 403;
// resubscribes; then lapses for good and is purged 90 days later by the
// dev time controls, with the three lapse emails in Mailpit in order. The
// emails are checked when MAILPIT_TEST_URL and MAILPIT_TEST_SMTP_ADDR are
// set.
func TestHostedBillingScenario(t *testing.T) {
	apiEnv := []string{"BILLING_WEB_PROVIDER=mock", "BILLING_MOCK_WEBHOOK_SECRET=e2e-mock-webhook-secret"}
	mailpit := strings.TrimSuffix(os.Getenv("MAILPIT_TEST_URL"), "/")
	if mailpit != "" {
		resend := httptest.NewServer(mockresend.Handler(os.Getenv("MAILPIT_TEST_SMTP_ADDR")))
		t.Cleanup(resend.Close)
		apiEnv = append(apiEnv, "RESEND_API_URL="+resend.URL, "RESEND_API_KEY=re_e2e")
	}
	s := startStack(t, apiEnv...)
	const home = "us-east"
	d := s.login(t, "e2e-billing-"+uuid.NewV7().String(), "e2e billing")
	s.cleanupUser(t, d.userID)
	d.expectJSON(http.MethodPut, "/api/account/region", map[string]string{"region_id": home}, nil)

	var capability struct {
		Billing bool `json:"billing"`
	}
	d.expectJSON(http.MethodGet, "/", nil, &capability)
	if !capability.Billing {
		t.Fatal("capability billing is false with the mock web provider")
	}

	// A new account reads but cannot write.
	collection := d.claimCollection()
	key := map[string]any{"key_salt": "salt", "key_kdf": map[string]any{"name": "argon2id"}, "encrypted_vault_key": "vault"}
	if resp := d.api(http.MethodPut, "/api/collections/"+collection+"/key", key, ""); resp.status != http.StatusPaymentRequired ||
		string(resp.body) != subscriptionRequired {
		t.Fatalf("key PUT without a subscription: %d %s", resp.status, resp.body)
	}
	readOnly := d.storageToken()
	d.expectUploadRefused(readOnly)

	// Subscribe, and write.
	d.buySubscription()
	grant := d.storageToken()
	data := randomBytes(t, 2048)
	up := d.upload(grant, data)
	resp := d.api(http.MethodPost, objectPath(collection, ""), map[string]string{"receipt": up.Receipt}, "billing-create")
	d.expect("create once subscribed", resp, http.StatusCreated)
	d.expect("key PUT once subscribed", d.api(http.MethodPut, "/api/collections/"+collection+"/key", key, ""), http.StatusOK)

	// Lapse: writes 402, reads work, uploads 403.
	d.dev("/dev/billing/events/lapse", nil)
	if resp := d.api(http.MethodPost, objectPath(collection, ""), map[string]string{"receipt": up.Receipt}, "billing-lapsed"); resp.status != http.StatusPaymentRequired {
		t.Fatalf("create after the lapse: %d %s", resp.status, resp.body)
	}
	d.expect("list objects after the lapse", d.api(http.MethodGet, objectPath(collection, ""), nil, ""), http.StatusOK)
	lapsedGrant := d.storageToken()
	if got := d.download(lapsedGrant, up.Key); string(got) != string(data) {
		t.Fatal("the lapsed account cannot read its blob")
	}
	d.expectUploadRefused(lapsedGrant)

	// Resubscribe: writes again.
	d.buySubscription()
	again := d.upload(d.storageToken(), randomBytes(t, 512))
	d.expect("create after resubscribing", d.api(http.MethodPost, objectPath(collection, ""),
		map[string]string{"receipt": again.Receipt}, "billing-resubscribed"), http.StatusCreated)

	// Lapse for good: the lapse job records it and emails, the warning comes
	// 83 days later, the purge 90 days later.
	d.dev("/dev/billing/events/lapse", nil)
	var run struct {
		Notified, Warned, Purged int
	}
	d.dev("/dev/jobs/billing-lapses", &run)
	if run.Notified != 1 {
		t.Fatalf("lapse run = %+v", run)
	}
	d.dev("/dev/billing/lapsed-at?days=83", nil)
	d.dev("/dev/jobs/billing-lapses", &run)
	if run.Warned != 1 || run.Purged != 0 {
		t.Fatalf("warning run = %+v", run)
	}
	d.dev("/dev/billing/lapsed-at?days=7", nil)
	d.dev("/dev/jobs/billing-lapses", &run)
	if run.Purged != 1 {
		t.Fatalf("purge run = %+v", run)
	}
	if n := s.count(t, `SELECT count(*) FROM users WHERE id = $1`, d.userID); n != 0 {
		t.Fatal("the account survived its lapse purge")
	}
	if keys := s.keys(t, home, d.userID+"/"); len(keys) != 0 {
		t.Fatalf("blobs left after the lapse purge: %v", keys)
	}
	d.expect("session after the purge", d.api(http.MethodGet, "/api/billing", nil, ""), http.StatusUnauthorized)

	if mailpit == "" {
		t.Log("MAILPIT_TEST_URL not set; lapse emails not checked")
		return
	}
	if got := lapseEmails(t, mailpit, d.userEmail()); !slices.Equal(got, []string{"lapsed", "purge_warning", "purged"}) {
		t.Fatalf("emails in Mailpit = %v, want lapsed, purge_warning, purged", got)
	}
}

// userEmail is the address the device's sign-in gave the issuer.
func (d *device) userEmail() string { return d.sub + "@example.test" }

// expectUploadRefused checks storage refuses an upload with grant.
func (d *device) expectUploadRefused(grant storageGrant) {
	d.t.Helper()
	data := []byte("refused")
	resp := d.do(request{method: http.MethodPost, url: grant.Region.Endpoint + "/blobs", bearer: grant.AccessToken,
		body: strings.NewReader(string(data)), length: int64(len(data))})
	var body struct {
		Code string `json:"code"`
	}
	if resp.status != http.StatusForbidden || json.Unmarshal(resp.body, &body) != nil || body.Code != "write_not_allowed" {
		d.t.Fatalf("upload with a read-only token: %d %s", resp.status, resp.body)
	}
}

// lapseEmails returns the categories of the emails Mailpit holds for to,
// oldest first, once there are three of them.
func lapseEmails(t *testing.T, mailpit, to string) []string {
	t.Helper()
	var search struct {
		Messages []struct {
			Created time.Time
			Tags    []string
		}
	}
	for range 50 {
		resp, err := http.Get(mailpit + "/api/v1/search?query=" + url.QueryEscape(fmt.Sprintf("to:%q", to)))
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&search)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(search.Messages) >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	slices.SortStableFunc(search.Messages, func(a, b struct {
		Created time.Time
		Tags    []string
	}) int {
		return a.Created.Compare(b.Created)
	})
	var categories []string
	for _, m := range search.Messages {
		i := slices.IndexFunc(m.Tags, func(tag string) bool { return slices.Contains([]string{"lapsed", "purge_warning", "purged"}, tag) })
		if i < 0 {
			categories = append(categories, strings.Join(m.Tags, ","))
			continue
		}
		categories = append(categories, m.Tags[i])
	}
	return categories
}
