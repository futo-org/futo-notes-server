package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestStrictJSONDistinguishesStringAndNumber(t *testing.T) {
	number := decodeStrict([]byte(`{"version":3}`))
	stringValue := decodeStrict([]byte(`{"version":"3"}`))
	if reflect.DeepEqual(number, stringValue) {
		t.Fatal("strict JSON comparison treated a string and number as equal")
	}
	got, _ := jsonPath(number, "version")
	if _, ok := got.(json.Number); !ok {
		t.Fatalf("number decoded as %T, want json.Number", got)
	}
}

func TestAcceptedDeviationDoesNotHideAdditionalProblem(t *testing.T) {
	pair := responsePair{
		TS: wireResponse{Status: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": []string{"invalid"}}, JSON: decodeStrict([]byte(`{"error":"session expired or invalid","code":"invalid_session"}`))},
		Go: wireResponse{Status: http.StatusUnauthorized, Header: http.Header{}, JSON: decodeStrict([]byte(`{"error":"unauthorized"}`))},
	}
	if _, ok := matchAcceptedDeviation(allowMalformedAuthorization, []string{"header", "json"}, pair); !ok {
		t.Fatal("known malformed-authorization shape was not accepted")
	}
	if _, ok := matchAcceptedDeviation(allowMalformedAuthorization, []string{"header", "json", "new difference"}, pair); ok {
		t.Fatal("allowlist hid an additional difference")
	}
}

func TestCapabilityVersionDeviationIsNarrow(t *testing.T) {
	pair := responsePair{
		TS: wireResponse{Status: http.StatusOK, JSON: decodeStrict([]byte(`{"name":"futo-notes","version":"ts","auth_mode":"dev"}`))},
		Go: wireResponse{Status: http.StatusOK, JSON: decodeStrict([]byte(`{"name":"futo-notes","version":"go","auth_mode":"dev"}`))},
	}
	if _, ok := matchAcceptedDeviation(allowCapabilityVersion, []string{"JSON differs here"}, pair); !ok {
		t.Fatal("version-only capability difference was not accepted")
	}
	pair.Go.JSON = decodeStrict([]byte(`{"name":"different","version":"go","auth_mode":"dev"}`))
	if _, ok := matchAcceptedDeviation(allowCapabilityVersion, []string{"JSON differs here"}, pair); ok {
		t.Fatal("capability allowlist hid a non-version difference")
	}
}

// The tombstone race guard is the one place the Go rewrite answers a different
// status than the TypeScript server, so the allowance has to accept exactly
// that pair and nothing wider.
func TestTombstoneStaleVersionDeviationIsNarrow(t *testing.T) {
	problems := []string{"expected status 409", "status differs", "JSON differs"}
	pair := responsePair{
		TS: wireResponse{Status: http.StatusOK, JSON: decodeStrict([]byte(`{"object":{"id":"one","version":"4","change_seq":"9","deleted":true},"collectionVersion":9}`))},
		Go: wireResponse{Status: http.StatusConflict, JSON: decodeStrict([]byte(`{"error":"version conflict","currentVersion":3,"currentBlobKey":"blob"}`))},
	}
	if _, ok := matchAcceptedDeviation(allowTombstoneStaleVersion, problems, pair); !ok {
		t.Fatal("known tombstone stale-version shape was not accepted")
	}
	if _, ok := matchAcceptedDeviation(allowTombstoneStaleVersion, append(problems, "new difference"), pair); ok {
		t.Fatal("allowlist hid an additional difference")
	}
	stillOK := pair
	stillOK.Go = wireResponse{Status: http.StatusOK, JSON: decodeStrict([]byte(`{"object":{"id":"one","version":"3","change_seq":"8","deleted":true},"collectionVersion":8}`))}
	if _, ok := matchAcceptedDeviation(allowTombstoneStaleVersion, problems, stillOK); ok {
		t.Fatal("allowlist accepted a Go side that skipped the race guard")
	}
	staleCurrent := pair
	staleCurrent.Go.JSON = decodeStrict([]byte(`{"error":"version conflict","currentVersion":1,"currentBlobKey":"blob"}`))
	if _, ok := matchAcceptedDeviation(allowTombstoneStaleVersion, problems, staleCurrent); ok {
		t.Fatal("allowlist accepted a conflict that did not report a newer version")
	}
}

func TestPullLimitProblems(t *testing.T) {
	objects := make([]any, 1000)
	value := map[string]any{
		"objects": objects, "hasMore": true, "nextCursor": json.Number("1000"),
	}
	if problems := pullLimitProblems(value); len(problems) != 0 {
		t.Fatalf("valid pull page problems: %v", problems)
	}
	value["hasMore"] = false
	if problems := pullLimitProblems(value); len(problems) != 1 {
		t.Fatalf("invalid pull page problems: %v", problems)
	}
}

func TestNormalizeIdentityStringHandlesCompositeKeys(t *testing.T) {
	target := &target{name: "test", identities: map[string]string{}, reverse: map[string]string{}}
	if err := target.bind(phUserA, "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatal(err)
	}
	key := "11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222"
	want := phUserA + "/22222222-2222-2222-2222-222222222222"
	if got := normalizeIdentityString(key, target); got != want {
		t.Fatalf("normalizeIdentityString() = %q, want %q", got, want)
	}
}

func TestDownloadFrameRoundTrip(t *testing.T) {
	body := []byte{0, 3, 'k', 'e', 'y', 0, 0, 0, 0, 3, 'o', 'n', 'e'}
	frames, err := decodeDownloadFrames(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []downloadFrame{{Key: "key", Status: 0, Blob: []byte("one")}}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("frames = %#v, want %#v", frames, want)
	}
}

func TestValidateTimestamps(t *testing.T) {
	now := time.Now().UTC()
	if issues := validateTimestamps(map[string]any{"created_at": now.Format(time.RFC3339Nano)}, now); len(issues) != 0 {
		t.Fatalf("valid timestamp issues: %v", issues)
	}
	if issues := validateTimestamps(map[string]any{"created_at": "yesterday-ish"}, now); len(issues) != 1 {
		t.Fatalf("invalid timestamp issues: %v", issues)
	}
}
