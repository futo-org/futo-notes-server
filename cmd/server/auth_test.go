package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeDevLogin(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantEmail string
		wantName  string
		wantError string
	}{
		{name: "normalizes email and defaults name", body: `{"email":"  DEV@Example.COM "}`, wantEmail: "dev@example.com", wantName: "dev"},
		{name: "trims explicit name", body: `{"email":"a@example.com","name":"  A User  "}`, wantEmail: "a@example.com", wantName: "A User"},
		{name: "missing email", body: `{}`, wantError: "email is required"},
		{name: "blank email", body: `{"email":"  "}`, wantError: "email is required"},
		{name: "bad json", body: `{`, wantError: "invalid json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/auth/dev/login", strings.NewReader(tt.body))
			got, gotError := decodeDevLogin(r)
			if gotError != tt.wantError || got.Email != tt.wantEmail || got.Name != tt.wantName {
				t.Fatalf("decodeDevLogin() = %#v, %q; want email=%q name=%q error=%q", got, gotError, tt.wantEmail, tt.wantName, tt.wantError)
			}
		})
	}
}
