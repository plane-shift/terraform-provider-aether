package provider

import "context"

// collectAll drains a cursor-paginated list. fetch returns the page items and the next cursor ("" when done).
func collectAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	for {
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || next == cursor {
			return out, nil
		}
		cursor = next
	}
}
