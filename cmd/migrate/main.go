// Command migrate explicitly creates or upgrades the agent persistence tables.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/collie-agent/codex-go/internal/database"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: migrate\nCreate conversation and agent_run tables using CODEX_MYSQL_URL; safe to repeat.")
	}
	flag.Parse()
	if err := migrate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "database migration completed")
}

func migrate() error {
	ctx := context.Background()
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return err
	}
	defer database.Close(db)
	return database.Migrate(ctx, db)
}
