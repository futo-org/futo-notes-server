package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"futo-notes-server/internal/config"
	"futo-notes-server/internal/db"
)

func runMigrateToSQLite(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate-to-sqlite", flag.ContinueOnError)
	flags.SetOutput(stderr)
	targetURL := flags.String("to", db.DefaultSQLiteURL, "destination sqlite: URL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "migrate-to-sqlite does not accept positional arguments")
		return 2
	}
	sourceURL, err := config.LoadRequiredDatabaseURL()
	if err != nil {
		fmt.Fprintf(stderr, "loading source database: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := db.MigratePostgresToSQLite(ctx, sourceURL, *targetURL, stdout); err != nil {
		fmt.Fprintf(stderr, "migrate-to-sqlite: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "SQLite copy complete: %s\n", *targetURL)
	return 0
}
