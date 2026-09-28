package provider

import (
	"context"
	"fmt"
)

// collectAll drains a cursor-paginated list. fetch returns the page items and
// the next cursor ("" when done). A cursor the API has already been sent
// (including the initial "") is an ERROR, never a silent stop: a server that
// ignores the cursor would otherwise yield page 1 twice (a by-name lookup
// would then report a duplicate match), and an A->B->A cycle would spin until
// the context or the Terraform timeout ended it.
func collectAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	for {
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if _, dup := seen[next]; dup {
			return nil, fmt.Errorf("the API repeated pagination cursor %q; refusing to loop", next)
		}
		seen[next] = struct{}{}
		cursor = next
	}
}
