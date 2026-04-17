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
}

func GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
