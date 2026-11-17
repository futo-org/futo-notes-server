package syncapi

import "net/http"

// Capability is the discovery document served at GET /. Clients call it once
// when adding a server, to learn which login flow to drive and whether
// retry-safe mutations exist.
type Capability struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	AuthMode    string      `json:"auth_mode"`
	Signup      string      `json:"signup"`
	Billing     bool        `json:"billing"`
	MutationIDs MutationIDs `json:"mutation_ids"`
}

// MutationIDs describes the server's Mutation-Id support in the capability
// document.
type MutationIDs struct {
	Supported                bool   `json:"supported"`
	Required                 bool   `json:"required"`
	RetentionDays            int    `json:"retention_days"`
	SuccessfulCreateOutcomes string `json:"successful_create_outcomes"`
}

// NewCapability returns the capability document of a server at version that
// logs in with authMode and whose signup policy is signup. Mutation-Id support
// is the same everywhere the sync handlers run.
func NewCapability(version, authMode, signup string) Capability {
	return Capability{
		Name:     "futo-notes",
		Version:  version,
		AuthMode: authMode,
		Signup:   signup,
		Billing:  false,
		MutationIDs: MutationIDs{
			Supported:                true,
			Required:                 false,
			RetentionDays:            30,
			SuccessfulCreateOutcomes: "durable",
		},
	}
}

// HandleCapability serves document at GET /.
func HandleCapability(document Capability) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, document)
	}
}
