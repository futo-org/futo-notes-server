package api

import (
	"errors"
	"net/http"
	"time"

	"futo-notes-server/internal/auth"
	"futo-notes-server/internal/hosted/account"
	"futo-notes-server/internal/hosted/config"
	"futo-notes-server/internal/httputil"
)

// regionResponse is a region as clients see it. Receipt keys stay private.
type regionResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Endpoint    string `json:"endpoint"`
}

func publicRegion(r config.Region) regionResponse {
	return regionResponse{ID: r.ID, Name: r.Name, Description: r.Description, Endpoint: r.Endpoint}
}

// handleRegions serves GET /api/regions, the regions a user can choose from.
func (s *Server) handleRegions(w http.ResponseWriter, r *http.Request) {
	regions := make([]regionResponse, 0, len(s.cfg.Regions))
	for _, region := range s.cfg.Regions {
		regions = append(regions, publicRegion(region))
	}
	httputil.WriteJSON(w, r, http.StatusOK, regions)
}

// accountResponse is the body of the account endpoints. Region and Deletion
// are null until a region is chosen and while no deletion is pending.
type accountResponse struct {
	User     auth.User         `json:"user"`
	Region   *regionResponse   `json:"region"`
	Deletion *deletionResponse `json:"deletion"`
	Usage    usageResponse     `json:"usage"`
}

type deletionResponse struct {
	RequestedAt time.Time `json:"requested_at"`
	PurgeAt     time.Time `json:"purge_at"`
}

type usageResponse struct {
	Collections int64 `json:"collections"`
	Blobs       int64 `json:"blobs"`
	StoredBytes int64 `json:"stored_bytes"`
}

// writeAccount answers with the caller's account.
func (s *Server) writeAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.accounts.Account(r.Context(), principalFrom(r).Session.User.ID)
	if err != nil {
		serverError(w, r, "reading account", err)
		return
	}
	response := accountResponse{
		User:  a.User,
		Usage: usageResponse{Collections: a.Collections, Blobs: a.Blobs, StoredBytes: a.StoredBytes},
	}
	if region, ok := s.regions[a.RegionID]; ok {
		response.Region = new(publicRegion(region))
	}
	if a.DeletionRequestedAt != nil && a.PurgeAt != nil {
		response.Deletion = &deletionResponse{RequestedAt: a.DeletionRequestedAt.UTC(), PurgeAt: a.PurgeAt.UTC()}
	}
	httputil.WriteJSON(w, r, http.StatusOK, response)
}

// handleAccount serves GET /api/account.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.writeAccount(w, r)
}

// handleChooseRegion serves PUT /api/account/region. A region is chosen once
// and then fixed.
func (s *Server) handleChooseRegion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RegionID string `json:"region_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, ok := s.regions[body.RegionID]; !ok {
		badRequest(w, r, "Unknown region")
		return
	}
	err := s.accounts.ChooseRegion(r.Context(), principalFrom(r).Session.User.ID, body.RegionID)
	if errors.Is(err, account.ErrRegionChosen) {
		httputil.WriteError(w, r, http.StatusConflict, codeRegionAlreadyChosen, "Region already chosen")
		return
	}
	if err != nil {
		serverError(w, r, "choosing region", err)
		return
	}
	s.writeAccount(w, r)
}

// handleDeleteAccount serves DELETE /api/account: it schedules the account's
// deletion and signs out every other device.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if err := s.accounts.RequestDeletion(r.Context(), p.Session.User.ID, p.Session.ID); err != nil {
		serverError(w, r, "requesting account deletion", err)
		return
	}
	s.writeAccount(w, r)
}

// handleRestoreAccount serves POST /api/account/restore, which cancels a
// pending deletion.
func (s *Server) handleRestoreAccount(w http.ResponseWriter, r *http.Request) {
	err := s.accounts.Restore(r.Context(), principalFrom(r).Session.User.ID)
	if errors.Is(err, account.ErrPurgeStarted) {
		httputil.WriteError(w, r, http.StatusConflict, codePurgeStarted, "The deletion grace period is over; the account can no longer be restored")
		return
	}
	if err != nil {
		serverError(w, r, "restoring account", err)
		return
	}
	s.writeAccount(w, r)
}

// sessionResponse is one device in GET /api/sessions.
type sessionResponse struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent"`
	DeviceName *string   `json:"device_name"`
	Current    bool      `json:"current"`
}

type sessionsResponse struct {
	Sessions []sessionResponse `json:"sessions"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// handleSessions serves GET /api/sessions, the caller's signed-in devices.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	sessions, err := s.accounts.Sessions(r.Context(), p.Session.User.ID)
	if err != nil {
		serverError(w, r, "listing sessions", err)
		return
	}
	response := sessionsResponse{Sessions: make([]sessionResponse, 0, len(sessions))}
	for _, d := range sessions {
		response.Sessions = append(response.Sessions, sessionResponse{
			ID:         d.ID,
			CreatedAt:  d.CreatedAt.UTC(),
			LastUsedAt: d.LastUsedAt.UTC(),
			ExpiresAt:  d.ExpiresAt.UTC(),
			UserAgent:  d.UserAgent,
			DeviceName: nullable(d.DeviceName),
			Current:    d.ID == p.Session.ID,
		})
	}
	httputil.WriteJSON(w, r, http.StatusOK, response)
}

// handleRevokeSession serves DELETE /api/sessions/{id}: it signs the device
// out and revokes its storage refresh token.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	err := s.accounts.RevokeSession(r.Context(), principalFrom(r).Session.User.ID, r.PathValue("id"))
	if errors.Is(err, account.ErrNotFound) {
		httputil.WriteError(w, r, http.StatusNotFound, httputil.CodeNotFound, "Session not found")
		return
	}
	if err != nil {
		serverError(w, r, "revoking session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
