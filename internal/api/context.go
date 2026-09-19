package api

import (
	"context"

	"samplechain/internal/store"
)

func contextWithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}
