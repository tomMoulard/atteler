package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/llm"
)

var fixedExportedAt = time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)

func shareableTestOptions() ExportOptions {
	return ExportOptions{Profile: ExportProfileShareable, ExportedAt: fixedExportedAt}
}

func credentialLike(value string) string {
	return "sk-" + "proj-" + value + strings.Repeat("x", 24)
}

func TestMarkdown_RendersTranscript(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:           "abc",
		CreatedAt:    time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 4, 30, 10, 5, 0, 0, time.UTC),
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-test",
		AgentLoopBudget: llm.AgentLoopBudget{
			MaxWallTime:     time.Minute,
			MaxOutputBytes:  4096,
			MaxCostMicros:   25_000,
			MaxIterations:   3,
			MaxModelCalls:   4,
			MaxToolCalls:    5,
			MaxInputTokens:  100,
			MaxOutputTokens: 50,
			MaxTotalTokens:  150,
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleAssistant, Content: "hi"},
		},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"# Atteler Session abc",
		"- **Created:** 2026-04-30T10:00:00Z",
		"- **Updated:** 2026-04-30T10:05:00Z",
		"- **Agent:** reviewer",
		"- **Model:** gpt-test",
		"- **Agent loop budget:** iter=3,model=4,tool=5,wall=1m0s,in=100,out=50,total=150,bytes=4096,costµ=25000",
		"## Export Manifest",
		"- **Redaction profile:** redacted-shareable",
		"### User\n\n```text\nhello\n```",
		"### Assistant\n\n```text\nhi\n```",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_EmptyTranscript(t *testing.T) {
	t.Parallel()

	got := MarkdownWithOptions(Session{}, shareableTestOptions())
	assert.Contains(t, got, "_No messages._")
	assert.Contains(t, got, "- **Omitted sections:** none")
}

func TestMarkdown_UsesTitle(t *testing.T) {
	t.Parallel()

	got := MarkdownWithOptions(Session{ID: "abc", Title: "Auth review", Tags: []string{"auth", "review"}}, shareableTestOptions())
	for _, want := range []string{
		"# Auth review",
		"- **Session:** abc",
		"- **Tags:** auth, review",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_RendersNegativeKnowledge(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		NegativeKnowledge: []NegativeKnowledge{
			{
				Approach:  "Patch token refresh timer",
				Reason:    "Created retry storms",
				Commit:    "abc123",
				Agent:     "reviewer",
				TaskType:  "migration",
				Severity:  "critical",
				CreatedAt: time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC),
			},
		},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"## Negative Knowledge",
		"- **Approach:** Patch token refresh timer",
		"  - **Reason:** Created retry storms",
		"  - **Commit:** abc123",
		"  - **Agent:** reviewer",
		"  - **Task Type:** migration",
		"  - **Severity:** critical",
		"  - **Created:** 2026-04-30T11:00:00Z",
		"_No messages._",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdown_RendersEvaluationsAndArtifacts(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		Evaluations: []AgentEvaluation{{
			Agent:           "reviewer",
			Outcome:         "pass",
			Notes:           "caught issue",
			Reference:       "eval.md",
			Source:          EvaluationSourceHarness,
			Evaluator:       "eval-bot",
			RubricVersion:   "review/v2",
			TaskType:        "code-review",
			Difficulty:      "hard",
			ExpectedOutcome: "catch regression",
			Model:           "gpt-test",
			AgentVersion:    "reviewer@abc123",
			SchemaVersion:   AgentEvaluationSchemaVersion,
			Score:           90,
			DurationMillis:  1200,
			Cost:            0.012300,
			Confidence:      0.91,
			CreatedAt:       time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:        "docs/research.md",
			Kind:        "research",
			Summary:     "OAuth notes",
			SourceAgent: "researcher",
			CreatedAt:   time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())
	for _, want := range []string{
		"## Agent Evaluations",
		"- **Agent:** reviewer",
		"  - **Outcome:** pass",
		"  - **Score:** 90",
		"  - **Source:** harness",
		"  - **Evaluator:** eval-bot",
		"  - **Rubric Version:** review/v2",
		"  - **Task Type:** code-review",
		"  - **Difficulty:** hard",
		"  - **Expected Outcome:** catch regression",
		"  - **Model:** gpt-test",
		"  - **Agent Version:** reviewer@abc123",
		"  - **Schema Version:** 1",
		"  - **Duration Millis:** 1200",
		"  - **Cost:** 0.012300",
		"  - **Confidence:** 0.91",
		"## Artifacts",
		"- **Path:** docs/research.md",
		"  - **Kind:** research",
		"  - **Source Agent:** researcher",
	} {
		assert.Contains(t, got, want)
	}
}

func TestMarkdownAndJSON_ExportMultiAgentRunArtifacts(t *testing.T) {
	t.Parallel()

	secret := credentialLike("multirun")
	session := Session{
		ID: "abc",
		MultiAgentRuns: []MultiAgentRun{{
			ID:        "run-1",
			ReceiptID: "receipt-1",
			Kind:      MultiAgentRunKindSpeculation,
			Status:    MultiAgentRunStatusBudgetExhausted,
			Prompt:    "review key=" + secret,
			Model:     "gpt-test",
			StartedAt: time.Date(2026, 4, 30, 14, 10, 0, 0, time.UTC),
			Budget: MultiAgentRunBudget{
				PerCallMaxInputTokens:  1000,
				PerCallMaxOutputTokens: 500,
				MaxRunTotalTokens:      2000,
				MaxRunCostMicros:       900,
				MaxRunWallTimeMS:       30_000,
			},
			Usage: MultiAgentRunUsage{
				ModelCalls:            1,
				CompletedCalls:        1,
				BudgetRejectedCalls:   1,
				EstimatedInputTokens:  42,
				EstimatedOutputTokens: 11,
				EstimatedTotalTokens:  53,
				EstimatedCostMicros:   700,
				InputTokens:           42,
				CachedInputTokens:     4,
				OutputTokens:          11,
				TotalTokens:           53,
				DurationMS:            125,
			},
			Branches: []MultiAgentRunBranch{{
				Name:                 "planner",
				Role:                 "proposal",
				Provenance:           "provider-call:call-001",
				Model:                "gpt-test",
				PromptHash:           "sha256:abc123",
				Status:               MultiAgentRunStatusBudgetExhausted,
				InputTokenEstimate:   42,
				OutputTokenEstimate:  11,
				ContextWindow:        4096,
				MaxOutputTokens:      100,
				InputTokens:          42,
				CachedInputTokens:    4,
				OutputTokens:         11,
				TotalTokens:          53,
				EstimatedCostMicros:  700,
				DurationMS:           100,
				BudgetRejectionRule:  "budget.max_run_total_tokens",
				BudgetRejectionUsage: 2100,
				BudgetRejectionLimit: 2000,
			}},
			Reviewers: []MultiAgentRunReviewer{{
				Name:        "critic",
				Role:        "cross-review",
				TargetAgent: "planner",
				Model:       "gpt-test",
				PromptHash:  "sha256:def456",
				CallID:      "call-002",
			}},
			Calls: []MultiAgentRunCall{{
				ID:                   "call-001",
				Phase:                "proposal",
				Agent:                "planner",
				Status:               MultiAgentRunStatusBudgetExhausted,
				RequestedModel:       "gpt-test",
				ResponseModel:        "backup-test",
				FallbackModels:       []string{"backup-test"},
				PromptHash:           "sha256:abc123",
				SystemPrompt:         "system prompt",
				UserPrompt:           "user prompt key=" + secret,
				Response:             "proposal body",
				InputTokenEstimate:   42,
				ContextWindow:        4096,
				MaxOutputTokens:      100,
				InputTokens:          42,
				CachedInputTokens:    4,
				OutputTokens:         11,
				TotalTokens:          53,
				EstimatedCostMicros:  700,
				DurationMS:           100,
				BudgetRejectionRule:  "budget.max_run_total_tokens",
				BudgetRejectionUsage: 2100,
				BudgetRejectionLimit: 2000,
			}},
			Artifacts: []MultiAgentRunArtifact{{
				Kind:    "proposal",
				Phase:   "proposal",
				Agent:   "planner",
				Content: "proposal body key=" + secret,
				Index:   1,
				Metadata: map[string]string{
					"call_id":               "call-001",
					"raw_provider_response": "true",
				},
			}},
			Gates: []MultiAgentRunGate{{
				Name:   "tests pass",
				Phase:  "aggregate-verdict",
				Agent:  "planner",
				Passed: false,
				Notes:  "tests not run",
			}},
			Decisions: []MultiAgentRunDecision{{
				Kind:      "proposal",
				Phase:     "proposal",
				Agent:     "planner",
				Outcome:   "accepted",
				Rationale: "best evidence key=" + secret,
				Index:     1,
			}},
			Disagreements: []MultiAgentRunDisagreement{{
				Phase:       "cross-review",
				Reviewer:    "critic",
				TargetAgent: "planner",
				Subject:     "cross-review",
				Notes:       "needs tests key=" + secret,
				Index:       1,
			}},
			Errors: []MultiAgentRunError{{
				Stage:       "aggregate-verdict",
				Reviewer:    "judge",
				TargetAgent: "planner",
				Message:     "parse failed key=" + secret,
			}},
			Summary:            MultiAgentRunSummary{Winner: "planner", Reason: "best evidence"},
			CancellationReason: "canceled by operator key=" + secret,
			ResumeReason:       "continue from partial receipt key=" + secret,
			Error:              "gate failed key=" + secret,
		}},
	}

	options := shareableTestOptions()
	markdown := MarkdownWithOptions(session, options)
	data, err := JSONWithOptions(session, options)
	require.NoError(t, err)

	var decoded MachineReadableExport
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.MultiAgentRuns, 1)
	assert.Equal(t, 1, decoded.Session.MultiAgentRunCount)
	assert.Equal(t, "run-1", decoded.MultiAgentRuns[0].ID)
	assert.Equal(t, "receipt-1", decoded.MultiAgentRuns[0].ReceiptID)
	assert.Equal(t, MultiAgentRunStatusBudgetExhausted, decoded.MultiAgentRuns[0].Status)
	assert.Equal(t, int64(900), decoded.MultiAgentRuns[0].Budget.MaxRunCostMicros)
	assert.Equal(t, int64(30_000), decoded.MultiAgentRuns[0].Budget.MaxRunWallTimeMS)
	assert.Equal(t, int64(700), decoded.MultiAgentRuns[0].Usage.EstimatedCostMicros)
	assert.Equal(t, int64(125), decoded.MultiAgentRuns[0].Usage.DurationMS)
	assert.Equal(t, 4, decoded.MultiAgentRuns[0].Usage.CachedInputTokens)
	assert.Equal(t, 1, decoded.MultiAgentRuns[0].Usage.BudgetRejectedCalls)
	assert.Equal(t, 42, decoded.MultiAgentRuns[0].Branches[0].InputTokenEstimate)
	assert.Equal(t, 11, decoded.MultiAgentRuns[0].Branches[0].OutputTokenEstimate)
	assert.Equal(t, 4096, decoded.MultiAgentRuns[0].Branches[0].ContextWindow)
	assert.Equal(t, 100, decoded.MultiAgentRuns[0].Branches[0].MaxOutputTokens)
	assert.Equal(t, 4, decoded.MultiAgentRuns[0].Branches[0].CachedInputTokens)
	assert.Equal(t, "budget.max_run_total_tokens", decoded.MultiAgentRuns[0].Branches[0].BudgetRejectionRule)
	assert.Equal(t, 2100, decoded.MultiAgentRuns[0].Branches[0].BudgetRejectionUsage)
	assert.Equal(t, 2000, decoded.MultiAgentRuns[0].Branches[0].BudgetRejectionLimit)
	require.Len(t, decoded.MultiAgentRuns[0].Reviewers, 1)
	assert.Equal(t, "critic", decoded.MultiAgentRuns[0].Reviewers[0].Name)
	assert.Equal(t, "sha256:def456", decoded.MultiAgentRuns[0].Reviewers[0].PromptHash)
	assert.Equal(t, "sha256:abc123", decoded.MultiAgentRuns[0].Calls[0].PromptHash)
	assert.Equal(t, 100, decoded.MultiAgentRuns[0].Calls[0].MaxOutputTokens)
	assert.Equal(t, int64(700), decoded.MultiAgentRuns[0].Calls[0].EstimatedCostMicros)
	assert.Equal(t, int64(100), decoded.MultiAgentRuns[0].Calls[0].DurationMS)
	assert.Equal(t, 2100, decoded.MultiAgentRuns[0].Calls[0].BudgetRejectionUsage)
	assert.Equal(t, 2000, decoded.MultiAgentRuns[0].Calls[0].BudgetRejectionLimit)
	assert.Equal(t, "proposal", decoded.MultiAgentRuns[0].Artifacts[0].Kind)
	assert.Equal(t, 1, decoded.MultiAgentRuns[0].Artifacts[0].Index)
	assert.Equal(t, "tests pass", decoded.MultiAgentRuns[0].Gates[0].Name)
	require.Len(t, decoded.MultiAgentRuns[0].Disagreements, 1)
	assert.Equal(t, "critic", decoded.MultiAgentRuns[0].Disagreements[0].Reviewer)
	assert.Equal(t, 1, decoded.MultiAgentRuns[0].Disagreements[0].Index)
	assert.Contains(t, decoded.MultiAgentRuns[0].Disagreements[0].Notes, "[REDACTED_API_KEY]")
	require.Len(t, decoded.MultiAgentRuns[0].Errors, 1)
	assert.Equal(t, "aggregate-verdict", decoded.MultiAgentRuns[0].Errors[0].Stage)
	assert.Equal(t, "judge", decoded.MultiAgentRuns[0].Errors[0].Reviewer)
	assert.Equal(t, "planner", decoded.MultiAgentRuns[0].Errors[0].TargetAgent)
	assert.Contains(t, decoded.MultiAgentRuns[0].Errors[0].Message, "[REDACTED_API_KEY]")
	assert.Equal(t, "accepted", decoded.MultiAgentRuns[0].Decisions[0].Outcome)
	assert.Equal(t, 1, decoded.MultiAgentRuns[0].Decisions[0].Index)
	assert.Empty(t, decoded.MultiAgentRuns[0].Summary.Winner)
	assert.Empty(t, decoded.MultiAgentRuns[0].Summary.Reason)
	assert.Contains(t, decoded.Manifest.ContentHashes, "multi_agent_runs")
	assert.Contains(t, markdown, "## Multi-agent Runs")
	assert.Contains(t, markdown, "**speculation:** `run-1`")
	assert.NotContains(t, markdown, "**Winner:** planner")
	assert.NotContains(t, markdown, "**Reason:** best evidence")
	assert.Contains(t, markdown, `**Prompt:** review key=\[REDACTED\_API\_KEY\]`)
	assert.Contains(t, markdown, "**Budget:** per_call_max_input_tokens=1000")
	assert.Contains(t, markdown, "per_call_max_output_tokens=500")
	assert.Contains(t, markdown, "max_run_total_tokens=2000")
	assert.Contains(t, markdown, "max_run_cost_micros=900")
	assert.Contains(t, markdown, "max_run_wall_time_ms=30000")
	assert.Contains(t, markdown, "**Cancellation reason:**")
	assert.Contains(t, markdown, "**Resume reason:**")
	assert.Contains(t, markdown, "budget_rejected_calls=1")
	assert.Contains(t, markdown, "input_estimate=42")
	assert.Contains(t, markdown, "output_estimate=11")
	assert.Contains(t, markdown, "context_window=4096")
	assert.Contains(t, markdown, "max_output_tokens=100")
	assert.Contains(t, markdown, "cached_input_tokens=4")
	assert.Contains(t, markdown, "estimated_total_tokens=53")
	assert.Contains(t, markdown, "estimated_cost_micros=700")
	assert.Contains(t, markdown, "total_tokens=53")
	assert.Contains(t, markdown, "provenance=provider-call:call-001")
	assert.Contains(t, markdown, "prompt_hash=sha256:abc123")
	assert.Contains(t, markdown, "budget_rejection=budget.max\\_run\\_total\\_tokens used=2100 limit=2000")
	assert.Contains(t, markdown, "**Reviewers:**")
	assert.Contains(t, markdown, "critic role=cross-review target=planner model=gpt-test prompt_hash=sha256:def456 call=call-002")
	assert.Contains(t, markdown, "**Disagreements:**")
	assert.Contains(t, markdown, "reviewer=critic target=planner subject=cross-review")
	assert.Contains(t, markdown, "subject=cross-review index=1")
	assert.Contains(t, markdown, "**Workflow errors:**")
	assert.Contains(t, markdown, "stage=aggregate-verdict reviewer=judge target=planner")
	assert.Contains(t, markdown, "parse failed key=\\[REDACTED\\_API\\_KEY\\]")
	assert.Contains(t, markdown, "**Decisions:**")
	assert.Contains(t, markdown, "proposal accepted phase=proposal agent=planner index=1")
	assert.Contains(t, markdown, "model=backup-test")
	assert.Contains(t, markdown, "requested_model=gpt-test")
	assert.Contains(t, markdown, "response_model=backup-test")
	assert.Contains(t, markdown, "fallback_models=backup-test")
	assert.Contains(t, markdown, "max_output_tokens=100")
	assert.Contains(t, markdown, "context_window=4096")
	assert.Contains(t, markdown, "input_tokens=42 cached_input_tokens=4 output_tokens=11 total_tokens=53")
	assert.Contains(t, markdown, "duration_ms=100")
	assert.Contains(t, markdown, "budget_rejection=budget.max\\_run\\_total\\_tokens used=2100 limit=2000")
	assert.Contains(t, markdown, "system_prompt=system prompt")
	assert.Contains(t, markdown, "response=proposal body")
	assert.Contains(t, markdown, "proposal phase=proposal agent=planner target= index=1")
	assert.Contains(t, markdown, "metadata.call\\_id=call-001")
	assert.Contains(t, markdown, "metadata.raw\\_provider\\_response=true")
	assert.NotContains(t, string(data), secret)
	assert.NotContains(t, markdown, secret)
}

func TestMarkdownAndJSON_ExportMultiAgentRunSummaryRequiresAcceptedOutput(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		MultiAgentRuns: []MultiAgentRun{
			{
				ID:      "run-running",
				Kind:    MultiAgentRunKindSpeculation,
				Status:  MultiAgentRunStatusRunning,
				Summary: MultiAgentRunSummary{Winner: "stale", Reason: "not terminal"},
				Artifacts: []MultiAgentRunArtifact{{
					Kind:    "verdict",
					Phase:   "aggregate-verdict",
					Agent:   "judge",
					Content: "winner: stale",
					Index:   1,
				}},
				Decisions: []MultiAgentRunDecision{{
					Kind:    "verdict",
					Phase:   "aggregate-verdict",
					Agent:   "judge",
					Outcome: "accepted",
					Index:   1,
				}},
			},
			{
				ID:      "run-missing-artifact",
				Kind:    MultiAgentRunKindReview,
				Status:  MultiAgentRunStatusCompleted,
				Summary: MultiAgentRunSummary{VerdictReviewer: "missing-artifact", Findings: 2},
				Decisions: []MultiAgentRunDecision{{
					Kind:    "verdict",
					Outcome: "accepted",
				}},
			},
			{
				ID:     "run-final",
				Kind:   MultiAgentRunKindSpeculation,
				Status: MultiAgentRunStatusCompleted,
				Summary: MultiAgentRunSummary{
					Winner: "final",
					Reason: "accepted aggregate evidence",
				},
				Artifacts: []MultiAgentRunArtifact{{
					Kind:    "verdict",
					Phase:   "aggregate-verdict",
					Agent:   "judge",
					Content: "winner: final\nreason: accepted aggregate evidence",
					Index:   1,
				}},
				Decisions: []MultiAgentRunDecision{{
					Kind:      "verdict",
					Phase:     "aggregate-verdict",
					Agent:     "judge",
					Outcome:   "accepted",
					Rationale: "accepted aggregate evidence",
					Index:     1,
				}},
			},
		},
	}

	options := shareableTestOptions()
	export := BuildMachineReadableExport(session, options)
	require.Len(t, export.MultiAgentRuns, 3)
	assert.Empty(t, export.MultiAgentRuns[0].Summary.Winner)
	assert.Empty(t, export.MultiAgentRuns[0].Summary.Reason)
	assert.Empty(t, export.MultiAgentRuns[1].Summary.VerdictReviewer)
	assert.Zero(t, export.MultiAgentRuns[1].Summary.Findings)
	assert.Equal(t, "final", export.MultiAgentRuns[2].Summary.Winner)
	assert.Equal(t, "accepted aggregate evidence", export.MultiAgentRuns[2].Summary.Reason)

	markdown := MarkdownWithOptions(session, options)
	assert.NotContains(t, markdown, "**Winner:** stale")
	assert.NotContains(t, markdown, "**Verdict reviewer:** missing-artifact")
	assert.Contains(t, markdown, "**Winner:** final")
	assert.Contains(t, markdown, "**Reason:** accepted aggregate evidence")
}

func TestMarkdown_DefaultShareableRedactsSecretsAndAbsolutePaths(t *testing.T) {
	t.Parallel()

	const (
		openAIKey    = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"
		bearerToken  = "abcdefghijklmnopqrstuvwxyz1234567890"
		customSecret = "ultra-private-value"
		quotedSecret = "two words secret"
	)

	dsnMarker := "p" + "@" + "ssw0rd"
	awsMarker := "aws" + strings.Repeat("x", 40)
	githubMarker := "github" + strings.Repeat("x", 40)
	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		DefaultModel: openAIKey,
		Tags:         []string{"token=" + customSecret},
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: strings.Join([]string{
				"Authorization: Bearer " + bearerToken,
				"OPENAI_API_KEY=" + openAIKey,
				"AWS_SECRET_ACCESS_KEY=" + awsMarker,
				"GITHUB_TOKEN=" + githubMarker,
				"DATABASE_URL=postgres://alice:" + dsnMarker + "@db.example/app",
				"tenant_secret=" + customSecret,
				"password=\"" + quotedSecret + "\"",
				"read /Users/tom/work/private-repo/.env",
				"temp /tmp",
				"home ~/private/project/.env",
				"path:/Users/tom/work/private-repo/config.yaml",
				"file='/Users/tom/My Project/config.yaml'",
				"root=\"C:\\Users\\tom\\My Project\"",
				"open file:///Users/tom/work/private-repo/config.yaml",
				"share \\\\server\\share\\secret.txt",
				"Windows C:\\Users\\tom\\secret.txt",
			}, "\n"),
		}},
		Artifacts: []Artifact{{Path: "/Users/tom/work/private-repo/report.md", Kind: "note"}},
	}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfileShareable,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})

	assert.NotContains(t, got, openAIKey)
	assert.NotContains(t, got, bearerToken)
	assert.NotContains(t, got, awsMarker)
	assert.NotContains(t, got, githubMarker)
	assert.NotContains(t, got, dsnMarker)
	assert.NotContains(t, got, "alice:"+dsnMarker)
	assert.NotContains(t, got, customSecret)
	assert.NotContains(t, got, quotedSecret)
	assert.NotContains(t, got, "/Users/tom")
	assert.NotContains(t, got, "/tmp")
	assert.NotContains(t, got, "~/private")
	assert.NotContains(t, got, "My Project")
	assert.NotContains(t, got, `\\server\share`)
	assert.NotContains(t, got, `C:\Users\tom`)
	assert.Contains(t, got, "[REDACTED]")
	assert.Contains(t, got, "[REDACTED_PATH]")
}

func TestBuildMachineReadableExport_ExcludedFieldsOmitByPolicy(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:           "abc",
		Title:        "Private title",
		CreatedAt:    time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC),
		DefaultAgent: "reviewer",
		DefaultModel: "gpt-secret",
		WorktreePath: "/private/repo",
		Tags:         []string{"private-tag"},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "private transcript"}},
		NegativeKnowledge: []NegativeKnowledge{{
			Approach:  "private failed approach",
			Reason:    "private failure reason",
			Agent:     "reviewer",
			CreatedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Evaluations: []AgentEvaluation{{
			Agent:     "reviewer",
			Outcome:   "private evaluation",
			CreatedAt: time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:      "/private/artifact.md",
			Kind:      "private artifact",
			Summary:   "private artifact summary",
			CreatedAt: time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
		}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:    ExportProfilePrivate,
		ExportedAt: fixedExportedAt,
		ExcludedFields: []SearchField{
			SearchFieldTranscript,
			SearchFieldFailures,
			SearchFieldEvaluations,
			SearchFieldArtifacts,
			SearchFieldTags,
			SearchFieldRepo,
			SearchFieldAgent,
			SearchFieldModel,
			SearchFieldDate,
			SearchFieldSession,
			SearchFieldTitle,
		},
	})

	assert.Empty(t, export.Messages)
	assert.Empty(t, export.NegativeKnowledge)
	assert.Empty(t, export.Evaluations)
	assert.Empty(t, export.Artifacts)
	assert.Empty(t, export.Session.Tags)
	assert.Empty(t, export.Session.WorktreePath)
	assert.Empty(t, export.Session.DefaultAgent)
	assert.Empty(t, export.Session.DefaultModel)
	assert.Empty(t, export.Session.ID)
	assert.Empty(t, export.Session.Title)
	assert.Empty(t, export.Manifest.SessionID)
	assert.True(t, export.Manifest.ExportedAt.IsZero())
	assert.True(t, export.Session.CreatedAt.IsZero())
	assert.True(t, export.Session.UpdatedAt.IsZero())
	assert.Equal(t, 1, export.Session.MessageCount)
	assert.Equal(t, 1, export.Session.NegativeKnowledgeCount)
	assert.Equal(t, 1, export.Session.EvaluationCount)
	assert.Equal(t, 1, export.Session.ArtifactCount)
	assert.Contains(t, export.Manifest.OmittedSections, "transcript omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "negative knowledge omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "evaluations omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "artifacts omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "session.tags omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "session.worktree_path omitted by export field policy")
	assert.Contains(t, export.Manifest.OmittedSections, "manifest.exported_at omitted by export field policy")
}

func TestBuildMachineReadableExport_IncludesAgentLoopBudget(t *testing.T) {
	t.Parallel()

	budget := llm.AgentLoopBudget{
		MaxWallTime:     2 * time.Minute,
		MaxOutputBytes:  4096,
		MaxCostMicros:   25_000,
		MaxIterations:   7,
		MaxModelCalls:   5,
		MaxToolCalls:    9,
		MaxInputTokens:  100,
		MaxOutputTokens: 50,
		MaxTotalTokens:  150,
	}
	session := Session{
		ID:              "abc",
		AgentLoopBudget: budget,
	}

	export := BuildMachineReadableExport(session, shareableTestOptions())
	assert.Equal(t, budget, export.Session.AgentLoopBudget)

	data, err := JSONWithOptions(session, shareableTestOptions())
	require.NoError(t, err)
	assert.Contains(t, string(data), `"agent_loop_budget"`)

	var decoded MachineReadableExport
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, budget, decoded.Session.AgentLoopBudget)
}

func TestBuildMachineReadableExport_MetadataPolicyDoesNotLeakNestedFields(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		NegativeKnowledge: []NegativeKnowledge{{
			Approach:  "cache patch",
			Reason:    "broke auth",
			Agent:     "reviewer",
			CreatedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		}},
		Evaluations: []AgentEvaluation{{
			Agent:     "reviewer",
			Outcome:   "pass",
			Notes:     "retained notes",
			CreatedAt: time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC),
		}},
		Artifacts: []Artifact{{
			Path:        "docs/research.md",
			Kind:        "research",
			SourceAgent: "reviewer",
			CreatedAt:   time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC),
		}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:        ExportProfilePrivate,
		ExportedAt:     fixedExportedAt,
		ExcludedFields: []SearchField{SearchFieldAgent, SearchFieldDate},
	})

	require.Len(t, export.NegativeKnowledge, 1)
	require.Len(t, export.Evaluations, 1)
	require.Len(t, export.Artifacts, 1)
	assert.Equal(t, "cache patch", export.NegativeKnowledge[0].Approach)
	assert.Equal(t, "retained notes", export.Evaluations[0].Notes)
	assert.Equal(t, "docs/research.md", export.Artifacts[0].Path)
	assert.Empty(t, export.NegativeKnowledge[0].Agent)
	assert.Empty(t, export.Evaluations[0].Agent)
	assert.Empty(t, export.Artifacts[0].SourceAgent)
	assert.True(t, export.NegativeKnowledge[0].CreatedAt.IsZero())
	assert.True(t, export.Evaluations[0].CreatedAt.IsZero())
	assert.True(t, export.Artifacts[0].CreatedAt.IsZero())
}

func TestMarkdown_DefaultUsesShareableProfile(t *testing.T) {
	t.Parallel()

	secret := credentialLike("default")
	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/private",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "key=" + secret + " path=/Users/tom/private/.env"}},
	}

	got := Markdown(session)

	assert.Contains(t, got, "- **Redaction profile:** redacted-shareable")
	assert.Contains(t, got, "[REDACTED_PATH]")
	assert.NotContains(t, got, secret)
	assert.NotContains(t, got, "/Users/tom")
}

func TestMarkdown_PrivateProfileIsExplicitAndFullFidelity(t *testing.T) {
	t.Parallel()

	const openAIKey = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "OPENAI_API_KEY=" + openAIKey + " at /Users/tom/work/private-repo/.env",
		}},
	}

	got := MarkdownWithOptions(session, ExportOptions{Profile: ExportProfilePrivate, ExportedAt: fixedExportedAt})

	assert.Contains(t, got, "Private full-fidelity export")
	assert.Contains(t, got, "- **Redaction profile:** private-full")
	assert.Contains(t, got, "- **Privacy notice:** Private full-fidelity export")
	assert.Contains(t, got, openAIKey)
	assert.Contains(t, got, "/Users/tom/work/private-repo/.env")
}

func TestMarkdown_PrivateProfileHonorsSensitiveFieldPolicy(t *testing.T) {
	t.Parallel()

	const (
		secret     = "ultra-private-value"
		toolSecret = "tool-private-value"
	)

	session := Session{
		ID:           "abc",
		WorktreePath: "/Users/tom/work/private-repo",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "tenant_secret=" + secret + " path=/Users/tom/work/private-repo/.env",
			ToolCalls: []llm.ToolCall{{
				ID:    "call-1",
				Name:  "read_file",
				Input: map[string]any{"tenant_secret": toolSecret},
			}},
			ToolResult: &llm.ToolResult{ToolCallID: "call-1", Content: "tenant_secret=" + toolSecret},
		}},
	}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})
	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"tenant_secret"},
	})

	assert.Contains(t, got, "Private export with sensitive-field redaction")
	assert.Contains(t, got, "[REDACTED]")
	assert.Contains(t, got, "redacted private export")
	assert.NotContains(t, got, secret)
	assert.NotContains(t, got, toolSecret)
	assert.NotContains(t, got, "/Users/tom")
	require.Len(t, export.Messages, 1)
	assert.Empty(t, export.Messages[0].ToolCalls)
	assert.Nil(t, export.Messages[0].ToolResult)
	assert.Equal(t, 1, export.Messages[0].ToolCallCount)
	assert.True(t, export.Messages[0].ToolResultOmitted)
	assert.NotContains(t, export.Messages[0].Content, secret)
	assert.Contains(t, export.Manifest.PrivacyNotice, "sensitive-field redaction")
}

func TestBuildMachineReadableExport_SensitiveFieldNamesRedactWholeValues(t *testing.T) {
	t.Parallel()

	const (
		rawModel   = "raw-model-field-secret"
		rawTag     = "raw-tag-field-secret"
		rawContent = "raw-content-field-secret"
	)

	session := Session{
		ID:           "abc",
		DefaultModel: rawModel,
		Tags:         []string{rawTag},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: rawContent}},
	}

	export := BuildMachineReadableExport(session, ExportOptions{
		Profile:         ExportProfilePrivate,
		ExportedAt:      fixedExportedAt,
		SensitiveFields: []string{"default_model", "tags", "content"},
	})
	data, err := json.Marshal(export)
	require.NoError(t, err)

	assert.Equal(t, "[REDACTED]", export.Session.DefaultModel)
	assert.Equal(t, []string{"[REDACTED]"}, export.Session.Tags)
	require.Len(t, export.Messages, 1)
	assert.Equal(t, "[REDACTED]", export.Messages[0].Content)
	assert.NotContains(t, string(data), rawModel)
	assert.NotContains(t, string(data), rawTag)
	assert.NotContains(t, string(data), rawContent)
	assert.Contains(t, export.Manifest.PrivacyNotice, "sensitive-field redaction")
}

func TestMarkdown_FencesTranscriptAndEscapesInlineInjection(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:    "abc",
		Title: "Review\n## injected title <script>",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "before\n```\n## injected\n<script>alert(1)</script>\n```",
		}},
		Evaluations: []AgentEvaluation{{Agent: "reviewer", Outcome: "pass", Notes: "ok\n## injected note"}},
	}

	got := MarkdownWithOptions(session, shareableTestOptions())

	assert.Contains(t, got, "# Review \\#\\# injected title &lt;script&gt;")
	assert.NotContains(t, got, "Review\n## injected title")
	assert.Contains(t, got, "  - **Notes:** ok \\#\\# injected note")
	assert.NotContains(t, got, "  - **Notes:** ok\n## injected note")
	assert.Contains(t, got, "````text\nbefore\n```\n## injected\n<script>alert(1)</script>\n```\n````")
}

func TestMarkdown_TruncatesHugeTranscriptContent(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("a", 30)
	session := Session{ID: "abc", Messages: []llm.Message{{Role: llm.RoleUser, Content: content}}}

	got := MarkdownWithOptions(session, ExportOptions{
		Profile:         ExportProfileShareable,
		ExportedAt:      fixedExportedAt,
		MaxContentRunes: 10,
	})

	assert.NotContains(t, got, content)
	assert.Contains(t, got, "aaaaaaaaaa\n\n[Truncated: omitted 20 runes]")
	assert.Contains(t, got, "truncated by 20 runes")
}

func TestMarkdown_LimitsTranscriptMessagesAndOmitsToolAttachments(t *testing.T) {
	t.Parallel()

	omittedMarker := credentialLike("omittedmessage")
	callMarker := credentialLike("toolcall")
	resultMarker := credentialLike("toolresult")

	session := Session{
		ID: "abc",
		Messages: []llm.Message{
			{
				Role:    llm.RoleAssistant,
				Content: "using a tool",
				ToolCalls: []llm.ToolCall{{
					ID:    "call-1",
					Name:  "read_file",
					Input: map[string]any{"path": "/Users/tom/private.txt", "token": callMarker},
				}},
				ToolResult: &llm.ToolResult{ToolCallID: "call-1", Content: "result " + resultMarker},
			},
			{Role: llm.RoleUser, Content: "keep this"},
			{Role: llm.RoleUser, Content: "omit this " + omittedMarker},
		},
	}

	options := ExportOptions{
		Profile:               ExportProfileShareable,
		ExportedAt:            fixedExportedAt,
		MaxTranscriptMessages: 2,
	}
	got := MarkdownWithOptions(session, options)
	export := BuildMachineReadableExport(session, options)

	require.Len(t, export.Messages, 2)
	assert.Equal(t, 1, export.Messages[0].ToolCallCount)
	assert.True(t, export.Messages[0].ToolResultOmitted)
	assert.Contains(t, got, "transcript messages 3-3 omitted by message limit 2")
	assert.Contains(t, got, "messages\\[1\\].tool\\_calls omitted from shareable export")
	assert.Contains(t, got, "messages\\[1\\].tool\\_result omitted from shareable export")
	assert.NotContains(t, got, omittedMarker)
	assert.NotContains(t, got, callMarker)
	assert.NotContains(t, got, resultMarker)
	assert.NotContains(t, got, "/Users/tom")
}

func TestIssueMarkdown_OmitsTranscriptBodies(t *testing.T) {
	t.Parallel()

	session := Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "private prompt body"}},
	}

	got := MarkdownWithOptions(session, ExportOptions{Profile: ExportProfileIssue, ExportedAt: fixedExportedAt})

	assert.Contains(t, got, "## Issue/PR Summary")
	assert.Contains(t, got, "transcript omitted by issue/PR summary profile")
	assert.Contains(t, got, "- **Messages:** 1 total, 0 exported")
	assert.NotContains(t, got, "private prompt body")
	assert.NotContains(t, got, "## Transcript")
}

func TestIssueExport_OmitsMultiAgentRunBodies(t *testing.T) {
	t.Parallel()

	session := Session{
		ID: "abc",
		MultiAgentRuns: []MultiAgentRun{{
			ID:     "run-1",
			Kind:   MultiAgentRunKindSpeculation,
			Status: MultiAgentRunStatusBudgetExhausted,
			Prompt: "private run prompt",
			Calls: []MultiAgentRunCall{{
				ID:           "call-001",
				Phase:        "proposal",
				Agent:        "planner",
				Status:       MultiAgentRunStatusCompleted,
				PromptHash:   "sha256:abc",
				SystemPrompt: "private system prompt",
				UserPrompt:   "private user prompt",
				Response:     "private provider response",
			}},
			Artifacts: []MultiAgentRunArtifact{{
				Kind:    "proposal",
				Phase:   "proposal",
				Agent:   "planner",
				Content: "private proposal artifact",
			}},
			Disagreements: []MultiAgentRunDisagreement{{
				Phase:       "cross-review",
				Reviewer:    "critic",
				TargetAgent: "planner",
				Subject:     "cross-review",
				Notes:       "private disagreement note",
			}},
			Errors: []MultiAgentRunError{{
				Stage:    "aggregate-verdict",
				Reviewer: "judge",
				Message:  "structured verdict parse failed",
			}},
		}},
	}
	options := ExportOptions{Profile: ExportProfileIssue, ExportedAt: fixedExportedAt}

	markdown := MarkdownWithOptions(session, options)
	data, err := JSONWithOptions(session, options)
	require.NoError(t, err)

	var decoded MachineReadableExport
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded.MultiAgentRuns, 1)
	require.Len(t, decoded.MultiAgentRuns[0].Calls, 1)
	require.Len(t, decoded.MultiAgentRuns[0].Artifacts, 1)
	require.Len(t, decoded.MultiAgentRuns[0].Disagreements, 1)
	require.Len(t, decoded.MultiAgentRuns[0].Errors, 1)
	assert.Empty(t, decoded.MultiAgentRuns[0].Prompt)
	assert.Empty(t, decoded.MultiAgentRuns[0].Calls[0].SystemPrompt)
	assert.Empty(t, decoded.MultiAgentRuns[0].Calls[0].UserPrompt)
	assert.Empty(t, decoded.MultiAgentRuns[0].Calls[0].Response)
	assert.Empty(t, decoded.MultiAgentRuns[0].Artifacts[0].Content)
	assert.Empty(t, decoded.MultiAgentRuns[0].Disagreements[0].Notes)
	assert.Equal(t, "sha256:abc", decoded.MultiAgentRuns[0].Calls[0].PromptHash)
	assert.Equal(t, "critic", decoded.MultiAgentRuns[0].Disagreements[0].Reviewer)
	assert.Equal(t, "structured verdict parse failed", decoded.MultiAgentRuns[0].Errors[0].Message)

	for _, privateText := range []string{
		"private run prompt",
		"private system prompt",
		"private user prompt",
		"private provider response",
		"private proposal artifact",
		"private disagreement note",
	} {
		assert.NotContains(t, markdown, privateText)
		assert.NotContains(t, string(data), privateText)
	}

	assert.Contains(t, markdown, "multi-agent run prompts omitted by issue/PR summary profile")
	assert.Contains(t, markdown, "multi-agent run provider-call prompts/responses omitted by issue/PR summary profile")
	assert.Contains(t, markdown, "multi-agent run artifact contents omitted by issue/PR summary profile")
	assert.Contains(t, markdown, "multi-agent run disagreement notes omitted by issue/PR summary profile")
	assert.Contains(t, markdown, "**Workflow errors:**")
	assert.Contains(t, markdown, "structured verdict parse failed")
}

func TestJSON_MachineReadableExportMatchesMarkdownRedaction(t *testing.T) {
	t.Parallel()

	const openAIKey = "sk-proj-abcdefghijklmnopqrstuvwx1234567890"

	session := Session{
		ID:       "abc",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "key=" + openAIKey + " path=/Users/tom/project"}},
	}
	options := shareableTestOptions()

	markdown := MarkdownWithOptions(session, options)
	data, err := JSONWithOptions(session, options)
	require.NoError(t, err)

	var decoded MachineReadableExport
	require.NoError(t, json.Unmarshal(data, &decoded))

	built := BuildMachineReadableExport(session, options)
	assert.Equal(t, built.Manifest.ContentHashes, decoded.Manifest.ContentHashes)
	require.Len(t, decoded.Messages, 1)
	assert.Equal(t, built.Messages[0].Content, decoded.Messages[0].Content)
	assert.Contains(t, markdown, decoded.Messages[0].Content)
	assert.Equal(t, ExportProfileShareable, decoded.Manifest.RedactionProfile)
	assert.Empty(t, decoded.Manifest.OmittedSections)
	assert.Contains(t, string(data), `"omitted_sections": []`)
	assert.NotContains(t, string(data), openAIKey)
	assert.NotContains(t, string(data), "/Users/tom")
	assert.NotContains(t, markdown, openAIKey)
	assert.NotContains(t, markdown, "/Users/tom")
}
