// Package polar implements just enough of Polar's webhook contract to accept
// subscription events over the standard library, with no Polar SDK: Standard
// Webhooks signature verification against the raw request body, and the
// subset of a subscription payload this server keeps.
//
// Polar signs deliveries per the Standard Webhooks specification. The secret
// is a base64 key behind a whsec_ prefix; the signed content is the message
// id, the unix timestamp, and the raw body joined by dots; the signature is
// the base64 HMAC-SHA256 of that, carried as one or more space-separated
// "v1,<signature>" entries so a secret can be rotated without dropping
// deliveries.
package polar

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The Standard Webhooks headers every delivery carries.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// SignatureTolerance is how far a delivery's timestamp may be from now. It
// is what stops a captured delivery from being replayed forever; without it
// a signature stays valid for as long as the secret does.
//
// It does not strand a retry. The Standard Webhooks specification is that
// "every time an attempt is retried the timestamp of the attempt is updated,
// while the timestamp of the original event remains the same" — a retry is
// re-signed with the moment it is sent, and keeps its original webhook-id.
// So a delivery missed during a restart still verifies hours later, and only
// a genuine replay of captured bytes is refused.
const SignatureTolerance = 5 * time.Minute

// secretPrefix marks a Standard Webhooks symmetric secret.
const secretPrefix = "whsec_"

// signatureVersion is the symmetric HMAC-SHA256 scheme. Entries carrying any
// other version belong to a scheme this server does not implement.
const signatureVersion = "v1"

// secretKey decodes POLAR_WEBHOOK_SECRET into the HMAC key.
func secretKey(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil {
		return nil, fmt.Errorf("POLAR_WEBHOOK_SECRET is not a base64 Standard Webhooks secret: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("POLAR_WEBHOOK_SECRET is empty")
	}
	return key, nil
}

// signature is the base64 HMAC-SHA256 over "<id>.<timestamp>.<body>".
func signature(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether header signs exactly these body bytes with secret,
// recently enough. body has to be the bytes as delivered: re-encoded JSON
// signs differently even when it means the same thing.
func Verify(secret string, header http.Header, body []byte, now time.Time) error {
	key, err := secretKey(secret)
	if err != nil {
		return err
	}
	id := header.Get(HeaderID)
	timestamp := header.Get(HeaderTimestamp)
	signatures := header.Get(HeaderSignature)
	if id == "" || timestamp == "" || signatures == "" {
		return fmt.Errorf("delivery is missing one of the %s, %s, or %s headers", HeaderID, HeaderTimestamp, HeaderSignature)
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%s %q is not a unix timestamp", HeaderTimestamp, timestamp)
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > SignatureTolerance || age < -SignatureTolerance {
		return fmt.Errorf("delivery timestamp is %s from now, outside the %s tolerance", age.Round(time.Second), SignatureTolerance)
	}

	expected := signature(key, id, timestamp, body)
	// Several entries mean a rotation is in progress; any one may match.
	for entry := range strings.SplitSeq(signatures, " ") {
		version, value, ok := strings.Cut(entry, ",")
		if !ok || version != signatureVersion {
			continue
		}
		if hmac.Equal([]byte(value), []byte(expected)) {
			return nil
		}
	}
	return errors.New("no delivery signature matches the configured secret")
}

// Sign produces the headers Polar would send for body. It exists so the tests
// and the /dev card can put a delivery through the real route, signature
// verification included, rather than around it.
func Sign(secret, id string, at time.Time, body []byte) (http.Header, error) {
	key, err := secretKey(secret)
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(at.Unix(), 10)
	header := http.Header{}
	header.Set(HeaderID, id)
	header.Set(HeaderTimestamp, timestamp)
	header.Set(HeaderSignature, signatureVersion+","+signature(key, id, timestamp, body))
	return header, nil
}
