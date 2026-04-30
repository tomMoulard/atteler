package llm

import (
	"context"

	"github.com/tommoulard/atteler/pkg/events"
)

func emitActivity(ctx context.Context, event events.Event) {
	if err := events.EmitFromContext(ctx, event); err == nil {
		return
	}
}
