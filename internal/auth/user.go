package auth

import (
	"context"
	"database/sql"
	"errors"
	"uuid"
)

// The singleton user in password mode. All E2EE data is owned by this row.
const (
	singletonSub   = "local"
	singletonEmail = "local@futo-notes.local"
	singletonName  = "FUTO Notes"
)

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// UpsertLocalUser returns the singleton user, creating it on first login.
// ON CONFLICT DO NOTHING plus a re-select keeps two concurrent first logins
// from failing on the sub unique constraint.
func UpsertLocalUser(ctx context.Context, database *sql.DB) (User, error) {
	var u User
	err := database.QueryRowContext(ctx,
		`SELECT id, email, name FROM users WHERE sub = $1`, singletonSub).
		Scan(&u.ID, &u.Email, &u.Name)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}

	err = database.QueryRowContext(ctx,
		`INSERT INTO users (id, sub, email, name) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (sub) DO NOTHING
		 RETURNING id, email, name`,
		uuid.NewV7().String(), singletonSub, singletonEmail, singletonName).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		// Lost the race; the row exists now.
		err = database.QueryRowContext(ctx,
			`SELECT id, email, name FROM users WHERE sub = $1`, singletonSub).
			Scan(&u.ID, &u.Email, &u.Name)
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
