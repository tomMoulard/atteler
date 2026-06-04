package main

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tommoulard/atteler/pkg/permission"
)

func contextWithTUIPermissionPrompt(
	ctx context.Context,
	policy *permission.Policy,
	requestCh chan<- agentLoopConfirmRequest,
	responseCh <-chan bool,
) context.Context {
	ctx = permission.ContextWithPolicy(ctx, policy)
	if (policy == nil && permission.PolicyFromContext(ctx) == nil) || requestCh == nil || responseCh == nil {
		return ctx
	}

	return permission.ContextWithConfirmer(ctx, func(ctx context.Context, req permission.Request, decision permission.Decision) bool {
		return sendAgentLoopConfirmation(ctx, requestCh, responseCh, agentLoopConfirmRequest{
			kind:   agentLoopConfirmPermission,
			prompt: permissionConfirmPrompt(req, decision),
		})
	})
}

func contextWithTUIDefaultPermissionBlock(ctx context.Context, policy *permission.Policy) context.Context {
	ctx = permission.ContextWithPolicy(ctx, policy)
	if !permissionPolicyNeedsPrompt(policy) {
		return ctx
	}

	return permission.ContextWithConfirmer(ctx, func(context.Context, permission.Request, permission.Decision) bool {
		return false
	})
}

func contextWithPostTUIPermissionPrompt(ctx context.Context, policy *permission.Policy) context.Context {
	ctx = permission.ContextWithPolicy(ctx, policy)
	if !permissionPolicyNeedsPrompt(policy) {
		return ctx
	}

	return permission.ContextWithConfirmer(ctx, confirmPermissionStdin)
}

func permissionPolicyNeedsPrompt(policy *permission.Policy) bool {
	if policy == nil {
		return false
	}

	for _, kind := range permission.KnownOperationKinds() {
		if policy.ModeFor(kind) == permission.ModeAsk {
			return true
		}
	}

	return false
}

func withPermissionPromptLifecycle(cmd tea.Cmd, requestCh chan agentLoopConfirmRequest, promptCmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		if requestCh != nil {
			close(requestCh)
		}

		return nil
	}

	if requestCh == nil || promptCmd == nil {
		return cmd
	}

	return func() tea.Msg {
		raw := cmd()

		batch, ok := raw.(tea.BatchMsg)
		if !ok {
			var wg sync.WaitGroup

			wg.Add(1)

			return tea.Batch(
				func() tea.Msg {
					defer wg.Done()

					return raw
				},
				func() tea.Msg {
					wg.Wait()
					close(requestCh)

					return nil
				},
				promptCmd,
			)()
		}

		wrapped := make([]tea.Cmd, 0, len(batch)+2)

		var wg sync.WaitGroup

		for _, child := range batch {
			wg.Add(1)

			wrapped = append(wrapped, func() tea.Msg {
				defer wg.Done()

				return child()
			})
		}

		wrapped = append(wrapped,
			func() tea.Msg {
				wg.Wait()
				close(requestCh)

				return nil
			},
			promptCmd,
		)

		return tea.Batch(wrapped...)()
	}
}

func withPermissionPromptClose(cmd tea.Cmd, requestCh chan agentLoopConfirmRequest) tea.Cmd {
	if cmd == nil || requestCh == nil {
		return cmd
	}

	return func() tea.Msg {
		defer close(requestCh)

		return cmd()
	}
}
