package main

import (
	"codex-feishu/internal/feishu"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
)

// Emit only permission-shaped identifiers, never the raw application payload.
func checkScopes(ctx context.Context, g *feishu.Gateway) {
	raw, err := g.Scopes(ctx)
	if err != nil {
		reportError(err)
		os.Exit(4)
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		os.Exit(4)
	}
	pattern := regexp.MustCompile(`^[a-z][a-z0-9_.]*(:[a-z0-9_.]+)+$`)
	ids := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			if pattern.MatchString(v) {
				ids[v] = true
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		case map[string]any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(data)
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"application_scope_ids": result, "api_capability_verified": false}); err != nil {
		os.Exit(4)
	}
}
