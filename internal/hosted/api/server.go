// Package api is the hosted API's HTTP surface: the sync contract (shared
// with the self-hosted server through internal/syncapi) with blob bytes moved
// to the regional storage services, OIDC sign-in for the dashboard and apps,
// storage tokens, and account, region and device management.
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/db"
	"futo-notes-server/internal/events"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/hosted/storageclient"
	"futo-notes-server/internal/hostedauth"
	"futo-notes-server/internal/httputil"
	"futo-notes-server/internal/syncapi"
)

// Options are what a Server is built from.
type Options struct {
	Config   config.Config
	Database *db.DB
	// Hub fans sync events out to this replica's streams; events.Listen
	// feeds it from Postgres.
	Hub *events.Hub
	// Storage reaches the regions' storage services; only the DEV_UI job
	// triggers use it.
	Storage *storageclient.Client
	Metrics *Metrics
	// Version is reported in the capability document.
	Version string
}

// Server serves the hosted API. Its embedded Readiness answers the readiness
// probe and drains it.
type Server struct {
	httputil.Readiness
	cfg       config.Config
	database  *db.DB
	accounts  *account.Store
	hub       *events.Hub
	publisher events.Publisher
	minter    *hostedauth.Minter
	receipts  *hostedauth.ReceiptVerifier
	regions   map[string]config.Region
	metrics   *Metrics
	version   string
	oidc      *oidcClient
	storage   *storageclient.Client
}

// New returns a Server for o.
func New(o Options) (*Server, error) {
	regions := make(map[string]config.Region, len(o.Config.Regions))
	receiptKeys := make(map[string]hostedauth.KeySet, len(o.Config.Regions))
	for _, region := range o.Config.Regions {
		regions[region.ID] = region
		receiptKeys[region.ID] = region.ReceiptKeys
	}
	receipts, err := hostedauth.NewReceiptVerifier(receiptKeys)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:       o.Config,
		database:  o.Database,
		accounts:  account.New(o.Database),
		hub:       o.Hub,
		publisher: events.PostgresPublisher{},
		minter:    hostedauth.NewMinter(o.Config.SigningKey),
		receipts:  receipts,
		regions:   regions,
		metrics:   o.Metrics,
		version:   o.Version,
		oidc:      newOIDCClient(o.Config.OIDC, o.Config.PublicURL+callbackPath),
		storage:   o.Storage,
	}, nil
}

// Accounts returns the account store the server acts through.
func (s *Server) Accounts() *account.Store { return s.accounts }

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.Readiness.Wrap(s.router())
}

func (s *Server) router() chi.Router {
	r := chi.NewRouter()
	r.Use(
		hlog.NewHandler(log.Logger),
		hlog.RequestIDHandler("request_id", "X-Request-Id"),
		hlog.RemoteAddrHandler("remote_ip"),
		hlog.UserAgentHandler("user_agent"),
		hlog.AccessHandler(httputil.LogAccess),
		s.metrics.http.Middleware,
		syncapi.RecoverPanic,
		chimw.GetHead,
	)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusNotFound, "Not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteError(w, r, http.StatusMethodNotAllowed, "Method not allowed")
	})

	r.Get("/", syncapi.HandleCapability(syncapi.NewCapability(s.version, "oidc", "open")))
	r.Route("/api", func(api chi.Router) {
		api.Get("/regions", s.handleRegions)
		api.Get("/auth/oidc/start", s.handleOIDCStart)
		api.Get(callbackRoute, s.handleOIDCCallback)
		api.Post("/auth/oidc/token", s.handleOIDCToken)
		api.Post("/storage/token", s.handleStorageToken)

		api.Group(func(sync chi.Router) {
			sync.Use(s.authenticate(syncErrors))
			sync.Post("/auth/logout", syncapi.HandleLogout(s.database))
			sync.Group(func(gated chi.Router) {
				gated.Use(requireActiveAccount)
				s.syncRoutes(gated)
			})
		})

		api.Group(func(hosted chi.Router) {
			hosted.Use(s.authenticate(hostedErrors))
			hosted.Get("/account", s.handleAccount)
			hosted.Put("/account/region", s.handleChooseRegion)
			hosted.Delete("/account", s.handleDeleteAccount)
			hosted.Post("/account/restore", s.handleRestoreAccount)
			hosted.Get("/sessions", s.handleSessions)
			hosted.Delete("/sessions/{id}", s.handleRevokeSession)
		})
	})
	s.dashboardRoutes(r)
	if s.cfg.DevUI {
		r.Get("/dev", handleDevUI)
		s.devJobRoutes(r)
		log.Info().Str("path", "/dev").Msg("dev UI enabled")
	}
	return r
}

// syncRoutes mounts the sync contract. The routes that carry blob bytes are
// absent: clients upload to and download from their region's storage
// service, and object writes name a storage receipt instead of a staged key.
func (s *Server) syncRoutes(r chi.Router) {
	blobs := receiptBlobs{verifier: s.receipts, metrics: s.metrics}
	r.Get("/auth", syncapi.HandleCurrentUser)
	r.Post("/collections", syncapi.HandleClaimCollection(s.database))
	r.Get("/collections", syncapi.HandleListCollections(s.database))
	r.Get("/collections/{id}", syncapi.HandleGetCollection(s.database))
	r.Delete("/collections/{id}", syncapi.HandleDeleteCollection(s.database))
	r.Get("/collections/{id}/key", syncapi.HandleGetCollectionKey(s.database))
	r.Put("/collections/{id}/key", syncapi.HandlePutCollectionKey(s.database))
	r.Get("/collections/{id}/objects", syncapi.HandleListObjects(s.database))
	r.Post("/collections/{id}/objects", syncapi.HandleCreateObject(s.database, s.publisher, blobs))
	r.Get("/collections/{id}/objects/{objectId}", syncapi.HandleGetObject(s.database))
	r.Put("/collections/{id}/objects/{objectId}", syncapi.HandleUpdateObject(s.database, s.publisher, blobs))
	r.Delete("/collections/{id}/objects/{objectId}", syncapi.HandleDeleteObject(s.database, s.publisher))
	r.Get("/collections/{id}/create-mutations/{mutationId}", syncapi.HandleRecoverCreate(s.database))
	r.Get("/sync/events", s.metrics.countStreams(syncapi.HandleSyncEvents(s.hub)))
}

// errorShape picks how authentication failures are answered: the sync
// contract's bodies on sync routes, the hosted body elsewhere.
type errorShape int

const (
	syncErrors errorShape = iota
	hostedErrors
)

type principalCtxKey struct{}

// principalFrom returns the caller authenticate attached.
func principalFrom(r *http.Request) account.Principal {
	return r.Context().Value(principalCtxKey{}).(account.Principal)
}

// authenticate requires a live session, from the session cookie or a bearer
// token, and attaches its caller for both syncapi and this package. A
// session presented as the cookie gets the cookie extended when it slides.
func (s *Server) authenticate(shape errorShape) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := syncapi.SessionToken(r)
			if token == "" {
				if shape == syncErrors {
					syncapi.WriteUnauthorized(w)
				} else {
					httputil.WriteError(w, r, http.StatusUnauthorized, "Unauthorized")
				}
				return
			}
			principal, err := s.accounts.Authenticate(r.Context(), token)
			switch {
			case errors.Is(err, auth.ErrNoSession) && shape == syncErrors:
				syncapi.WriteInvalidSession(w)
				return
			case errors.Is(err, auth.ErrNoSession):
				w.Header().Set("WWW-Authenticate", `Bearer realm="futo-notes", error="invalid_token"`)
				httputil.WriteError(w, r, http.StatusUnauthorized, "Session expired or invalid")
				return
			case err != nil && shape == syncErrors:
				syncapi.ServerError(w, r, "validating session", err)
				return
			case err != nil:
				serverError(w, r, "validating session", err)
				return
			}
			if cookie, err := r.Cookie(syncapi.SessionCookie); principal.Slid && err == nil && cookie.Value == token {
				s.setSessionCookie(w, token)
			}
			hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
				return c.Str("user", principal.Session.User.ID)
			})
			ctx := syncapi.WithSession(r.Context(), &principal.Session)
			ctx = context.WithValue(ctx, principalCtxKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireActiveAccount refuses sync requests while the caller's account is
// pending deletion; only account, session and logout endpoints keep working.
func requireActiveAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFrom(r).PendingDeletion() {
			syncapi.WriteError(w, http.StatusForbidden, account.ErrPendingDeletion.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}
