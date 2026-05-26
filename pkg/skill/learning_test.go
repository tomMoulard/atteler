package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/events"
)

func TestLearnerCreatesAndImprovesGeneratedSkillFromRecurringCommands(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       skillDir,
		MaxSteps:       3,
		MinOccurrences: 2,
	})

	first := []string{
		"kubectl --context prod-use1 -n payments get pods",
		"kubectl --context prod-use1 -n payments describe pod checkout-7d9f",
		"kubectl --context prod-use1 -n payments logs checkout-7d9f",
	}
	second := []string{
		"kubectl --context prod-eu1 -n billing get pods",
		"kubectl --context prod-eu1 -n billing describe pod api-55f9",
		"kubectl --context prod-eu1 -n billing logs api-55f9",
	}
	third := []string{
		"kubectl --context prod-ap1 -n support get pods",
		"kubectl --context prod-ap1 -n support describe pod worker-abc123",
		"kubectl --context prod-ap1 -n support logs worker-abc123",
	}

	observeCommands(t, learner, first...)
	observeCommands(t, learner, second...)

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.False(t, state.Disabled)
	require.Len(t, state.Skills, 1)
	require.Equal(t, 2, state.Skills[0].Occurrences)
	require.FileExists(t, state.Skills[0].SkillPath)

	data, err := os.ReadFile(state.Skills[0].SkillPath)
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "kubectl --context={{context}} -n {{namespace}} get pods")
	require.Contains(t, content, "describe pod {{pod}}")
	require.Contains(t, content, "logs {{pod}}")
	require.NotContains(t, content, "prod-use1")
	require.NotContains(t, content, "payments")
	require.NotContains(t, content, "checkout-7d9f")

	observeCommands(t, learner, third...)

	state, err = NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.GreaterOrEqual(t, state.Skills[0].Occurrences, 3)
	require.GreaterOrEqual(t, len(state.Skills[0].Revisions), 2)
}

func TestLearnerCreatesAdditionalSkillWhenExistingBestIsCurrent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	observeCommands(t, learner,
		"kubectl --context prod-use1 -n payments get pods",
		"kubectl --context prod-use1 -n payments logs checkout-7d9f",
		"kubectl --context prod-eu1 -n billing get pods",
		"kubectl --context prod-eu1 -n billing logs api-55f9",
	)

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)

	observeCommands(t, learner,
		"git status --short",
		"go test ./pkg/skill",
		"git status --short",
		"go test ./pkg/skill",
	)

	state, err = NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 2)
	require.True(t, generatedSkillStepsContain(state.Skills, "run git status --short"))
}

func TestLearnerUsesUserPromptsAsContextNotWorkflowSteps(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       3,
		MinOccurrences: 2,
	})

	observeKubernetesPromptFlow := func(prompt, namespace, pod string) {
		t.Helper()

		require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
			Type:    events.UserMessage,
			Content: prompt,
		}))
		observeCommands(t, learner,
			"kubectl -n "+namespace+" get pods",
			"kubectl -n "+namespace+" logs "+pod,
		)
	}

	observeKubernetesPromptFlow("Investigate this Kubernetes incident.", "payments", "checkout-7d9f")
	observeKubernetesPromptFlow("Please debug this k8s outage.", "billing", "api-55f9")

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Equal(t, []string{
		"run kubectl -n {{namespace}} get pods",
		"run kubectl -n {{namespace}} logs {{pod}}",
	}, state.Skills[0].Steps)
	require.NotContains(t, state.Skills[0].Steps, "investigate kubernetes workflow")

	data, err := os.ReadFile(state.Skills[0].SkillPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "Prompts:")
	require.Contains(t, string(data), "investigate kubernetes workflow")
}

func TestLearnerAggregatesWorkflowsAcrossSessionsWithoutStitchingBoundaries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	observeSessionCommands(t, learner, "session-a", "echo left")
	observeSessionCommands(t, learner, "session-b", "echo right")
	observeSessionCommands(t, learner, "session-c", "echo left")
	observeSessionCommands(t, learner, "session-d", "echo right")

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.Len(t, state.Observations, 4)
	require.NotEmpty(t, state.Observations[0].SequenceKey)
	require.NotEqual(t, "session-a", state.Observations[0].SequenceKey)
}

func TestLearnerDetectsRepeatedWorkflowAcrossSessions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	observeSessionCommands(t, learner, "session-a", "echo plan", "echo code")
	observeSessionCommands(t, learner, "session-b", "echo plan", "echo code")

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Equal(t, []string{"run echo plan", "run echo code"}, state.Skills[0].Steps)
}

func TestLearnerDoesNotCarryPromptAcrossSequenceBoundaries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
		Type:      events.UserMessage,
		SessionID: "session-a",
		Content:   "Investigate this Kubernetes incident.",
	}))
	observeCommands(t, learner, "echo plan", "echo code", "echo plan", "echo code")

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)

	data, err := os.ReadFile(state.Skills[0].SkillPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "investigate kubernetes workflow")
}

func TestLearnerCreatesGeneratedSkillFromRecurringToolUsage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	observeTool := func(name string) {
		t.Helper()

		require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
			Type:    events.ToolExecute,
			Content: "token=raw-tool-output-should-not-persist",
			Metadata: map[string]string{
				"tool": name,
			},
		}))
	}

	observeTool("grafana.query")
	observeTool("loki.query")
	observeTool("grafana.query")
	observeTool("loki.query")

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Equal(t, []string{"use tool grafana.query", "use tool loki.query"}, state.Skills[0].Steps)

	data, err := os.ReadFile(state.Skills[0].SkillPath)
	require.NoError(t, err)

	content := string(data)
	require.Contains(t, content, "use tool grafana.query")
	require.Contains(t, content, "use tool loki.query")
	require.Contains(t, content, "tool")
	require.NotContains(t, content, "raw-tool-output")
	require.NotContains(t, content, "token=")
}

func TestObservationFromEventRedactsSecretsAndSkipsRawCommandOutput(t *testing.T) {
	t.Parallel()

	_, ok := ObservationFromEvent(events.Event{
		Type:    events.CommandOutput,
		Content: "token=secret-value raw pod logs",
	})
	require.False(t, ok)

	observation, ok := ObservationFromEvent(events.Event{
		Type:    events.UserMessage,
		Content: "Please inspect the failing pod logs for checkout token abc123.",
	})
	require.True(t, ok)
	require.Equal(t, "investigate kubernetes workflow", observation.Action)
	require.Equal(t, "investigate kubernetes workflow", observation.Prompt)
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "abc123")

	_, ok = ObservationFromEvent(events.Event{
		Type:    events.UserMessage,
		Content: "Write a podcast intro.",
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "codex.responses", "provider": "codex"},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "fzf"},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "spawn-agent --agent reviewer --prompt inspect"},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "env ATTELER_TOKEN=secret-value spawn-agent --agent reviewer --prompt inspect",
		},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "timeout 30 fzf",
		},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "codex.responses --model gpt-test"},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.ToolExecute,
		Metadata: map[string]string{
			"provider": "openai",
			"tool":     "llm.complete",
		},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type:    events.ToolExecute,
		Content: "grafana.query token=raw-output-should-not-be-stored",
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "spawn-agent --agent reviewer --prompt inspect",
			"source":  learningCommandSourceLLMTool,
		},
	})
	require.False(t, ok)

	observation, ok = ObservationFromEvent(events.Event{
		Type:    events.ToolExecute,
		Content: "token=raw-output-should-not-be-stored",
		Metadata: map[string]string{
			"tool": "grafana.query",
		},
	})
	require.True(t, ok)
	require.Equal(t, "use tool grafana.query", observation.Action)
	require.Equal(t, "tool", observation.ToolClass)
	require.Equal(t, []string{"grafana.query"}, observation.Inputs)
	require.NotContains(t, observation.Action, "raw-output")
	require.NotContains(t, observation.Action, "token")

	_, ok = ObservationFromEvent(events.Event{
		Type: events.ToolExecute,
		Metadata: map[string]string{
			"tool": "secret.scan",
		},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.ToolExecute,
		Metadata: map[string]string{
			"tool": "grafana.query --token raw",
		},
	})
	require.False(t, ok)

	_, ok = ObservationFromEvent(events.Event{
		Type: events.ToolExecute,
		Metadata: map[string]string{
			"tool": "https://private.example/tool",
		},
	})
	require.False(t, ok)

	observation, ok = ObservationFromEvent(events.Event{
		Type:      events.CommandExecute,
		SessionID: "session-that-should-not-be-stored",
		Metadata: map[string]string{
			"command": "kubectl --token=abc123 --context prod-use1 -n payments get secret stripe-api-key",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl --token={{token}} --context={{context}} -n {{namespace}} get secret {{secret}}", observation.Action)
	require.NotContains(t, observation.Action, "abc123")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")

	connectionURL := "postgres://user:" + "hidden-value" + "@db.internal/prod"
	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "DATABASE_URL=" + connectionURL + " migrate up",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run DATABASE_URL={{url}} migrate up", observation.Action)
	require.NotContains(t, observation.Action, "hidden-value")
	require.NotContains(t, observation.Action, "db.internal")
	require.NotContains(t, observation.Action, "prod")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "echo incident@example.com 550e8400-e29b-41d4-a716-446655440000",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run echo {{email}} {{id}}", observation.Action)
	require.NotContains(t, observation.Action, "incident@example.com")
	require.NotContains(t, observation.Action, "550e8400")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl --api-key=private-key get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl --api-key=[REDACTED] get pods", observation.Action)
	require.NotContains(t, observation.Action, "private-key")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl --token "abc 123" --context "prod use1" get pods`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl --token={{token}} --context={{context}} get pods", observation.Action)
	require.NotContains(t, observation.Action, "abc")
	require.NotContains(t, observation.Action, "prod use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl --selector app=checkout get pods -o wide",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl --selector={{selector}} get pods -o wide", observation.Action)
	require.NotContains(t, observation.Action, "checkout")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -l=app=checkout get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -l={{selector}} get pods", observation.Action)
	require.NotContains(t, observation.Action, "checkout")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl get -f ./deploy/private.yaml",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl get -f {{path}}", observation.Action)
	require.NotContains(t, observation.Action, "./deploy/private.yaml")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl rollout restart deployment/checkout-api",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl rollout restart deployment/{{deployment}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments exec checkout-7d9f -- printenv TOKEN",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} exec {{pod}} -- printenv TOKEN", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments exec checkout-7d9f -- cat /var/run/secrets/kubernetes.io/serviceaccount/token",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} exec {{pod}} -- cat [REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "/var/run/secrets")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl exec checkout-7d9f -- env API_TOKEN=abc123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl exec {{pod}} -- env API_TOKEN=[REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "abc123")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments exec checkout-7d9f -- curl --token "private token value" https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} exec {{pod}} -- curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "private")
	require.NotContains(t, observation.Action, "token value")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments exec checkout-7d9f -- curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} exec {{pod}} -- curl --data {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl logs --tail 100 checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl logs --tail 100 {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl logs -f checkout-7d9f -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl logs -f {{pod}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl logs -p checkout-7d9f -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl logs -p {{pod}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl top pod checkout-7d9f -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl top pod {{pod}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl set image deployment/checkout-api main=ghcr.io/private/project/checkout:abc123 -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl set image deployment/{{deployment}} main={{path}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")
	require.NotContains(t, observation.Action, "ghcr.io")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments set env deployment checkout-api API_TOKEN=-private LOG_LEVEL=debug",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} set env deployment {{deployment}} API_TOKEN=[REDACTED] LOG_LEVEL={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")
	require.NotContains(t, observation.Action, "-private")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "debug")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl set env --all API_TOKEN=-private",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl set env --all API_TOKEN=[REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "-private")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl annotate deployment/checkout-api private.example.com/token=secret-value -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl annotate deployment/{{deployment}} {{filter}}=[REDACTED] -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "secret-value")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl label pod checkout-7d9f customer-id=acme-prod",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl label pod {{pod}} {{filter}}={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "acme-prod")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl autoscale deployment checkout-api --min=1 --max=10 -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl autoscale deployment {{deployment}} --min=1 --max=10 -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl expose deployment checkout-api --name checkout-public -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl expose deployment {{deployment}} --name={{resource}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-api")
	require.NotContains(t, observation.Action, "checkout-public")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl drain prod-node-1 --ignore-daemonsets",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl drain {{node}} --ignore-daemonsets", observation.Action)
	require.NotContains(t, observation.Action, "prod-node-1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl taint nodes prod-node-1 dedicated=payments:NoSchedule",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl taint nodes {{node}} {{filter}}={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-node-1")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl port-forward pod/checkout-7d9f 8080:8080 -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl port-forward pod/{{pod}} 8080:8080 -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl cp payments/checkout-7d9f:/var/log/app.log ./app.log -c main",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl cp {{namespace}}/{{pod}}:{{path}} {{path}} -c {{container}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "/var/log/app.log")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectl -n payments logs checkout-7d9f"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectl -n {{namespace}} logs {{pod}}"`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectl -n payments logs checkout-7d9f | grep stripe-api-key"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectl -n {{namespace}} logs {{pod}} | grep [REDACTED]"`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "stripe-api-key")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123 | kubectl -n payments logs checkout-7d9f"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "curl --data {{filter}} {{url}} | kubectl -n {{namespace}} logs {{pod}}"`, observation.Action)
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectl -n payments run checkout-debug -- curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectl -n {{namespace}} run {{pod}} -- curl --data {{filter}} {{url}}"`, observation.Action)
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-debug")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectl -n payments get pods | grep checkout-7d9f"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectl -n {{namespace}} get pods | grep {{pod}}"`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectl -n payments get pods | grep checkout-api"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectl -n {{namespace}} get pods | grep {{filter}}"`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-api")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments get pods && kubectl -n payments logs checkout-7d9f`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run kubectl -n {{namespace}} get pods && kubectl -n {{namespace}} logs {{pod}}`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments get pods;kubectl -n payments logs checkout-7d9f`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run kubectl -n {{namespace}} get pods ; kubectl -n {{namespace}} logs {{pod}}`, observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `bash -lc "kubectx prod-use1 && kubectl -n payments get pods"`,
		},
	})
	require.True(t, ok)
	require.Equal(t, `run bash -lc "kubectx {{context}} && kubectl -n {{namespace}} get pods"`, observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubens payments && kubectl logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubens {{namespace}} && kubectl logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "KUBECONFIG=/private/clusters/prod-use1.yaml kubectl -n payments logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run KUBECONFIG={{path}} kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "sudo -E kubectl -n payments logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run sudo -E kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "sudo -u deploy kubectl -n payments logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run sudo -u {{user}} kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "deploy")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "env -i KUBECONFIG=/private/clusters/prod-use1.yaml kubectl --context prod-use1 get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run env -i KUBECONFIG={{path}} kubectl --context={{context}} get pods", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "env -u KUBECONFIG kubectl -n payments logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run env -u {{user}} kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "watch -n 2 kubectl -n payments get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run watch -n 2 kubectl -n {{namespace}} get pods", observation.Action)
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "timeout 30s kubectl -n payments logs checkout-7d9f",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run timeout 30s kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "nice -n 10 kubectl -n payments get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run nice -n 10 kubectl -n {{namespace}} get pods", observation.Action)
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "KUBE_CONTEXT=prod-use1 kubectl get pods",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run KUBE_CONTEXT={{context}} kubectl get pods", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl config use-context prod-use1",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl config use-context {{context}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectx prod-use1",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectx {{context}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubens payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubens {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments describe secret/stripe-api-key",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} describe secret/{{secret}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments get secret stripe-api-key -o jsonpath='{.data.token}'",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} get secret {{secret}} -o {{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")
	require.NotContains(t, observation.Action, ".data")
	require.NotContains(t, observation.Action, "token")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl get --raw /api/v1/namespaces/payments/secrets/stripe-api-key",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl get --raw={{path}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")
	require.NotContains(t, observation.Action, "/api/v1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl config view --raw -o json",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl config view --raw -o json", observation.Action)

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n argo get workflows checkout-flow-abc",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} get workflows {{resource}}", observation.Action)
	require.NotContains(t, observation.Action, "argo")
	require.NotContains(t, observation.Action, "checkout-flow-abc")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl auth can-i get secret stripe-api-key --as alice@example.com -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl auth can-i get secret {{secret}} --as={{user}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "stripe-api-key")
	require.NotContains(t, observation.Action, "alice@example.com")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n argo auth can-i get workflows checkout-flow-abc",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} auth can-i get workflows {{resource}}", observation.Action)
	require.NotContains(t, observation.Action, "argo")
	require.NotContains(t, observation.Action, "checkout-flow-abc")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n argo wait --for=condition=Completed workflow/checkout-flow-abc --timeout=5m",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} wait --for=condition=Completed workflow/{{resource}} --timeout=5m", observation.Action)
	require.NotContains(t, observation.Action, "argo")
	require.NotContains(t, observation.Action, "checkout-flow-abc")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl get pods --template '{{range .items}}{{.metadata.name}}{{end}}'`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl get pods --template={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, ".metadata.name")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments wait --for=condition=Ready pod/checkout-7d9f --timeout=60s",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} wait --for=condition=Ready pod/{{pod}} --timeout=60s", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments debug pod/checkout-7d9f --image=ghcr.io/private/debug:sha --target main",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} debug pod/{{pod}} --image={{path}} --target={{container}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "ghcr.io")
	require.NotContains(t, observation.Action, "main")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments debug pod/checkout-7d9f --image=ghcr.io/private/debug:sha -- curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} debug pod/{{pod}} --image={{path}} -- curl --data {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "ghcr.io")
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl run checkout-debug --image ghcr.io/private/debug:sha --env API_TOKEN=secret -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl run {{pod}} --image={{path}} --env={{filter}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-debug")
	require.NotContains(t, observation.Action, "ghcr.io")
	require.NotContains(t, observation.Action, "API_TOKEN")
	require.NotContains(t, observation.Action, "secret")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments run checkout-debug -- curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} run {{pod}} -- curl --data {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-debug")
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments run checkout-debug -- curl --token "private token value" https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} run {{pod}} -- curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-debug")
	require.NotContains(t, observation.Action, "private")
	require.NotContains(t, observation.Action, "token value")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl attach checkout-7d9f -c main -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl attach {{pod}} -c {{container}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "main")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl proxy --accept-hosts='^private.example.com$' --api-prefix=/clusters/prod-use1 --www /tmp/k8s-dashboard",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl proxy --accept-hosts={{filter}} --api-prefix={{filter}} --www={{path}}", observation.Action)
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "/tmp/k8s-dashboard")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl proxy -p 8001 --accept-hosts private.example.com",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl proxy -p 8001 --accept-hosts={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "{{patch}}")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl proxy -p=8001 --accept-hosts private.example.com",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl proxy -p=8001 --accept-hosts={{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "{{patch}}")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments create secret generic stripe-api-key --from-literal=password=hunter2",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} create secret generic {{secret}} --from-literal=[REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")
	require.NotContains(t, observation.Action, "hunter2")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "kubectl -n payments create secret tls checkout-tls --cert=prod-use1.crt --key=prod-use1.key",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} create secret tls {{secret}} --cert={{path}} --key={{path}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-tls")
	require.NotContains(t, observation.Action, "prod-use1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `kubectl -n payments patch secret stripe-api-key -p '{"data":{"token":"abc123","password":"hunter2"}}'`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} patch secret {{secret}} -p {{patch}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "stripe-api-key")
	require.NotContains(t, observation.Action, "abc123")
	require.NotContains(t, observation.Action, "hunter2")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl --token private-token https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-token")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl --token -private-token https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-token")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --token "private token value" https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private")
	require.NotContains(t, observation.Action, "token value")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --token "private && token value" https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --token [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private")
	require.NotContains(t, observation.Action, "token value")
	require.NotContains(t, observation.Action, "&&")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl --apiKey private-key https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --apiKey [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-key")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl --user admin:secret https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --user {{user}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "admin")
	require.NotContains(t, observation.Action, "secret")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --data {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl -d token=private-token https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl -d [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-token")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123 | kubectl -n payments logs checkout-7d9f`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --data {{filter}} {{url}} | kubectl -n {{namespace}} logs {{pod}}", observation.Action)
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `kubectl -n payments logs checkout-7d9f | curl --data '{"customer":"cust-123"}' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run kubectl -n {{namespace}} logs {{pod}} | curl --data {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "checkout-7d9f")
	require.NotContains(t, observation.Action, "customer")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl -H 'x-api-key: abc123' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl -H [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "x-api-key")
	require.NotContains(t, observation.Action, "abc123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --header='Authorization: Bearer abc123' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --header=[REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "Authorization")
	require.NotContains(t, observation.Action, "Bearer")
	require.NotContains(t, observation.Action, "abc123")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl -H 'Host: private.example.com' https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl -H {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "Host")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --resolve private.example.com:443:10.0.0.1 https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --resolve {{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "10.0.0.1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --connect-to=private.example.com:443:internal.svc.cluster.local:8443 https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --connect-to={{filter}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private.example.com")
	require.NotContains(t, observation.Action, "internal.svc.cluster.local")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "curl --oauth2-bearer private-bearer-token https://private.example.com/customer/123",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --oauth2-bearer [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-bearer-token")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `curl --cookie "session=private cookie value" https://private.example.com/customer/123`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run curl --cookie [REDACTED] {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "session=")
	require.NotContains(t, observation.Action, "private cookie value")
	require.NotContains(t, observation.Action, "private.example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "CLIENT_CREDENTIAL=private-client-credential npm test",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run CLIENT_CREDENTIAL=[REDACTED] npm test", observation.Action)
	require.NotContains(t, observation.Action, "private-client-credential")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "deploy --private-key /tmp/private-prod.key",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run deploy --private-key [REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "/tmp/private-prod.key")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "AWS_SECRET_ACCESS_KEY=super-secret-value go test ./pkg/skill",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}", observation.Action)
	require.NotContains(t, observation.Action, "super-secret-value")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `AWS_SECRET_ACCESS_KEY="super secret value" go test ./pkg/skill`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}", observation.Action)
	require.NotContains(t, observation.Action, "super")
	require.NotContains(t, observation.Action, "secret value")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": `AWS_SECRET_ACCESS_KEY="super && secret value" go test ./pkg/skill`,
		},
	})
	require.True(t, ok)
	require.Equal(t, "run AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}", observation.Action)
	require.NotContains(t, observation.Action, "super")
	require.NotContains(t, observation.Action, "secret value")
	require.NotContains(t, observation.Action, "&&")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "grep stripe-api-key README.md",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run grep [REDACTED] {{path}}", observation.Action)
	require.NotContains(t, observation.Action, "stripe-api-key")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "grep kubectl README.md",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run grep kubectl {{path}}", observation.Action)

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "gcloud container clusters get-credentials prod-use1 --region us-east1 --project private-project",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run gcloud container clusters get-credentials {{cluster}} --region {{region}} --project {{project}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "us-east1")
	require.NotContains(t, observation.Action, "private-project")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "gcloud --project private-project container clusters get-credentials prod-use1 --region us-east1",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run gcloud --project {{project}} container clusters get-credentials {{cluster}} --region {{region}}", observation.Action)
	require.NotContains(t, observation.Action, "private-project")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "us-east1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `gcloud logging read 'resource.labels.namespace_name="payments" resource.labels.cluster_name="prod-use1" resource.labels.container_name="main" resource.labels.pod_name="checkout-7d9f"'`,
		},
	})
	require.True(t, ok)
	require.Contains(t, observation.Action, "{{filter}}")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "main")
	require.NotContains(t, observation.Action, "checkout-7d9f")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": `gcloud --project private-project logging read 'jsonPayload.customer_id="cust-123" textPayload:"api-token" resource.labels.namespace_name="payments"'`,
		},
	})
	require.True(t, ok)
	require.Contains(t, observation.Action, "{{filter}}")
	require.NotContains(t, observation.Action, "private-project")
	require.NotContains(t, observation.Action, "cust-123")
	require.NotContains(t, observation.Action, "api-token")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "aws eks update-kubeconfig --name prod-use1 --region us-east1 --profile synthflow",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run aws eks update-kubeconfig --name {{cluster}} --region {{region}} --profile {{profile}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "us-east1")
	require.NotContains(t, observation.Action, "synthflow")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "aws eks update-kubeconfig --name prod-use1 --alias synthflow-prod --user-alias alice@example.com",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run aws eks update-kubeconfig --name {{cluster}} --alias {{context}} --user-alias {{user}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "synthflow-prod")
	require.NotContains(t, observation.Action, "alice@example.com")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "aws --profile synthflow eks update-kubeconfig --name prod-use1 --region us-east1",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run aws --profile {{profile}} eks update-kubeconfig --name {{cluster}} --region {{region}}", observation.Action)
	require.NotContains(t, observation.Action, "synthflow")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "us-east1")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "az aks get-credentials --name prod-use1 --resource-group rg-prod --subscription private-subscription",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run az aks get-credentials --name {{cluster}} --resource-group {{resource-group}} --subscription {{subscription}}", observation.Action)
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "rg-prod")
	require.NotContains(t, observation.Action, "private-subscription")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "az --subscription private-subscription aks get-credentials --name prod-use1 --resource-group rg-prod",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run az --subscription {{subscription}} aks get-credentials --name {{cluster}} --resource-group {{resource-group}}", observation.Action)
	require.NotContains(t, observation.Action, "private-subscription")
	require.NotContains(t, observation.Action, "prod-use1")
	require.NotContains(t, observation.Action, "rg-prod")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm status checkout -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm status {{release}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm status checkout --password private-password -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm status {{release}} --password [REDACTED] -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "private-password")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm repo add internal https://alice:" + "secret-token" + "@charts.example.com/private --username alice --password private-password",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm repo add {{repository}} {{url}} --username {{user}} --password [REDACTED]", observation.Action)
	require.NotContains(t, observation.Action, "internal")
	require.NotContains(t, observation.Action, "charts.example.com")
	require.NotContains(t, observation.Action, "alice")
	require.NotContains(t, observation.Action, "secret-token")
	require.NotContains(t, observation.Action, "private-password")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm install checkout ./charts/checkout -n payments --set image.tag=private-build-123 --set-string api.token=secret-token",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm install {{release}} {{path}} -n {{namespace}} --set {{filter}} --set-string {{filter}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "payments")
	require.NotContains(t, observation.Action, "private-build-123")
	require.NotContains(t, observation.Action, "secret-token")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm upgrade checkout oci://registry.example.com/private/chart --set-json='secret={\"token\":\"abc123\"}' -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm upgrade {{release}} {{url}} --set-json={{filter}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "registry.example.com")
	require.NotContains(t, observation.Action, "abc123")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm upgrade --password private-password checkout oci://registry.example.com/private/chart -n payments",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm upgrade --password [REDACTED] {{release}} {{url}} -n {{namespace}}", observation.Action)
	require.NotContains(t, observation.Action, "private-password")
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "registry.example.com")
	require.NotContains(t, observation.Action, "payments")

	observation, ok = ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"command": "helm --kube-token private-token --kube-apiserver https://k8s.example.com upgrade checkout oci://registry.example.com/private/chart",
		},
	})
	require.True(t, ok)
	require.Equal(t, "run helm --kube-token {{token}} --kube-apiserver {{server}} upgrade {{release}} {{url}}", observation.Action)
	require.NotContains(t, observation.Action, "private-token")
	require.NotContains(t, observation.Action, "k8s.example.com")
	require.NotContains(t, observation.Action, "checkout")
	require.NotContains(t, observation.Action, "registry.example.com")
}

func TestObservationFromEventDetectsVerificationCommandsThroughShellPrefixes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		command   string
		sanitized string
	}{
		{
			name:      "environment assignment",
			command:   "AWS_SECRET_ACCESS_KEY=super-secret-value go test ./pkg/skill",
			sanitized: "AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}",
		},
		{
			name:      "quoted environment assignment",
			command:   `AWS_SECRET_ACCESS_KEY="super secret value" go test ./pkg/skill`,
			sanitized: "AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}",
		},
		{
			name:      "quoted environment assignment containing shell operator",
			command:   `AWS_SECRET_ACCESS_KEY="super && secret value" go test ./pkg/skill`,
			sanitized: "AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}",
		},
		{
			name:      "env wrapper",
			command:   "env AWS_SECRET_ACCESS_KEY=super-secret-value go test ./pkg/skill",
			sanitized: "env AWS_SECRET_ACCESS_KEY=[REDACTED] go test {{path}}",
		},
		{
			name:      "timeout wrapper",
			command:   "timeout 30 go test ./pkg/skill",
			sanitized: "timeout 30 go test {{path}}",
		},
		{
			name:      "sudo wrapper",
			command:   "sudo -u root go test ./pkg/skill",
			sanitized: "sudo -u {{user}} go test {{path}}",
		},
		{
			name:      "package manager test",
			command:   "pnpm --filter frontend test",
			sanitized: "pnpm --filter frontend test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			observation, ok := ObservationFromEvent(events.Event{
				Type: events.CommandExecute,
				Metadata: map[string]string{
					"source":  "user",
					"command": tc.command,
				},
			})
			require.True(t, ok)
			require.Equal(t, "run "+tc.sanitized, observation.Action)
			require.Equal(t, []string{tc.sanitized}, observation.VerificationCommands)
		})
	}

	observation, ok := ObservationFromEvent(events.Event{
		Type: events.CommandExecute,
		Metadata: map[string]string{
			"source":  "user",
			"command": "grep go test README.md",
		},
	})
	require.True(t, ok)
	require.Empty(t, observation.VerificationCommands)
}

func TestLearningStoreCanDisableAndDeleteGeneratedSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillRoot := filepath.Join(root, "skills", "plan-code")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code\n"), 0o600))

	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{{
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Name:      "Plan Code Skill",
		Slug:      "plan-code",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
	}}}))

	updated, err := store.SetSkillStatus("plan-code", LearningSkillStatusDisabled)
	require.NoError(t, err)
	require.Equal(t, LearningSkillStatusDisabled, updated.Status)

	updated, err = store.SetSkillStatus("plan-code", LearningSkillStatusActive)
	require.NoError(t, err)
	require.Equal(t, LearningSkillStatusActive, updated.Status)
	require.Equal(t, hashBytes([]byte("# Plan Code\n")), updated.SkillHash)

	require.NoError(t, store.SetEnabled(false))
	state, err := store.Load()
	require.NoError(t, err)
	require.True(t, state.Disabled)

	require.NoError(t, store.DeleteSkill("plan-code", true))
	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.NoFileExists(t, skillPath)
}

func TestLearningStoreDeleteSkillPrunesSourceObservations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       skillDir,
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	observeCommands(t, learner,
		"echo plan",
		"echo code",
		"echo plan",
		"echo code",
	)

	store := NewLearningStore(storeDir)
	state, err := store.Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Len(t, state.Observations, 4)

	skill := state.Skills[0]
	require.NoError(t, store.DeleteSkillInDir(skill.Slug, true, skillDir))
	require.NoDirExists(t, filepath.Dir(skill.SkillPath))

	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.Empty(t, state.Observations)

	observeCommands(t, learner, "echo unrelated")

	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.Len(t, state.Observations, 1)
}

func TestLearningStoreDeleteSkillKeepsPromptsAcrossSequenceBoundary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillDir := filepath.Join(root, "skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")

	require.NoError(t, store.Save(LearningState{
		Observations: []ObservationRecord{
			{
				EventType:   events.UserMessage,
				Action:      "investigate kubernetes workflow",
				Prompt:      "investigate kubernetes workflow",
				SequenceKey: "prompt-session",
				ToolClass:   "prompt",
			},
			{EventType: events.CommandExecute, Action: "run echo plan", ToolClass: "shell"},
			{EventType: events.CommandExecute, Action: "run echo code", ToolClass: "shell"},
		},
		Skills: []GeneratedSkill{{
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			Name:      "Plan Code Skill",
			Slug:      "plan-code",
			Status:    LearningSkillStatusActive,
			SkillPath: skillPath,
			Steps:     []string{"run echo plan", "run echo code"},
		}},
	}))

	require.NoError(t, store.DeleteSkillInDir("plan-code", false, skillDir))

	state, err := store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.Len(t, state.Observations, 1)
	require.Equal(t, "investigate kubernetes workflow", state.Observations[0].Action)
}

func TestLearnerRespectsDisabledStoreAndDisabledOptions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	store := NewLearningStore(storeDir)
	require.NoError(t, store.SetEnabled(false))

	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MinOccurrences: 2,
	})
	require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "kubectl -n payments get pods"},
	}))

	state, err := store.Load()
	require.NoError(t, err)
	require.True(t, state.Disabled)
	require.Empty(t, state.Observations)
	require.Empty(t, state.Skills)

	enabled := false
	disabledDir := filepath.Join(root, "disabled-options")
	disabledLearner := NewLearner(LearningOptions{
		Enabled:  &enabled,
		StoreDir: disabledDir,
		SkillDir: filepath.Join(root, "disabled-skills"),
	})
	require.NoError(t, disabledLearner.ObserveEvent(t.Context(), events.Event{
		Type:     events.CommandExecute,
		Metadata: map[string]string{"command": "kubectl -n payments get pods"},
	}))
	require.NoDirExists(t, disabledDir)
}

func TestLearningStoreLoadMissingDoesNotCreateStateDir(t *testing.T) {
	t.Parallel()

	storeDir := filepath.Join(t.TempDir(), "learning")
	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Equal(t, learningStateVersion, state.Version)
	require.NoDirExists(t, storeDir)
}

func TestLearnerSerializesConcurrentObservers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	opts := LearningOptions{
		StoreDir:       filepath.Join(root, "learning"),
		SkillDir:       filepath.Join(root, "skills"),
		MinOccurrences: 100,
	}

	const count = 20

	errCh := make(chan error, count)

	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			learner := NewLearner(opts)
			errCh <- learner.ObserveEvent(t.Context(), events.Event{
				Type:     events.CommandExecute,
				Metadata: map[string]string{"command": fmt.Sprintf("echo recurring-workflow-%02d", i)},
			})
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	state, err := NewLearningStore(opts.StoreDir).Load()
	require.NoError(t, err)
	require.Len(t, state.Observations, count)
}

func TestLearnerKeepsObservationsWhenSkillGenerationFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	validSkillDir := filepath.Join(root, "skills")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       validSkillDir,
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	commands := []string{
		"git status --short",
		"go test ./pkg/skill",
		"git status --short",
		"go test ./pkg/skill",
	}
	observeCommands(t, learner, commands...)

	store := NewLearningStore(storeDir)
	state, err := store.Load()
	require.NoError(t, err)
	require.Len(t, state.Observations, len(commands))
	require.Len(t, state.Skills, 1)

	state.Skills = nil
	require.NoError(t, store.Save(state))

	blockedSkillDir := filepath.Join(root, "skills-file")
	require.NoError(t, os.WriteFile(blockedSkillDir, []byte("not a directory\n"), 0o600))

	blockedLearner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       blockedSkillDir,
		MaxSteps:       2,
		MinOccurrences: 2,
	})

	for _, command := range commands[:2] {
		observeErr := blockedLearner.ObserveEvent(t.Context(), events.Event{
			Type:      events.CommandExecute,
			Timestamp: time.Now().UTC(),
			Metadata:  map[string]string{"command": command},
		})
		require.Error(t, observeErr)
	}

	state, err = store.Load()
	require.NoError(t, err)
	require.Len(t, state.Observations, len(commands)+2)
	require.Empty(t, state.Skills)
}

func TestApplyGeneratedSkillSuggestionKeepsSubsumedSkillWhenReviewBuildFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	oldSkillPath := filepath.Join(skillDir, "k8s", "SKILL.md")
	oldContent := []byte("# K8s Skill\n")

	require.NoError(t, os.MkdirAll(filepath.Dir(oldSkillPath), 0o750))
	require.NoError(t, os.WriteFile(oldSkillPath, oldContent, 0o600))

	learner := NewLearner(LearningOptions{
		StoreDir: storeDir,
		SkillDir: skillDir,
	})

	state := LearningState{Skills: []GeneratedSkill{{
		Name:      "K8s Skill",
		Slug:      "k8s",
		Status:    LearningSkillStatusActive,
		SkillPath: oldSkillPath,
		Steps:     []string{"run kubectl get pods"},
		SkillHash: hashBytes(oldContent),
	}}}

	updated, changed, err := learner.applyGeneratedSkillSuggestion(state, Suggestion{
		Name: "K8s Investigation Skill",
		Slug: "k8s-investigation",
		// Missing Steps intentionally makes BuildReview fail after the new
		// suggestion has been identified as subsuming the old generated skill.
	})
	require.Error(t, err)
	require.False(t, changed)
	require.Len(t, updated.Skills, 1)
	require.Equal(t, "k8s", updated.Skills[0].Slug)
	require.FileExists(t, oldSkillPath)
}

func TestLearningStoreDeleteSkillRefusesUnexpectedPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillRoot := filepath.Join(root, "skills", "unexpected")
	skillPath := filepath.Join(skillRoot, "SKILL.md")
	require.NoError(t, os.MkdirAll(skillRoot, 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code\n"), 0o600))

	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{{
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Name:      "Plan Code Skill",
		Slug:      "plan-code",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
	}}}))

	err := store.DeleteSkill("plan-code", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to delete unexpected generated skill directory")
	require.FileExists(t, skillPath)

	state, loadErr := store.Load()
	require.NoError(t, loadErr)
	require.Len(t, state.Skills, 1)
}

func TestLearningStoreDeleteSkillInDirRefusesOutsideSkillDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillPath := filepath.Join(root, "outside", "plan-code", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code\n"), 0o600))

	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{{
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Name:      "Plan Code Skill",
		Slug:      "plan-code",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
	}}}))

	err := store.DeleteSkillInDir("plan-code", true, filepath.Join(root, "skills"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside generated skill directory")
	require.FileExists(t, skillPath)

	state, loadErr := store.Load()
	require.NoError(t, loadErr)
	require.Len(t, state.Skills, 1)
}

func TestGeneratedSkillPathValidationRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillDir := filepath.Join(root, "skills")
	outsideDir := filepath.Join(root, "outside", "k8s-investigation")
	outsidePath := filepath.Join(outsideDir, "SKILL.md")
	require.NoError(t, os.MkdirAll(outsideDir, 0o750))
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	require.NoError(t, os.WriteFile(outsidePath, []byte("# Outside\n"), 0o600))

	symlinkPath := filepath.Join(skillDir, "k8s-investigation")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	skill := GeneratedSkill{
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      LearningSkillStatusActive,
		SkillPath:   filepath.Join(symlinkPath, "SKILL.md"),
		Steps:       []string{"run kubectl get pods"},
		Occurrences: 2,
	}

	err := ValidateGeneratedSkillPath(skill, skillDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolves outside generated skill directory")

	storeDir := filepath.Join(root, "learning")
	store := NewLearningStore(storeDir)
	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{skill}}))

	refs, err := MatchingGeneratedSkills("investigate kubernetes pods", ReferenceOptions{
		StoreDir: storeDir,
		SkillDir: skillDir,
	})
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestGeneratedSkillPathValidationAllowsAbsolutePathUnderRelativeSkillDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	skillDir := filepath.Join(root, "skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Plan Code\n"), 0o600))

	relativeSkillDir, err := filepath.Rel(cwd, skillDir)
	require.NoError(t, err)

	require.NoError(t, ValidateGeneratedSkillPath(GeneratedSkill{
		Name:      "Plan Code Skill",
		Slug:      "plan-code",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
	}, relativeSkillDir))
}

func TestLearningStoreDeleteSkillAllowsMissingGeneratedSkillDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillDir := filepath.Join(root, "missing-skills")
	skillPath := filepath.Join(skillDir, "plan-code", "SKILL.md")

	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{{
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Name:      "Plan Code Skill",
		Slug:      "plan-code",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
	}}}))

	require.NoError(t, store.DeleteSkillInDir("plan-code", true, skillDir))

	state, err := store.Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
}

func TestLearnerDoesNotFollowGeneratedSkillSymlinkWhenUpdating(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	outsideDir := filepath.Join(root, "outside", "k8s-investigation")
	outsidePath := filepath.Join(outsideDir, "SKILL.md")
	outsideContent := []byte("# Outside\n")

	require.NoError(t, os.MkdirAll(outsideDir, 0o750))
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	require.NoError(t, os.WriteFile(outsidePath, outsideContent, 0o600))

	symlinkPath := filepath.Join(skillDir, "k8s-investigation")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	learner := NewLearner(LearningOptions{
		StoreDir: storeDir,
		SkillDir: skillDir,
	})
	state := LearningState{Skills: []GeneratedSkill{{
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      LearningSkillStatusActive,
		SkillPath:   filepath.Join(symlinkPath, "SKILL.md"),
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 2,
		SkillHash:   hashBytes(outsideContent),
	}}}

	updated, changed, err := learner.applyGeneratedSkillSuggestion(state, Suggestion{
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 3,
	})
	require.NoError(t, err)
	require.False(t, changed)
	require.Len(t, updated.Skills, 1)
	require.Equal(t, filepath.Join(symlinkPath, "SKILL.md"), updated.Skills[0].SkillPath)
	require.FileExists(t, outsidePath)

	data, readErr := os.ReadFile(outsidePath)
	require.NoError(t, readErr)
	require.Equal(t, outsideContent, data)
}

func TestWriteLearningReviewRefusesSymlinkedSubdirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillDir := filepath.Join(root, "skills")
	review, err := BuildReview(skillDir, Suggestion{
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 2,
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(review.Root, 0o750))

	outsideDir := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outsideDir, 0o750))

	if symlinkErr := os.Symlink(outsideDir, filepath.Join(review.Root, "evals")); symlinkErr != nil {
		t.Skipf("symlink unavailable: %v", symlinkErr)
	}

	err = writeLearningReview(review)
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolves outside generated skill directory")
	require.NoFileExists(t, filepath.Join(outsideDir, "triggers.yaml"))
}

func TestWriteLearningReviewRefusesSymlinkedGeneratedSkillParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outsideDir := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(outsideDir, "generated"), 0o750))

	symlinkParent := filepath.Join(root, "skills")
	if symlinkErr := os.Symlink(outsideDir, symlinkParent); symlinkErr != nil {
		t.Skipf("symlink unavailable: %v", symlinkErr)
	}

	review, err := BuildReview(filepath.Join(symlinkParent, "generated"), Suggestion{
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 2,
	})
	require.NoError(t, err)

	err = writeLearningReview(review)
	require.Error(t, err)
	require.Contains(t, err.Error(), "symlinked generated skill path")
	require.NoFileExists(t, filepath.Join(outsideDir, "generated", "k8s-investigation", "SKILL.md"))
}

func TestLearnerDoesNotOverwriteManuallyEditedGeneratedSkill(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       filepath.Join(root, "skills"),
		MaxSteps:       3,
		MinOccurrences: 2,
	})

	observeCommands(t, learner,
		"kubectl --context prod-use1 -n payments get pods",
		"kubectl --context prod-use1 -n payments logs checkout-7d9f",
		"kubectl --context prod-eu1 -n billing get pods",
		"kubectl --context prod-eu1 -n billing logs api-55f9",
	)

	store := NewLearningStore(storeDir)
	state, err := store.Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.NotEmpty(t, state.Skills[0].SkillHash)

	skillPath := state.Skills[0].SkillPath
	require.NoError(t, os.WriteFile(skillPath, []byte("# Custom K8s Skill\nmanual edit\n"), 0o600))

	observeCommands(t, learner,
		"kubectl --context prod-ap1 -n support get pods",
		"kubectl --context prod-ap1 -n support logs worker-abc123",
	)

	data, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	require.Equal(t, "# Custom K8s Skill\nmanual edit\n", string(data))

	state, err = store.Load()
	require.NoError(t, err)
	require.Len(t, state.Skills, 1)
	require.Equal(t, 2, state.Skills[0].Occurrences)
	require.Len(t, state.Skills[0].Revisions, 1)
}

func TestLearnerDoesNotOverwriteUntrackedSkillDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	untrackedSlug := slugForSteps([]string{
		"run kubectl --context={{context}} -n {{namespace}} get pods",
		"run kubectl --context={{context}} -n {{namespace}} logs {{pod}}",
	})
	untrackedPath := filepath.Join(skillDir, untrackedSlug, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(untrackedPath), 0o750))
	require.NoError(t, os.WriteFile(untrackedPath, []byte("# Existing User Skill\n"), 0o600))

	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       skillDir,
		MaxSteps:       3,
		MinOccurrences: 2,
	})

	observeCommands(t, learner,
		"kubectl --context prod-use1 -n payments get pods",
		"kubectl --context prod-use1 -n payments logs checkout-7d9f",
		"kubectl --context prod-eu1 -n billing get pods",
		"kubectl --context prod-eu1 -n billing logs api-55f9",
	)

	data, err := os.ReadFile(untrackedPath)
	require.NoError(t, err)
	require.Equal(t, "# Existing User Skill\n", string(data))

	state, err := NewLearningStore(storeDir).Load()
	require.NoError(t, err)
	require.Empty(t, state.Skills)
	require.NotEmpty(t, state.Observations)
}

func TestGeneratedSkillFileModifiedTreatsUnknownHashAsModified(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.WriteFile(path, []byte("# Existing Skill\n"), 0o600))

	require.True(t, generatedSkillFileModified(GeneratedSkill{SkillPath: path}))
	require.False(t, generatedSkillFileModified(GeneratedSkill{
		SkillPath: path,
		SkillHash: hashBytes([]byte("# Existing Skill\n")),
	}))
}

func TestGeneratedSkillFileModifiedTreatsUnreadablePathAsModified(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.Mkdir(path, 0o750))

	require.True(t, generatedSkillFileModified(GeneratedSkill{
		SkillPath: path,
		SkillHash: hashBytes([]byte("# Existing Skill\n")),
	}))
}

func TestRemoveSubsumedGeneratedSkillsPreservesEditedSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillPath := filepath.Join(root, "k8s", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Manually Edited K8s Skill\n"), 0o600))

	skills := []GeneratedSkill{{
		Name:      "K8s Skill",
		Slug:      "k8s",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
		Steps:     []string{"run kubectl get pods"},
		SkillHash: hashBytes([]byte("# Original Generated K8s Skill\n")),
	}}

	got := removeSubsumedGeneratedSkills(skills, Suggestion{
		Slug:  "k8s-investigation",
		Steps: []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
	}, root)
	require.Len(t, got, 1)
	require.Equal(t, "k8s", got[0].Slug)
	require.FileExists(t, skillPath)
}

func TestRemoveSubsumedGeneratedSkillsPreservesUnexpectedPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillPath := filepath.Join(root, "unexpected", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Generated K8s Skill\n"), 0o600))

	skills := []GeneratedSkill{{
		Name:      "K8s Skill",
		Slug:      "k8s",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
		Steps:     []string{"run kubectl get pods"},
		SkillHash: hashBytes([]byte("# Generated K8s Skill\n")),
	}}

	got := removeSubsumedGeneratedSkills(skills, Suggestion{
		Slug:  "k8s-investigation",
		Steps: []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
	}, root)
	require.Len(t, got, 1)
	require.Equal(t, "k8s", got[0].Slug)
	require.FileExists(t, skillPath)
}

func TestRemoveSubsumedGeneratedSkillsPreservesOutsideSkillDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	skillPath := filepath.Join(root, "outside", "k8s", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# Generated K8s Skill\n"), 0o600))

	skills := []GeneratedSkill{{
		Name:      "K8s Skill",
		Slug:      "k8s",
		Status:    LearningSkillStatusActive,
		SkillPath: skillPath,
		Steps:     []string{"run kubectl get pods"},
		SkillHash: hashBytes([]byte("# Generated K8s Skill\n")),
	}}

	got := removeSubsumedGeneratedSkills(skills, Suggestion{
		Slug:  "k8s-investigation",
		Steps: []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
	}, filepath.Join(root, "skills"))
	require.Len(t, got, 1)
	require.Equal(t, "k8s", got[0].Slug)
	require.FileExists(t, skillPath)
}

func TestMatchingGeneratedSkillsReturnsRelevantActiveSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillDir := filepath.Join(root, "skills")
	activePath := filepath.Join(skillDir, "k8s-investigation", "SKILL.md")
	destructivePath := filepath.Join(skillDir, "destructive-k8s", "SKILL.md")
	genericPath := filepath.Join(skillDir, "git-status", "SKILL.md")
	helmPath := filepath.Join(skillDir, "helm-upgrade", "SKILL.md")
	sensitivePath := filepath.Join(skillDir, "sensitive-k8s", "SKILL.md")
	disabledPath := filepath.Join(skillDir, "disabled-k8s", "SKILL.md")
	outsidePath := filepath.Join(root, "outside", "outside-k8s", "SKILL.md")

	require.NoError(t, os.MkdirAll(filepath.Dir(activePath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(destructivePath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(genericPath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(helmPath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(sensitivePath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(disabledPath), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Dir(outsidePath), 0o750))
	require.NoError(t, os.WriteFile(activePath, []byte("# K8s Investigation\ninspect pods and logs\n"), 0o600))
	require.NoError(t, os.WriteFile(destructivePath, []byte("# Destructive K8s\nrestart or delete resources only when explicitly requested\n"), 0o600))
	require.NoError(t, os.WriteFile(genericPath, []byte("# Git Status\ninspect git status\n"), 0o600))
	require.NoError(t, os.WriteFile(helmPath, []byte("# Helm Upgrade\nupgrade releases only when explicitly requested\n"), 0o600))
	require.NoError(t, os.WriteFile(sensitivePath, []byte("# Sensitive K8s\ninspect secret metadata only when explicitly requested\n"), 0o600))
	require.NoError(t, os.WriteFile(disabledPath, []byte("# Disabled\n"), 0o600))
	require.NoError(t, os.WriteFile(outsidePath, []byte("# Outside\n"), 0o600))

	now := time.Now().UTC()
	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "K8s Investigation Skill",
			Slug:        "k8s-investigation",
			Status:      LearningSkillStatusActive,
			SkillPath:   activePath,
			Steps:       []string{"run kubectl -n {{namespace}} get pods", "run kubectl -n {{namespace}} logs {{pod}}"},
			Occurrences: 4,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Destructive K8s Skill",
			Slug:        "destructive-k8s",
			Status:      LearningSkillStatusActive,
			SkillPath:   destructivePath,
			Steps:       []string{"run kubectl delete pod {{pod}}", "run kubectl delete secret {{secret}}"},
			Occurrences: 100,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Git Status Skill",
			Slug:        "git-status",
			Status:      LearningSkillStatusActive,
			SkillPath:   genericPath,
			Steps:       []string{"run git status --short", "run git diff --stat"},
			Occurrences: 90,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Helm Upgrade Skill",
			Slug:        "helm-upgrade",
			Status:      LearningSkillStatusActive,
			SkillPath:   helmPath,
			Steps:       []string{"run helm upgrade {{release}} {{path}}", "run helm status {{release}}"},
			Occurrences: 102,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Sensitive K8s Skill",
			Slug:        "sensitive-k8s",
			Status:      LearningSkillStatusActive,
			SkillPath:   sensitivePath,
			Steps:       []string{"run kubectl get secret {{secret}}", "run kubectl describe secret {{secret}}"},
			Occurrences: 101,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Disabled K8s Skill",
			Slug:        "disabled-k8s",
			Status:      LearningSkillStatusDisabled,
			SkillPath:   disabledPath,
			Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
			Occurrences: 10,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Stale K8s Skill",
			Slug:        "stale-k8s",
			Status:      LearningSkillStatusActive,
			SkillPath:   filepath.Join(skillDir, "missing", "SKILL.md"),
			Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
			Occurrences: 8,
		},
		{
			CreatedAt:   now,
			UpdatedAt:   now,
			Name:        "Outside K8s Skill",
			Slug:        "outside-k8s",
			Status:      LearningSkillStatusActive,
			SkillPath:   outsidePath,
			Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
			Occurrences: 99,
		},
	}}))

	refs, err := MatchingGeneratedSkills(
		"Please investigate this Kubernetes cluster incident and check the failing pod logs.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "k8s-investigation", refs[0].Slug)
	require.Equal(t, activePath, refs[0].Path)
	require.Contains(t, refs[0].Content, "K8s Investigation")

	refs, err = MatchingGeneratedSkills(
		"Please investigate this Kubernetes incident, but avoid destructive k8s actions.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "k8s-investigation", refs[0].Slug)

	refs, err = MatchingGeneratedSkills(
		"Investigate this kubectl pod incident; avoid delete secret commands.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "k8s-investigation", refs[0].Slug)

	refs, err = MatchingGeneratedSkills(
		"Use the destructive k8s skill.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "destructive-k8s", refs[0].Slug)

	refs, err = MatchingGeneratedSkills(
		"Use the sensitive k8s skill.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "sensitive-k8s", refs[0].Slug)

	refs, err = MatchingGeneratedSkills(
		"Investigate the helm upgrade release failure.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Empty(t, refs)

	refs, err = MatchingGeneratedSkills(
		"Use the helm upgrade skill.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "helm-upgrade", refs[0].Slug)

	refs, err = MatchingGeneratedSkills(
		"Review the application logs from this local file.",
		ReferenceOptions{StoreDir: store.StoreDir(), SkillDir: skillDir},
	)
	require.NoError(t, err)
	require.Empty(t, refs)
}

func TestMatchingGeneratedSkillsRespectsDisableAllAndContentLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store := NewLearningStore(filepath.Join(root, "learning"))
	skillPath := filepath.Join(root, "skills", "k8s-investigation", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o750))
	require.NoError(t, os.WriteFile(skillPath, []byte("# K8s Investigation\nlong body\n"), 0o600))

	now := time.Now().UTC()
	require.NoError(t, store.Save(LearningState{Skills: []GeneratedSkill{{
		CreatedAt:   now,
		UpdatedAt:   now,
		Name:        "K8s Investigation Skill",
		Slug:        "k8s-investigation",
		Status:      LearningSkillStatusActive,
		SkillPath:   skillPath,
		Steps:       []string{"run kubectl get pods", "run kubectl logs {{pod}}"},
		Occurrences: 2,
	}}}))

	refs, err := MatchingGeneratedSkills(
		"Use the k8s investigation skill.",
		ReferenceOptions{StoreDir: store.StoreDir(), MaxBytes: 8},
	)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.True(t, refs[0].Truncated)
	require.Equal(t, "# K8s In", refs[0].Content)

	require.NoError(t, store.SetEnabled(false))
	refs, err = MatchingGeneratedSkills("Use the k8s investigation skill.", ReferenceOptions{StoreDir: store.StoreDir()})
	require.NoError(t, err)
	require.Empty(t, refs)
}

func observeCommands(t *testing.T, learner *Learner, commands ...string) {
	t.Helper()

	for _, command := range commands {
		require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
			Type:      events.CommandExecute,
			Timestamp: time.Now().UTC(),
			Metadata:  map[string]string{"command": command},
		}))
	}
}

func observeSessionCommands(t *testing.T, learner *Learner, sessionID string, commands ...string) {
	t.Helper()

	for _, command := range commands {
		require.NoError(t, learner.ObserveEvent(t.Context(), events.Event{
			Type:      events.CommandExecute,
			Timestamp: time.Now().UTC(),
			SessionID: sessionID,
			Metadata:  map[string]string{"command": command},
		}))
	}
}

func generatedSkillStepsContain(skills []GeneratedSkill, step string) bool {
	for i := range skills {
		if slices.Contains(skills[i].Steps, step) {
			return true
		}
	}

	return false
}
