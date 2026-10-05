package main

import (
	"testing"
	"time"
)

func TestRunRequiresWorkspace(t *testing.T) {
	if err := run("", 30*time.Second); err == nil {
		t.Fatal("expected missing workspace to fail")
	}
}

func TestRunRejectsMissingWorkspaceDirectory(t *testing.T) {
	if err := run(t.TempDir()+"/missing", 30*time.Second); err == nil {
		t.Fatal("expected invalid workspace to fail")
	}
}
