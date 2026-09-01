package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sseCollector struct {
	cancel context.CancelFunc
	body   interface{ Close() error }
	target *target
	mu     sync.Mutex
	state  map[string]int64
	wake   chan struct{}
	err    error
}

func openSSE(parent context.Context, target *target, token string) (*sseCollector, error) {
	ctx, cancel := context.WithCancel(parent)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.baseURL+"/api/sync/events", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+target.render(token))
	response, err := target.client.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	if response.StatusCode != http.StatusOK || mediaType(response.Header.Get("Content-Type")) != "text/event-stream" {
		response.Body.Close()
		cancel()
		return nil, fmt.Errorf("status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	collector := &sseCollector{cancel: cancel, body: response.Body, target: target, state: map[string]int64{}, wake: make(chan struct{}, 1)}
	ready := make(chan error, 1)
	go collector.read(response.Body, ready)
	select {
	case err := <-ready:
		if err != nil {
			collector.close()
			return nil, err
		}
		return collector, nil
	case <-time.After(3 * time.Second):
		collector.close()
		return nil, fmt.Errorf("timed out waiting for ready event")
	}
}

func (c *sseCollector) read(body interface{ Read([]byte) (int, error) }, ready chan<- error) {
	scanner := bufio.NewScanner(body)
	event := ""
	data := ""
	readySent := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event == "ready" && !readySent {
				ready <- nil
				readySent = true
			}
			if event == "change" && data != "" {
				c.recordChange(data)
			}
			event, data = "", ""
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(value)
		}
	}
	if !readySent {
		ready <- scanner.Err()
	}
	c.mu.Lock()
	c.err = scanner.Err()
	c.mu.Unlock()
}

func (c *sseCollector) recordChange(data string) {
	var change struct {
		CollectionID   string      `json:"collectionId"`
		CurrentVersion json.Number `json:"currentVersion"`
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&change) != nil {
		return
	}
	version, err := strconv.ParseInt(string(change.CurrentVersion), 10, 64)
	if err != nil {
		return
	}
	if placeholder, ok := c.target.reverse[change.CollectionID]; ok {
		change.CollectionID = placeholder
	}
	c.mu.Lock()
	if version > c.state[change.CollectionID] {
		c.state[change.CollectionID] = version
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *sseCollector) waitState(timeout time.Duration) (map[string]int64, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		state := make(map[string]int64, len(c.state))
		for key, value := range c.state {
			state[key] = value
		}
		err := c.err
		c.mu.Unlock()
		if len(state) != 0 || err != nil {
			return state, err
		}
		select {
		case <-c.wake:
		case <-timer.C:
			return state, fmt.Errorf("timed out waiting for change event")
		}
	}
}

func (c *sseCollector) close() {
	c.cancel()
	_ = c.body.Close()
}
