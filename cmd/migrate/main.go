// Command migrate explicitly creates or upgrades the agent persistence tables.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/logging"
)

func main() {
	logDir := flag.String("log-dir", "", "log directory (default ./logs)")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: migrate\nCreate conversation and agent_run tables using CODEX_MYSQL_URL; safe to repeat.")
	}
	logging.ParseFlags()
	if err := logging.Setup(*logDir, ""); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	if err := migrate(); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	log.Print("[INFO] database migration completed")
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
