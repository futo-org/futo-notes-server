// Package db owns the Postgres connection.
package db

import (
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"

	"futo-notes-server/internal/config"
)

func Open(cfg config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.DBPoolMax)
	db.SetConnMaxIdleTime(cfg.DBPoolIdleTimeout)
	return db, nil
}
