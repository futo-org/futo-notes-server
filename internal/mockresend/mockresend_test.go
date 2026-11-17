package mockresend_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"futo-notes-server/internal/mockresend"
)

func TestHandlerRefuses(t *testing.T) {
	// Every case is refused before relaying, so no SMTP server is needed.
	handler := mockresend.Handler("127.0.0.1:1")
	valid := `{"from":"a@example.test","to":"b@example.test","subject":"s","text":"t"}`
	for name, tc := range map[string]struct {
		method, path, auth, body string
		status                   int
		errName                  string
	}{
		"no key":          {http.MethodPost, "/emails", "", valid, http.StatusUnauthorized, "missing_api_key"},
		"not json":        {http.MethodPost, "/emails", "Bearer k", "{", http.StatusUnprocessableEntity, "validation_error"},
		"bad from":        {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, "a@example.test", "nobody", 1), http.StatusUnprocessableEntity, "validation_error"},
		"no recipients":   {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `"b@example.test"`, `[]`, 1), http.StatusUnprocessableEntity, "validation_error"},
		"no subject":      {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `"s"`, `""`, 1), http.StatusUnprocessableEntity, "validation_error"},
		"no body":         {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `,"text":"t"`, ``, 1), http.StatusUnprocessableEntity, "validation_error"},
		"header in tag":   {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `}`, `,"tags":[{"name":"category","value":"x\r\nBcc: c@example.test"}]}`, 1), http.StatusUnprocessableEntity, "validation_error"},
		"unknown route":   {http.MethodPost, "/emails/batch", "Bearer k", "[]", http.StatusNotFound, "not_found"},
		"wrong method":    {http.MethodGet, "/emails", "Bearer k", "", http.StatusNotFound, "not_found"},
		"too many to":     {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `"b@example.test"`, `[`+strings.Repeat(`"b@example.test",`, 50)+`"b@example.test"]`, 1), http.StatusUnprocessableEntity, "validation_error"},
		"header in to":    {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `b@example.test`, `b@example.test\r\nBcc: c@example.test`, 1), http.StatusUnprocessableEntity, "validation_error"},
		"array of nonstr": {http.MethodPost, "/emails", "Bearer k", strings.Replace(valid, `"b@example.test"`, `[1]`, 1), http.StatusUnprocessableEntity, "validation_error"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			var body struct {
				StatusCode int    `json:"statusCode"`
				Name       string `json:"name"`
				Message    string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", w.Body, err)
			}
			if w.Code != tc.status || body.StatusCode != tc.status || body.Name != tc.errName || body.Message == "" {
				t.Fatalf("%s %s = %d %s, want %d %s", tc.method, tc.path, w.Code, w.Body, tc.status, tc.errName)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	w := httptest.NewRecorder()
	mockresend.Handler("127.0.0.1:1").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health = %d %s", w.Code, w.Body)
	}
}
