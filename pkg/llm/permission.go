package llm

import (
	"context"
	"fmt"

	"github.com/tommoulard/atteler/pkg/permission"
)

func authorizeProviderPermission(ctx context.Context, providerName, action, target string, kinds ...permission.OperationKind) error {
	if len(kinds) == 0 {
		return nil
	}

	source := "atteler.provider." + providerName

	ops := make([]permission.Operation, 0, len(kinds))
	for _, kind := range kinds {
		ops = append(ops, permission.Operation{
			Kind:   kind,
			Action: action,
			Target: target,
			Source: source,
			Metadata: map[string]string{
				"provider": providerName,
			},
		})
	}

	decision := permission.Evaluate(ctx, nil, permission.Request{
		Operations: ops,
		Action:     action,
		Source:     source,
		Target:     target,
	})
	if decision.Allowed {
		return nil
	}

	return fmt.Errorf("%s: %w", providerName, &permission.Error{Decision: decision})
}

func authorizeProviderCredentialFileRead(ctx context.Context, providerName, action, target string) error {
	return authorizeProviderPermission(
		ctx,
		providerName,
		action,
		target,
		permission.OperationRead,
		permission.OperationCredentialAccess,
	)
}
