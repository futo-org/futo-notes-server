package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/polar"
	"futo-notes-server/internal/standins"
	"futo-notes-server/internal/subscriptions"
	"futo-notes-server/internal/usage"
)

// Stand-in test mode, decided in docs/adr/0009-stand-in-test-mode.md: hosted
// mode run against in-process stand-ins for Zitadel and Polar, so a client's
// automated suite can drive Log in with FUTO, checkout, and billing on
// localhost with no network and no credentials.
//
// Everything the mode does hangs off config.StandinMode, which is the one
// read of STANDIN_MODE. A server that was not explicitly started this way has
// no stand-in listeners, no replaced settings, and no /standin routes on its
// mux at all.
//
// Two things at the bottom of this file are the exception, and deliberately
// so: setStorageQuota and deliverSignedSubscription are shared with the /dev
// cards, which fake the same two providers for a person rather than for a
// suite. They run whenever those cards do, flag or no flag, and neither is
// reachable except through a route one of the two flags mounted.
//
// The credentials are fixed strings that never leave this machine. The
// stand-ins do check them, so the flow that runs here is the real one: PKCE
// round trip, Polar access token, signed webhook.
const (
	standinClientID      = "standin-client"
	standinClientSecret  = "standin-client-secret"
	standinAccessToken   = "polar_oat_standin"
	standinProductID     = "polar_prod_standin"
	standinWebhookSecret = "whsec_c3RhbmQtaW4tc2lnbmluZy1rZXk="

	// The one identity a stand-in login produces. A suite that wants a
	// second account starts a second server on a fresh database.
	standinSub   = "standin-sub-1"
	standinEmail = "person@standin.test"
	standinName  = "Stand-in Person"
)

// startStandins boots the two stand-ins and returns the configuration the
// server runs with: the same settings, plus the eight hosted values pointed
// at the fakes. config.Load refuses to boot if any of those eight was set by
// hand, so nothing an operator configured is being overwritten here.
//
// serverOrigin is where this server is about to listen. The stand-in Polar
// needs it in order to deliver a webhook back, which is why the origin is
// decided before anything starts rather than read off the listener.
func startStandins(cfg config.Config, serverOrigin string) (config.Config, func(), error) {
	issuer, stopIssuer, err := standins.StartIssuer(
		standinClientID, standinClientSecret, standinSub, standinEmail, standinName)
	if err != nil {
		return cfg, nil, fmt.Errorf("starting the stand-in issuer: %w", err)
	}
	standinPolar, stopPolar, err := standins.StartPolar(standinAccessToken, standins.Webhook{
		URL:    serverOrigin + polarWebhookPath,
		Secret: standinWebhookSecret,
	})
	if err != nil {
		stopIssuer()
		return cfg, nil, fmt.Errorf("starting the stand-in Polar: %w", err)
	}

	cfg.OIDCIssuer = issuer.Origin
	cfg.OIDCClientID = standinClientID
	cfg.OIDCClientSecret = standinClientSecret
	cfg.OIDCRedirectURI = serverOrigin + config.OIDCCallbackPath
	cfg.PolarBaseURL = standinPolar.Origin
	cfg.PolarAccessToken = standinAccessToken
	cfg.PolarProductID = standinProductID
	cfg.PolarWebhookSecret = standinWebhookSecret

	// Said on every start, at warning level, naming what is fake. A server
	// that reached this line by accident has one line in its log that
	// explains every strange thing it is about to do.
	slog.Warn("STAND-IN MODE: this server's identity provider and payment provider are fakes",
		"login", "anyone who opens the login URL is signed in as "+standinEmail,
		"billing", "opening a checkout URL entitles the account for free",
		"account_controls", "POST /standin/lapse and /standin/quota rewrite entitlement and the plan",
		"listening", "loopback only",
		"use", "automated tests; never set STANDIN_MODE on a real server")

	return cfg, func() { stopPolar(); stopIssuer() }, nil
}

// registerStandinRoutes mounts the two account controls a suite needs in
// order to reach the refusals a real lapse and a real full disk produce.
// They are mounted only under the flag, which is what keeps them out of an
// ordinary binary's routing table entirely.
//
// Both go behind the middleware /api/ uses, so each acts for the account that
// called it and neither names an account itself.
func registerStandinRoutes(mux *http.ServeMux, cfg config.Config, database *db.DB, webhook http.HandlerFunc) {
	api := http.NewServeMux()
	api.HandleFunc("POST /standin/lapse", handleStandinLapse(cfg, database, webhook))
	api.HandleFunc("POST /standin/quota", handleStandinQuota(database))
	mux.Handle("/standin/", requireAuth(cfg, database, api))
}

// handleStandinLapse stops the calling account writing, so a suite can
// exercise the 402 path after it has already paid. It delivers the
// subscription.revoked Polar would send, signed, through the real webhook
// route — the same path a genuine lapse takes.
func handleStandinLapse(cfg config.Config, database *db.DB, webhook http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deliverSignedSubscription(w, r, cfg, database, webhook, "subscription.revoked", "canceled")
	}
}

// handleStandinQuota fills or sets the Storage Quota, so a suite can reach
// the 507 path without uploading ten gigabytes. An empty body fills it: the
// ceiling drops to what the account already stores, and the next byte is
// refused. A value puts it back.
//
// The one Plan is shared by every account, so this is a whole-server knob. A
// stand-in server has one identity and therefore one account, which is what
// makes that the same thing here.
func handleStandinQuota(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StorageQuotaBytes *int64 `json:"storage_quota_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}

		account, err := usage.Of(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			serverError(w, r, "reading the account's usage", err)
			return
		}
		quota := account.BytesUsed
		if body.StorageQuotaBytes != nil {
			if *body.StorageQuotaBytes < 0 {
				writeError(w, http.StatusBadRequest, "invalid storage_quota_bytes")
				return
			}
			quota = *body.StorageQuotaBytes
		}
		if err := setStorageQuota(r.Context(), database, quota); err != nil {
			serverError(w, r, "setting the stand-in storage quota", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{
			"storage_quota_bytes": quota,
			"bytes_used":          account.BytesUsed,
		})
	}
}

// setStorageQuota rewrites the one Plan's ceiling. No request path sets a
// Plan, so this exists for the test surfaces only: the /dev quota card and
// the stand-in control above.
func setStorageQuota(ctx context.Context, database *db.DB, bytes int64) error {
	_, err := database.ExecContext(ctx,
		`UPDATE plans SET storage_quota_bytes = $1 WHERE id = 'default'`, bytes)
	return err
}

// deliverSignedSubscription builds the subscription event Polar would send
// for the calling account, signs it with the configured webhook secret, and
// puts it through the real webhook route, answering with whatever that route
// said. Signature verification runs for real, so both callers — the /dev
// subscription card and the stand-in lapse control — exercise the whole path
// rather than a shortcut around it.
//
// Only call it from a handler behind requireAuth, where sessionFrom holds the
// account it acts for.
func deliverSignedSubscription(w http.ResponseWriter, r *http.Request, cfg config.Config,
	database *db.DB, webhook http.HandlerFunc, eventType, status string) {
	// The account's own customer, not an invented one: applying a delivery
	// rewrites the stored Polar customer id from what the delivery names, so
	// a made-up value here would leave the account pointing at a customer
	// that does not exist and break its next checkout.
	customer, err := subscriptions.CustomerOf(r.Context(), database, sessionFrom(r).User.ID)
	if err != nil {
		serverError(w, r, "reading the account's Polar customer", err)
		return
	}
	now := time.Now()
	periodEnd := now.AddDate(0, 1, 0).UTC()
	delivery, err := polar.Delivery(polar.Event{
		Type: eventType,
		Subscription: polar.Subscription{
			ID:                 "standin_subscription",
			Status:             status,
			CustomerID:         customer.ID,
			ExternalCustomerID: customer.ExternalID,
			CurrentPeriodEnd:   &periodEnd,
		},
	})
	if err != nil {
		serverError(w, r, "building a stand-in Polar delivery", err)
		return
	}
	header, err := polar.Sign(cfg.PolarWebhookSecret, "standin_"+db.Timestamp(now), now, delivery)
	if err != nil {
		serverError(w, r, "signing a stand-in Polar delivery", err)
		return
	}

	// The signed delivery goes through the webhook handler itself and answers
	// straight into this response, so the caller sees what Polar would have
	// got.
	signed := r.Clone(r.Context())
	signed.Method = http.MethodPost
	signed.URL = &url.URL{Path: polarWebhookPath}
	signed.Header = header
	signed.Body = io.NopCloser(bytes.NewReader(delivery))
	signed.ContentLength = int64(len(delivery))
	webhook(w, signed)
}
