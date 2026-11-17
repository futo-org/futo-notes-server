// Package syncapi holds the HTTP handlers of the sync contract that both the
// self-hosted server (cmd/server) and the hosted API (cmd/api) serve: auth
// session endpoints, collections, keys, objects, mutation recovery and sync
// events. The bytes these handlers write are the client contract.
//
// Handlers read the caller from the request context, which each server's own
// authentication middleware fills with WithSession.
package syncapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"
)

// ErrorResponse is the JSON body of every sync API error.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteError answers with status and the sync error body carrying msg.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Error: msg})
}

// WriteJSON answers with status and v encoded as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error().Err(err).Msg("writing JSON response")
	}
}

// ServerError logs an internal failure with request context and answers with
// the generic 500 body. action says what the handler was doing.
func ServerError(w http.ResponseWriter, r *http.Request, action string, err error) {
	hlog.FromRequest(r).Error().Err(err).Str("action", action).Str("method", r.Method).Str("path", r.URL.Path).Msg("internal server error")
	WriteError(w, http.StatusInternalServerError, "internal server error")
}

// RecoverPanic turns a handler panic into a logged generic 500 that closes the
// connection, with the sync contract's error body. http.ErrAbortHandler is
// re-panicked so the server aborts the response as intended.
func RecoverPanic(next http.Handler) http.Handler {
	return RecoverPanicWith(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusInternalServerError, "internal server error")
	})(next)
}

// RecoverPanicWith is RecoverPanic with the 500 response written by answer.
func RecoverPanicWith(answer func(http.ResponseWriter, *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				panicValue := recover()
				if panicValue == nil {
					return
				}
				if err, ok := panicValue.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(panicValue)
				}
				hlog.FromRequest(r).Error().
					Str(zerolog.ErrorFieldName, fmt.Sprint(panicValue)).
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Str("stack", string(debug.Stack())).
					Msg("panic")
				w.Header().Set("Connection", "close")
				answer(w, r)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
