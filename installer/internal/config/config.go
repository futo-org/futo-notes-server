package config

import (
	"crypto/rand"
	"encoding/hex"
)

const (
	DefaultPort     = 3005
	DefaultDataPath = "./stonefruit-data"
)

type Config struct {
	Port             int
	DataPath         string
	PostgresPassword string
	// Server password hash (scrypt format). Written to sibling .env;
	// NOT inlined in docker-compose.yml so it can be rotated without
	// rewriting compose.
	PasswordHash string
	// Release track: "stable" (tagged releases) or "latest" (main branch
	// rolling). Defaults to stable on fresh installs; switched via
	// `stonefruit release <track>`.
	Track string
}

func GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
