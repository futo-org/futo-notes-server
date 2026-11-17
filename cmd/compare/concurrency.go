package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	concurrencyMutations   = 32
	concurrencyJobRequests = 8
	concurrencySubscribers = 3
)

type sqliteHammerIdentity struct {
	userID       string
	token        string
	collectionID string
}

// scenarioSQLiteConcurrency hammers the client-visible SQLite boundary while
// maintenance jobs and SSE subscribers are active. A busy error hidden behind
// a generic 500 still fails because every operation has an exact success code.
func (r *runner) scenarioSQLiteConcurrency(ctx context.Context) {
	label := r.name + "/concurrency/sqlite mutations, SSE, reconciliation, and GC"
	r.result.Steps++
	target := r.goTarget

	fail := func(err error) {
		r.diverge(label, err.Error())
	}
	identity, err := r.prepareSQLiteConcurrency(ctx, target)
	if err != nil {
		fail(err)
		return
	}

	collectors := make([]*sseCollector, 0, concurrencySubscribers)
	for index := range concurrencySubscribers {
		collector, err := openSSE(ctx, target, identity.token)
		if err != nil {
			for _, opened := range collectors {
				opened.close()
			}
			fail(fmt.Errorf("opening SSE subscriber %d: %w", index, err))
			return
		}
		collectors = append(collectors, collector)
	}
	defer func() {
		for _, collector := range collectors {
			collector.close()
		}
	}()

	start := make(chan struct{})
	errorsFound := make(chan error, concurrencyMutations+concurrencyJobRequests)
	var workers sync.WaitGroup
	for index := range concurrencyMutations {
		workers.Go(func() {
			<-start
			response, err := target.do(ctx, requestSpec{
				Method: http.MethodPost, Path: "/api/collections/" + identity.collectionID + "/blob-objects",
				Body:        fmt.Appendf(nil, "concurrent encrypted payload %02d", index),
				ContentType: "application/octet-stream", Auth: identity.token,
				Headers: map[string]string{"Mutation-Id": fmt.Sprintf("sqlite-concurrent-%02d", index)},
			})
			if err != nil {
				errorsFound <- fmt.Errorf("mutation %02d transport: %w", index, err)
				return
			}
			if response.Status != http.StatusCreated || containsLocked(response.Body) {
				errorsFound <- fmt.Errorf("mutation %02d returned %d: %s", index, response.Status, preview(response.Body))
			}
		})
	}
	for index := range concurrencyJobRequests {
		workers.Go(func() {
			<-start
			job := "reconciliation"
			if index%2 == 1 {
				job = "blob-gc"
			}
			response, err := target.do(ctx, requestSpec{Method: http.MethodPost, Path: "/dev/jobs/" + job})
			if err != nil {
				errorsFound <- fmt.Errorf("%s request %d transport: %w", job, index, err)
				return
			}
			if response.Status != http.StatusOK || containsLocked(response.Body) {
				errorsFound <- fmt.Errorf("%s request %d returned %d: %s", job, index, response.Status, preview(response.Body))
			}
		})
	}
	close(start)
	workers.Wait()
	close(errorsFound)

	var problems []string
	for err := range errorsFound {
		problems = append(problems, err.Error())
	}
	for index, collector := range collectors {
		state, err := collector.waitState(10 * time.Second)
		if err != nil {
			problems = append(problems, fmt.Sprintf("SSE subscriber %d: %v", index, err))
			continue
		}
		if state[identity.collectionID] == 0 {
			problems = append(problems, fmt.Sprintf("SSE subscriber %d saw no collection change: %v", index, state))
		}
	}
	if len(problems) != 0 {
		fail(errors.New(strings.Join(problems, "; ")))
		return
	}
	r.result.Matched++
	fmt.Printf("PASS  %s\n", label)
}

func (r *runner) prepareSQLiteConcurrency(ctx context.Context, target *target) (sqliteHammerIdentity, error) {
	var identity sqliteHammerIdentity
	login, err := target.do(ctx, jsonRequest(http.MethodPost, "/api/auth/dev/login",
		`{"email":"sqlite-concurrency@example.invalid","name":"SQLite Concurrency"}`, "", http.StatusOK))
	if err := requireStatus(login, err, http.StatusOK); err != nil {
		return identity, fmt.Errorf("logging in concurrency user: %w", err)
	}
	// A missing path yields nil, which fails the string assertion.
	userID, _ := jsonPath(login.JSON, "user.id")
	token, _ := jsonPath(login.JSON, "token")
	var userOK, tokenOK bool
	identity.userID, userOK = userID.(string)
	identity.token, tokenOK = token.(string)
	if !userOK || !tokenOK || identity.userID == "" || identity.token == "" {
		return identity, errors.New("concurrency login returned incomplete identity")
	}
	claimed, err := target.do(ctx, requestSpec{Method: http.MethodPost, Path: "/api/collections", Auth: identity.token})
	if err := requireStatus(claimed, err, http.StatusCreated); err != nil {
		return identity, fmt.Errorf("claiming concurrency collection: %w", err)
	}
	collectionID, _ := jsonPath(claimed.JSON, "collection.id")
	var collectionOK bool
	identity.collectionID, collectionOK = collectionID.(string)
	if !collectionOK || identity.collectionID == "" {
		return identity, errors.New("concurrency claim returned no collection ID")
	}

	orphanDir := filepath.Join(r.env.goBlob, identity.userID)
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		return identity, fmt.Errorf("creating reconciliation fixtures: %w", err)
	}
	for index := range concurrencyJobRequests {
		id := fmt.Sprintf("20000000-0000-7000-8000-%012x", index)
		if err := os.WriteFile(filepath.Join(orphanDir, id), []byte("orphaned encrypted payload"), 0o600); err != nil {
			return identity, fmt.Errorf("writing reconciliation fixture %d: %w", index, err)
		}
	}

	// Create and tombstone real objects so concurrent GC has immediately
	// eligible rows without reaching into SQLite to manufacture state.
	for index := range concurrencyJobRequests {
		created, err := target.do(ctx, requestSpec{
			Method: http.MethodPost, Path: "/api/collections/" + identity.collectionID + "/blob-objects",
			Body:        fmt.Appendf(nil, "gc encrypted payload %02d", index),
			ContentType: "application/octet-stream", Auth: identity.token,
			Headers: map[string]string{"Mutation-Id": fmt.Sprintf("sqlite-gc-create-%02d", index)},
		})
		if err := requireStatus(created, err, http.StatusCreated); err != nil {
			return identity, fmt.Errorf("creating GC fixture %d: %w", index, err)
		}
		objectID, ok := jsonPath(created.JSON, "object.id")
		objectIDText, stringOK := objectID.(string)
		if !ok || !stringOK || objectIDText == "" {
			return identity, fmt.Errorf("creating GC fixture %d returned no object ID", index)
		}
		deleted, err := target.do(ctx, requestSpec{
			Method: http.MethodDelete,
			Path:   "/api/collections/" + identity.collectionID + "/objects/" + objectIDText + "?version=1",
			Auth:   identity.token,
			Headers: map[string]string{
				"Mutation-Id": fmt.Sprintf("sqlite-gc-delete-%02d", index),
			},
		})
		if err := requireStatus(deleted, err, http.StatusOK); err != nil {
			return identity, fmt.Errorf("deleting GC fixture %d: %w", index, err)
		}
	}
	return identity, nil
}

// requireStatus fails a setup request that hit a transport error or answered
// anything but want. Both cases report the status and body that came back.
func requireStatus(response wireResponse, err error, want int) error {
	if err != nil {
		return fmt.Errorf("status=%d body=%s: %w", response.Status, preview(response.Body), err)
	}
	if response.Status != want {
		return fmt.Errorf("status=%d body=%s", response.Status, preview(response.Body))
	}
	return nil
}

func containsLocked(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "database is locked")
}
