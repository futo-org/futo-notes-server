package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// serverError logs an internal failure with request context and answers with
// the generic 500 body. action says what the handler was doing.
func serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	slog.Error(action, "err", err, "method", r.Method, "path", r.URL.Path)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			panicValue := recover()
			if panicValue == nil {
				return
			}
			if err, ok := panicValue.(error); ok && err == http.ErrAbortHandler {
				panic(panicValue)
			}
			slog.Error("panic",
				"err", fmt.Sprintf("%v", panicValue),
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()))
			w.Header().Set("Connection", "close")
			writeError(w, http.StatusInternalServerError, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
