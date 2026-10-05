package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type symlinkPolicy bool

const (
	rejectSymlinks  symlinkPolicy = false
	resolveSymlinks symlinkPolicy = true
)

var errSymlink = errors.New("path is a symbolic link")

func resolveWorkspace(workspace string) (string, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory")
	}
	return root, nil
}

func relativeWorkspacePath(root, target string) (string, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside workspace")
	}
	return filepath.ToSlash(rel), nil
}

// resolveWorkspacePath enforces the workspace boundary. Missing components are
// allowed only when preparing patch targets; links are resolved only for tools
// whose contract permits reading their contained targets.
func resolveWorkspacePath(workspace, path string, links symlinkPolicy, allowMissing bool) (string, string, error) {
	if filepath.IsAbs(path) {
		return "", "", fmt.Errorf("absolute paths are not allowed")
	}
	root, err := resolveWorkspace(workspace)
	if err != nil {
		return "", "", err
	}
	target := filepath.Join(root, path)
	rel, err := relativeWorkspacePath(root, target)
	if err != nil {
		return "", "", err
	}
	if links == resolveSymlinks {
		target, err = filepath.EvalSymlinks(target)
		if err != nil {
			return "", "", err
		}
		rel, err = relativeWorkspacePath(root, target)
		return target, rel, err
	}
	current := root
	for _, part := range strings.Split(filepath.FromSlash(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if allowMissing && os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", errSymlink
		}
	}
	return target, rel, nil
}

func isGitPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}
