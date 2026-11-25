// Package httpbody reads request bodies under a size cap.
package httpbody

import (
	"errors"
	"io"
	"net/http"
)

// Rejection is a body the handler must refuse, with the status and message to
// answer with. Each server writes it in its own error shape.
type Rejection struct {
	Status  int
	Message string
}

// ReadLimited reads at most limit bytes of r's body. A longer body is rejected
// with 413 and tooLarge as the message, an unreadable one with 400.
func ReadLimited(w http.ResponseWriter, r *http.Request, limit int64, tooLarge string) ([]byte, *Rejection) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		return body, nil
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return nil, &Rejection{Status: http.StatusRequestEntityTooLarge, Message: tooLarge}
	}
	return nil, &Rejection{Status: http.StatusBadRequest, Message: "could not read body"}
}
