package main

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// serverError logs an internal failure with request context and answers with
// the generic 500 body. action says what the handler was doing.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	log.Error().Err(err).Str("action", action).Str("method", r.Method).Str("path", r.URL.Path).Msg("internal server error")
	writeError(w, http.StatusInternalServerError, "internal server error")
}

// recoverPanic turns a handler panic into a logged generic 500 that closes the
// connection. http.ErrAbortHandler is re-panicked so the server aborts the
// response as intended.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			panicValue := recover()
			if panicValue == nil {
				return
			}
			if err, ok := panicValue.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(panicValue)
			}
			log.Error().
				Str(zerolog.ErrorFieldName, fmt.Sprint(panicValue)).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("stack", string(debug.Stack())).
				Msg("panic")
			w.Header().Set("Connection", "close")
			writeError(w, http.StatusInternalServerError, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
