package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Health struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// CheckHealth hits baseURL/health with a short timeout. Returns nil on
// any transport error or non-2xx response.
func CheckHealth(baseURL string) *Health {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil
	}
	return &h
}

// WaitForHealthy polls /health every 500ms until status == "ok" or timeout.
func WaitForHealthy(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if h := CheckHealth(baseURL); h != nil && h.Status == "ok" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server did not become healthy within %s", timeout)
		}
		<-tick.C
	}
}
