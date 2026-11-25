package hosted

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"

	"uuid"
)

// TestReleasedStack is the client smoke test of the test stack a release
// ships (scripts/hosted-stack): sign in through the mock issuer's
// /test/authorize, subscribe through the mock web checkout, exchange for a
// storage token, upload, and create an object with the receipt. It runs against a stack `hosted-stack.sh up` started,
// found through the HOSTED_STACK_* lines it prints, and is skipped otherwise.
func TestReleasedStack(t *testing.T) {
	apiURL := os.Getenv("HOSTED_STACK_API_URL")
	if apiURL == "" {
		t.Skip("HOSTED_STACK_API_URL not set; start a stack with hosted-stack.sh up")
	}
	s := &stack{apiURL: apiURL, issuer: os.Getenv("HOSTED_STACK_ISSUER"), storage: map[string]string{}}
	ids := strings.Split(os.Getenv("HOSTED_STACK_REGIONS"), ",")
	for _, id := range ids {
		s.storage[id] = os.Getenv("HOSTED_STACK_STORAGE_URL_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")))
	}

	d := s.login(t, "smoke-"+uuid.NewV7().String(), "hosted stack smoke")

	var regions []struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	d.expectJSON(http.MethodGet, "/api/regions", nil, &regions)
	if len(regions) != len(ids) {
		t.Fatalf("GET /api/regions listed %+v, want %v", regions, ids)
	}
	for _, r := range regions {
		if s.storage[r.ID] == "" || s.storage[r.ID] != r.Endpoint {
			t.Fatalf("region %s endpoint %q, want %q", r.ID, r.Endpoint, s.storage[r.ID])
		}
	}
	home := ids[0]
	d.expectJSON(http.MethodPut, "/api/account/region", map[string]string{"region_id": home}, nil)
	d.buySubscription()

	grant := d.storageToken()
	if grant.Region.ID != home || grant.Region.Endpoint != s.storage[home] {
		t.Fatalf("storage token for region %+v, want %s at %s", grant.Region, home, s.storage[home])
	}
	data := randomBytes(t, 4096)
	up := d.upload(grant, data)
	if got := d.download(grant, up.Key); !bytes.Equal(got, data) {
		t.Fatalf("download returned %d bytes that differ from the %d uploaded", len(got), len(data))
	}

	collection := d.claimCollection()
	resp := d.api(http.MethodPost, objectPath(collection, ""), map[string]string{"receipt": up.Receipt}, "smoke-create")
	d.expect("create object", resp, http.StatusCreated)
	var created mutation
	decode(t, resp, &created)
	if created.Object.BlobKey == nil || *created.Object.BlobKey != up.Key ||
		created.Object.SizeBytes == nil || *created.Object.SizeBytes != "4096" {
		t.Fatalf("create returned %s", resp.body)
	}
}
