package api

import (
	"context"
	"fmt"

	"samplechain/internal/store"
)

func contextWithPerson(ctx context.Context, p store.Person) context.Context {
	return context.WithValue(ctx, personKey, p)
}

func personFrom(ctx context.Context) store.Person {
	p, _ := ctx.Value(personKey).(store.Person)
	return p
}

func sprintfF(f string, a ...any) string { return fmt.Sprintf(f, a...) }
