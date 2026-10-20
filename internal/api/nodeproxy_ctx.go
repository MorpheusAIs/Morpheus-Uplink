package api

import "context"

type nodePathKey struct{}

func contextWithNodePath(ctx context.Context, p string) context.Context {
	return context.WithValue(ctx, nodePathKey{}, p)
}
