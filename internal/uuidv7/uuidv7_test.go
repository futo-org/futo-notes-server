package uuidv7

import (
	"regexp"
	"testing"
)

func TestNew(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := New()
		if !pattern.MatchString(id) {
			t.Fatalf("not a v7 UUID: %s", id)
		}
		if seen[id] {
			t.Fatalf("duplicate UUID: %s", id)
		}
		seen[id] = true
	}
}
