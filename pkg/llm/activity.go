package llm

import (
	"context"
	"sync"

	"github.com/tommoulard/atteler/pkg/events"
)

func emitActivity(ctx context.Context, event events.Event) {
	if err := events.EmitFromContext(ctx, event); err == nil {
		return
	}
}

func commandActivityOnce(ctx context.Context, event events.Event) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			emitActivity(ctx, event)
		})
	}
}
