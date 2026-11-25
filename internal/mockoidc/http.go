package mockoidc

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"

	"github.com/rs/zerolog/log"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error().Err(err).Msg("write response")
	}
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

type loginPageData struct {
	Action string
	Params url.Values
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mock OIDC sign-in</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 24rem; margin: 4rem auto; padding: 0 1rem; }
label { display: block; margin-top: 1rem; }
input { width: 100%; padding: .4rem; box-sizing: border-box; }
button { margin-top: 1.5rem; padding: .5rem 1rem; }
p { color: #666; }
</style>
</head>
<body>
<h1>Mock OIDC sign-in</h1>
<p>Development issuer. Any subject signs in; email and name default from it.</p>
<form method="post" action="{{.Action}}">
{{- range $k, $vs := .Params}}{{range $vs}}
<input type="hidden" name="{{$k}}" value="{{.}}">
{{- end}}{{end}}
<label>Subject (sub) <input name="sub" required autofocus value="dev-user"></label>
<label>Email <input name="email" type="email" placeholder="dev-user@example.test"></label>
<label>Name <input name="name" placeholder="dev-user"></label>
<label><input name="email_verified" type="checkbox" value="false" style="width: auto"> Email not verified</label>
<button type="submit">Sign in</button>
</form>
</body>
</html>
`))
