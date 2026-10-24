package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseURL(t *testing.T) {
	cfg, err := parseURL("mysql://codex:p%40ss%3Aword@[::1]/codex?timeout=2s&loc=Asia%2FShanghai")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "codex" || cfg.Passwd != "p@ss:word" || cfg.Addr != "[::1]:3306" || cfg.DBName != "codex" {
		t.Fatal("URL fields were not decoded correctly")
	}
	if !cfg.ParseTime || cfg.Timeout != 2*time.Second || cfg.Loc.String() != "Asia/Shanghai" || cfg.Params["charset"] != "utf8mb4" {
		t.Fatal("unexpected MySQL connection options")
	}
}

func TestParseURLRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{
		"", "postgres://user:secret@localhost/db", "mysql://localhost/db",
		"mysql://user:secret@localhost", "mysql://user:secret@localhost/a/b",
		"mysql://user:secret@localhost:bad/db", "mysql://user:secret%zz@localhost/db",
		"mysql://user:secret@localhost/db?timeout=bad",
		"mysql://user:secret@localhost/db?timeout=1s&timeout=2s",
	} {
		_, err := parseURL(input)
		if err == nil {
			t.Fatal("invalid URL was accepted")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error contains credentials")
		}
	}
}

func TestOpenFromEnvRequiresURL(t *testing.T) {
	t.Setenv(MySQLURLEnv, "")
	if _, err := OpenFromEnv(context.Background()); err == nil {
		t.Fatal("expected missing configuration error")
	}
}

func TestMySQLIntegration(t *testing.T) {
	if os.Getenv(MySQLURLEnv) == "" {
		t.Skip("set CODEX_MYSQL_URL to run the MySQL connection check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := OpenFromEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Close(db); err != nil {
			t.Error(err)
		}
	})
	var result int
	if err := db.WithContext(ctx).Raw("SELECT 1").Scan(&result).Error; err != nil {
		t.Fatal(err)
	}
	if result != 1 {
		t.Fatalf("SELECT 1 returned %d", result)
	}
}
