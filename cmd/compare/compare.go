package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type runner struct {
	name      string
	opts      options
	env       *environment
	ts        *target
	goTarget  *target
	result    runResult
	startedAt time.Time
}

type target struct {
	name       string
	baseURL    string
	client     *http.Client
	identities map[string]string
	reverse    map[string]string
}

type requestSpec struct {
	Method      string
	Path        string
	Body        []byte
	ContentType string
	Headers     map[string]string
	Auth        string
	Cookie      string
	WantStatus  int
	Binds       []binding
	Frames      bool
	Allow       string
	CheckJSON   func(any) []string
}

type binding struct {
	Path        string
	Placeholder string
}

type wireResponse struct {
	Status int
	Header http.Header
	Body   []byte
	JSON   any
}

type responsePair struct {
	TS wireResponse
	Go wireResponse
	OK bool
}

func newRunner(name string, opts options, env *environment) *runner {
	return &runner{
		name: name, opts: opts, env: env, startedAt: time.Now(),
		ts:       &target{name: "ts", baseURL: fmt.Sprintf("http://127.0.0.1:%d", opts.tsPort), client: &http.Client{Timeout: 3 * time.Minute}, identities: map[string]string{}, reverse: map[string]string{}},
		goTarget: &target{name: "go", baseURL: fmt.Sprintf("http://127.0.0.1:%d", opts.goPort), client: &http.Client{Timeout: 3 * time.Minute}, identities: map[string]string{}, reverse: map[string]string{}},
	}
}

func (r *runner) enabled(name string) bool {
	return len(r.opts.scenarios) == 0 || r.opts.scenarios[name]
}

func (r *runner) step(ctx context.Context, scenario, name string, spec requestSpec) responsePair {
	label := r.name + "/" + scenario + "/" + name
	r.result.Steps++
	return r.stepNamed(ctx, label, spec)
}

func (r *runner) stepNamed(ctx context.Context, label string, spec requestSpec) responsePair {
	type namedOutcome struct {
		name     string
		response wireResponse
		err      error
	}
	results := make(chan namedOutcome, 2)
	for _, tgt := range []*target{r.ts, r.goTarget} {
		go func(t *target) {
			response, err := t.do(ctx, spec)
			results <- namedOutcome{name: t.name, response: response, err: err}
		}(tgt)
	}
	var pair responsePair
	var requestErrors []string
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			requestErrors = append(requestErrors, outcome.name+": "+outcome.err.Error())
		}
		if outcome.name == "ts" {
			pair.TS = outcome.response
		} else {
			pair.Go = outcome.response
		}
	}
	if len(requestErrors) != 0 {
		r.diverge(label, strings.Join(requestErrors, "; "))
		return pair
	}

	var problems []string
	if spec.WantStatus != 0 {
		if pair.TS.Status != spec.WantStatus || pair.Go.Status != spec.WantStatus {
			problems = append(problems, fmt.Sprintf("expected status %d, got ts=%d go=%d", spec.WantStatus, pair.TS.Status, pair.Go.Status))
		}
	}
	if pair.TS.Status != pair.Go.Status {
		problems = append(problems, fmt.Sprintf("status ts=%d go=%d", pair.TS.Status, pair.Go.Status))
	}
	if tsType, goType := mediaType(pair.TS.Header.Get("Content-Type")), mediaType(pair.Go.Header.Get("Content-Type")); tsType != goType {
		problems = append(problems, fmt.Sprintf("Content-Type ts=%q go=%q", tsType, goType))
	}
	for _, header := range []string{"Retry-After", "WWW-Authenticate"} {
		tsPresent, goPresent := pair.TS.Header.Get(header) != "", pair.Go.Header.Get(header) != ""
		if tsPresent != goPresent {
			problems = append(problems, fmt.Sprintf("%s presence ts=%t go=%t", header, tsPresent, goPresent))
		}
	}
	if tsCookie, goCookie := cookieShape(pair.TS), cookieShape(pair.Go); !reflect.DeepEqual(tsCookie, goCookie) {
		problems = append(problems, fmt.Sprintf("Set-Cookie shape ts=%v go=%v", tsCookie, goCookie))
	}

	for _, bind := range spec.Binds {
		for _, item := range []struct {
			target   *target
			response *wireResponse
		}{{r.ts, &pair.TS}, {r.goTarget, &pair.Go}} {
			value, ok := jsonPath(item.response.JSON, bind.Path)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s response missing bind path %s", item.target.name, bind.Path))
				continue
			}
			text, ok := value.(string)
			if !ok || text == "" {
				problems = append(problems, fmt.Sprintf("%s bind path %s is not a non-empty string (%T)", item.target.name, bind.Path, value))
				continue
			}
			if err := item.target.bind(bind.Placeholder, text); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}

	if spec.Frames {
		tsFrames, tsErr := decodeDownloadFrames(pair.TS.Body)
		goFrames, goErr := decodeDownloadFrames(pair.Go.Body)
		if tsErr != nil || goErr != nil {
			problems = append(problems, fmt.Sprintf("frame decode ts=%v go=%v", tsErr, goErr))
		} else if !reflect.DeepEqual(normalizeFrames(tsFrames, r.ts), normalizeFrames(goFrames, r.goTarget)) {
			problems = append(problems, fmt.Sprintf("binary frames differ ts=%v go=%v", summarizeFrames(tsFrames), summarizeFrames(goFrames)))
		}
	} else if pair.TS.JSON != nil || pair.Go.JSON != nil {
		for _, item := range []struct {
			name  string
			value any
		}{{"ts", pair.TS.JSON}, {"go", pair.Go.JSON}} {
			for _, issue := range validateTimestamps(item.value, r.startedAt) {
				problems = append(problems, item.name+" "+issue)
			}
		}
		tsJSON := normalizeJSON(pair.TS.JSON, r.ts, r.startedAt)
		goJSON := normalizeJSON(pair.Go.JSON, r.goTarget, r.startedAt)
		if !reflect.DeepEqual(tsJSON, goJSON) {
			problems = append(problems, fmt.Sprintf("JSON differs ts=%s go=%s", compactJSON(tsJSON), compactJSON(goJSON)))
		}
		if spec.CheckJSON != nil {
			for _, item := range []struct {
				name  string
				value any
			}{{"ts", pair.TS.JSON}, {"go", pair.Go.JSON}} {
				for _, issue := range spec.CheckJSON(item.value) {
					problems = append(problems, item.name+" "+issue)
				}
			}
		}
	} else if !bytes.Equal(pair.TS.Body, pair.Go.Body) {
		problems = append(problems, fmt.Sprintf("body differs ts=%s go=%s", preview(pair.TS.Body), preview(pair.Go.Body)))
	}

	if len(problems) == 0 {
		pair.OK = true
		r.result.Matched++
		fmt.Printf("PASS  %s\n", label)
	} else if reason, ok := matchAcceptedDeviation(spec.Allow, problems, pair); ok {
		pair.OK = true
		r.result.Accepted = append(r.result.Accepted, label+": "+reason+" (observed: "+strings.Join(problems, "; ")+")")
		fmt.Printf("ALLOW %s\n", label)
	} else {
		r.diverge(label, strings.Join(problems, "; "))
	}
	return pair
}

const (
	allowCapabilityVersion      = "capability-version"
	allowMalformedAuthorization = "malformed-authorization"
	allowTombstoneRedelete      = "tombstone-redelete"
	allowLegacyMutationID       = "legacy-mutation-id"
)

// matchAcceptedDeviation deliberately checks the complete expected shape and
// problem count. An allowlist entry must not hide a new header, status, body,
// timestamp, or transport divergence that happens at the same step.
func matchAcceptedDeviation(kind string, problems []string, pair responsePair) (string, bool) {
	switch kind {
	case allowCapabilityVersion:
		if len(problems) != 1 || !strings.HasPrefix(problems[0], "JSON differs ") ||
			pair.TS.Status != http.StatusOK || pair.Go.Status != http.StatusOK ||
			!capabilitiesDifferOnlyByVersion(pair.TS.JSON, pair.Go.JSON) {
			return "", false
		}
		return "accepted implementation version difference (docs/Rewriting the server in Go.md §How Authentication Works)", true
	case allowMalformedAuthorization:
		if len(problems) != 2 || pair.TS.Status != http.StatusUnauthorized || pair.Go.Status != http.StatusUnauthorized ||
			pair.TS.Header.Get("WWW-Authenticate") == "" || pair.Go.Header.Get("WWW-Authenticate") != "" ||
			!reflect.DeepEqual(pair.TS.JSON, decodeStrict([]byte(`{"error":"session expired or invalid","code":"invalid_session"}`))) ||
			!reflect.DeepEqual(pair.Go.JSON, decodeStrict([]byte(`{"error":"unauthorized"}`))) {
			return "", false
		}
		return "accepted malformed Authorization behavior (docs/Rewriting the server in Go.md §How Authentication Works)", true
	case allowTombstoneRedelete:
		if len(problems) != 1 || pair.TS.Status != http.StatusOK || pair.Go.Status != http.StatusOK {
			return "", false
		}
		tsVersion, tsOK := jsonInt(pair.TS.JSON, "object.version")
		goVersion, goOK := jsonInt(pair.Go.JSON, "object.version")
		tsCollection, tsCollectionOK := jsonInt(pair.TS.JSON, "collectionVersion")
		goCollection, goCollectionOK := jsonInt(pair.Go.JSON, "collectionVersion")
		tsDeleted, _ := jsonPath(pair.TS.JSON, "object.deleted")
		goDeleted, _ := jsonPath(pair.Go.JSON, "object.deleted")
		if !tsOK || !goOK || !tsCollectionOK || !goCollectionOK || tsVersion != goVersion+1 ||
			tsCollection != goCollection+1 || tsDeleted != true || goDeleted != true {
			return "", false
		}
		return "accepted no-op tombstone behavior (docs/Rewriting the server in Go.md §Delete)", true
	case allowLegacyMutationID:
		goError, _ := jsonPath(pair.Go.JSON, "error")
		tsDeleted, _ := jsonPath(pair.TS.JSON, "object.deleted")
		if len(problems) != 2 || pair.TS.Status != http.StatusCreated || pair.Go.Status != http.StatusBadRequest ||
			goError != "invalid Mutation-Id" || tsDeleted != false {
			return "", false
		}
		return "accepted legacy Mutation-Id syntax behavior (docs/Rewriting the server in Go.md §Mutation ID format)", true
	default:
		return "", false
	}
}

func capabilitiesDifferOnlyByVersion(tsValue, goValue any) bool {
	tsCapability, tsOK := tsValue.(map[string]any)
	goCapability, goOK := goValue.(map[string]any)
	if !tsOK || !goOK {
		return false
	}
	tsVersion, tsVersionOK := tsCapability["version"].(string)
	goVersion, goVersionOK := goCapability["version"].(string)
	if !tsVersionOK || !goVersionOK || tsVersion == "" || goVersion == "" || tsVersion == goVersion {
		return false
	}
	tsWithoutVersion := make(map[string]any, len(tsCapability)-1)
	goWithoutVersion := make(map[string]any, len(goCapability)-1)
	for key, value := range tsCapability {
		if key != "version" {
			tsWithoutVersion[key] = value
		}
	}
	for key, value := range goCapability {
		if key != "version" {
			goWithoutVersion[key] = value
		}
	}
	return reflect.DeepEqual(tsWithoutVersion, goWithoutVersion)
}

func jsonInt(value any, path string) (int64, bool) {
	raw, ok := jsonPath(value, path)
	if !ok {
		return 0, false
	}
	switch typed := raw.(type) {
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(typed, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func validateTimestamps(value any, startedAt time.Time) []string {
	var issues []string
	var walk func(any, string, string)
	walk = func(current any, path, key string) {
		switch typed := current.(type) {
		case map[string]any:
			for childKey, child := range typed {
				childPath := childKey
				if path != "" {
					childPath = path + "." + childKey
				}
				walk(child, childPath, childKey)
			}
		case []any:
			for index, child := range typed {
				walk(child, fmt.Sprintf("%s.%d", path, index), key)
			}
		case string:
			if !strings.HasSuffix(key, "_at") && !strings.HasSuffix(key, "At") {
				return
			}
			parsed, err := time.Parse(time.RFC3339Nano, typed)
			if err != nil {
				issues = append(issues, fmt.Sprintf("timestamp %s is not RFC3339: %q", path, typed))
				return
			}
			if parsed.Before(startedAt.Add(-10*time.Second)) || parsed.After(time.Now().Add(10*time.Second)) {
				issues = append(issues, fmt.Sprintf("timestamp %s is outside run window: %q", path, typed))
			}
		}
	}
	walk(value, "", "")
	return issues
}

func (r *runner) diverge(label, problem string) {
	r.result.Divergences = append(r.result.Divergences, label+": "+problem)
	fmt.Printf("DIFF  %s: %s\n", label, problem)
}

func (t *target) do(ctx context.Context, spec requestSpec) (wireResponse, error) {
	path := t.render(spec.Path)
	body := []byte(t.render(string(spec.Body)))
	request, err := http.NewRequestWithContext(ctx, spec.Method, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return wireResponse{}, err
	}
	if spec.ContentType != "" {
		request.Header.Set("Content-Type", spec.ContentType)
	}
	for key, value := range spec.Headers {
		request.Header.Set(key, t.render(value))
	}
	if spec.Auth != "" {
		request.Header.Set("Authorization", "Bearer "+t.render(spec.Auth))
	}
	if spec.Cookie != "" {
		request.AddCookie(&http.Cookie{Name: "session", Value: t.render(spec.Cookie)})
	}
	response, err := t.client.Do(request)
	if err != nil {
		return wireResponse{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return wireResponse{}, err
	}
	result := wireResponse{Status: response.StatusCode, Header: response.Header.Clone(), Body: data}
	if mediaType(response.Header.Get("Content-Type")) == "application/json" && len(bytes.TrimSpace(data)) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&result.JSON); err != nil {
			return result, fmt.Errorf("decoding JSON response: %w (body %s)", err, preview(data))
		}
	}
	return result, nil
}

func (t *target) render(value string) string {
	keys := make([]string, 0, len(t.identities))
	for key := range t.identities {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		value = strings.ReplaceAll(value, key, t.identities[key])
	}
	return value
}

func (t *target) bind(placeholder, actual string) error {
	if old, exists := t.identities[placeholder]; exists && old != actual {
		return fmt.Errorf("%s placeholder %s changed from %q to %q", t.name, placeholder, old, actual)
	}
	if old, exists := t.reverse[actual]; exists && old != placeholder {
		return fmt.Errorf("%s value %q is already bound to %s", t.name, actual, old)
	}
	t.identities[placeholder] = actual
	t.reverse[actual] = placeholder
	return nil
}

func decodeStrict(data []byte) any {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

func normalizeJSON(value any, target *target, startedAt time.Time) any {
	var walk func(any, string, string) any
	walk = func(current any, path, key string) any {
		switch typed := current.(type) {
		case map[string]any:
			out := make(map[string]any, len(typed))
			for childKey, child := range typed {
				childPath := childKey
				if path != "" {
					childPath = path + "." + childKey
				}
				out[childKey] = walk(child, childPath, childKey)
			}
			return out
		case []any:
			out := make([]any, len(typed))
			for index, child := range typed {
				childPath := strconv.Itoa(index)
				if path != "" {
					childPath = path + "." + childPath
				}
				out[index] = walk(child, childPath, key)
			}
			return out
		case string:
			if normalized := normalizeIdentityString(typed, target); normalized != typed {
				return normalized
			}
			if strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "At") {
				if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil && parsed.After(startedAt.Add(-10*time.Second)) && parsed.Before(time.Now().Add(10*time.Second)) {
					return "<timestamp>"
				}
			}
			return typed
		default:
			return current
		}
	}
	return walk(value, "", "")
}

func normalizeIdentityString(value string, target *target) string {
	if placeholder, ok := target.reverse[value]; ok {
		return placeholder
	}
	actuals := make([]string, 0, len(target.reverse))
	for actual := range target.reverse {
		actuals = append(actuals, actual)
	}
	sort.Slice(actuals, func(i, j int) bool { return len(actuals[i]) > len(actuals[j]) })
	for _, actual := range actuals {
		value = strings.ReplaceAll(value, actual, target.reverse[actual])
	}
	return value
}

func jsonPath(value any, path string) (any, bool) {
	current := value
	for _, part := range strings.Split(path, ".") {
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func mediaType(value string) string {
	result, _, err := mime.ParseMediaType(value)
	if err != nil {
		return value
	}
	return result
}

func cookieShape(response wireResponse) []string {
	fake := &http.Response{Header: response.Header}
	shapes := []string{}
	for _, cookie := range fake.Cookies() {
		maxAge := "none"
		if cookie.MaxAge > 0 {
			maxAge = "positive"
		} else if cookie.MaxAge < 0 {
			maxAge = "delete"
		}
		shapes = append(shapes, fmt.Sprintf("%s:path=%s:max-age=%s:http-only=%t:secure=%t:same-site=%d", cookie.Name, cookie.Path, maxAge, cookie.HttpOnly, cookie.Secure, cookie.SameSite))
	}
	sort.Strings(shapes)
	return shapes
}

func compactJSON(value any) string {
	data, _ := json.Marshal(value)
	return preview(data)
}

func preview(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) > 600 {
		return fmt.Sprintf("%q…(%d bytes)", data[:600], len(data))
	}
	return fmt.Sprintf("%q", data)
}
