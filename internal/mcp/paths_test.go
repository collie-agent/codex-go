package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedPathResolverEnforcesToolLinkPolicies(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	writeFixture(t, workspace, "dir/file.txt", "inside\n")
	writeFixture(t, outside, "file.txt", "outside\n")
	for name, target := range map[string]string{
		"inside-link": "dir/file.txt", "outside-link": filepath.Join(outside, "file.txt"),
		"inside-dir": "dir", "outside-dir": outside,
	} {
		if err := os.Symlink(target, filepath.Join(workspace, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, path string
		links      symlinkPolicy
		missing    bool
		want       string
	}{
		{"ordinary file", "dir/file.txt", rejectSymlinks, false, "dir/file.txt"},
		{"contained parent", "dir/../dir/file.txt", rejectSymlinks, false, "dir/file.txt"},
		{"contained file link", "inside-link", resolveSymlinks, false, "dir/file.txt"},
		{"contained parent link", "inside-dir/file.txt", resolveSymlinks, false, "dir/file.txt"},
		{"new nested patch target", "new/nested/file.txt", rejectSymlinks, true, "new/nested/file.txt"},
		{"read missing", "new/nested/file.txt", resolveSymlinks, false, ""},
		{"search missing", "new/nested/file.txt", rejectSymlinks, false, ""},
		{"search file link", "inside-link", rejectSymlinks, false, ""},
		{"search parent link", "inside-dir/file.txt", rejectSymlinks, false, ""},
		{"read outside link", "outside-link", resolveSymlinks, false, ""},
		{"read outside parent", "outside-dir/file.txt", resolveSymlinks, false, ""},
		{"patch outside parent", "outside-dir/new.txt", rejectSymlinks, true, ""},
		{"patch contained parent link", "inside-dir/new.txt", rejectSymlinks, true, ""},
		{"parent escape", "../file.txt", rejectSymlinks, true, ""},
		{"absolute path", filepath.Join(workspace, "dir/file.txt"), resolveSymlinks, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full, rel, err := resolveWorkspacePath(workspace, tc.path, tc.links, tc.missing)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unexpected accepted path %q", full)
				}
				return
			}
			if err != nil || rel != tc.want || full != filepath.Join(workspace, filepath.FromSlash(tc.want)) {
				t.Fatalf("full=%q, rel=%q, err=%v; want %q", full, rel, err, tc.want)
			}
		})
	}
}
