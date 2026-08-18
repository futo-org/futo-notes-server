package main

import (
	"database/sql"
	"log"
	"net/http"

	"futo-notes-server/internal/collections"
)

func handleClaimCollection(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, created, err := collections.Claim(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			log.Printf("claiming collection: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"collection": c})
	}
}

func handleListCollections(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs, err := collections.List(r.Context(), database, sessionFrom(r).User.ID)
		if err != nil {
			log.Printf("listing collections: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collections": cs})
	}
}

func handleGetCollection(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := collections.Get(r.Context(), database, sessionFrom(r).User.ID, r.PathValue("id"))
		if err != nil {
			log.Printf("getting collection: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if c == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collection": c})
	}
}
