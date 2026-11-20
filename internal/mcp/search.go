package mcp

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

func rgFiles(ctx context.Context, directory string, args []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = directory
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, commandError("rg file listing failed", err, "")
		}
	}
	if len(output) == 0 {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"), nil
}

func searchableFiles(ctx context.Context, directory, path string, fileOnly bool) (map[string]bool, error) {
	args := []string{"--files", "--null", "--hidden", "--no-follow", "--no-require-git", "--glob", "!.git", "--glob", "!**/.git/**"}
	if fileOnly {
		args = append(args, "--maxdepth", "0")
		path = "./" + path
	}
	paths, err := rgFiles(ctx, directory, append(args, "--", path))
	if err != nil {
		return nil, err
	}
	files := make(map[string]bool, len(paths))
	for _, path := range paths {
		files[filepath.Clean(path)] = true
	}
	return files, nil
}
