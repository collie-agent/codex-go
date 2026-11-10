package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

// RegisterExecDependencyFlags shares the dependency configuration format across
// CLI, consumer and external MCP entries. Explicit [] disables YAML caches.
func RegisterExecDependencyFlags(flags *flag.FlagSet, proxy *string, caches *[]ExecCache) {
	flags.StringVar(proxy, "exec-go-proxy", "", "Go GOPROXY value (default off; use host network for downloads)")
	flags.Func("exec-cache", "persistent cache JSON object (repeatable; [] clears YAML caches)", func(value string) error {
		if strings.TrimSpace(value) == "[]" {
			*caches = []ExecCache{}
			return nil
		}
		var cache ExecCache
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cache); err != nil {
			return fmt.Errorf("invalid exec-cache: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("invalid exec-cache: expected one JSON object")
		}
		*caches = append(*caches, cache)
		return nil
	})
}

// ExecDependencyArgs preserves nil versus explicitly empty cache overrides.
func ExecDependencyArgs(proxy string, caches []ExecCache) []string {
	var args []string
	if proxy != "" {
		args = append(args, "--exec-go-proxy", proxy)
	}
	if caches != nil && len(caches) == 0 {
		args = append(args, "--exec-cache", "[]")
	}
	for _, cache := range caches {
		data, _ := json.Marshal(cache)
		args = append(args, "--exec-cache", string(data))
	}
	return args
}
