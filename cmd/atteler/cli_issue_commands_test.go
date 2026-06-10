package main

import (
	"flag"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/symphony"
)

func TestIssueImplementLegacyFlagTracksExplicitEmptyValue(t *testing.T) {
	t.Parallel()

	var opts cliOptions

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	registerCLIFlagsWithFlagSet(fs, &opts)

	require.NoError(t, fs.Parse([]string{"--issue-implement", ""}))

	assert.True(t, opts.issueImplementRequested)
	assert.Empty(t, opts.issueImplementRef)
	require.NotEmpty(t, providerlessIssueCommands())
	assert.True(t, providerlessIssueCommands()[0].match(opts))
}

func TestIssueImplementValidationSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report *symphony.VerificationReport
		want   []string
	}{
		{
			name:   "nil report",
			report: nil,
			want:   nil,
		},
		{
			name: "not configured",
			report: &symphony.VerificationReport{
				Configured: false,
			},
			want: []string{"validation: no local gates configured"},
		},
		{
			name: "passed",
			report: &symphony.VerificationReport{
				Configured: true,
				Passed:     true,
				Gates:      []symphony.VerificationGateResult{{Name: "unit"}},
			},
			want: []string{"validation: passed (1 gate(s))"},
		},
		{
			name: "optional failed",
			report: &symphony.VerificationReport{
				Configured: true,
				Passed:     true,
				Gates: []symphony.VerificationGateResult{
					{Name: "unit", Status: symphony.VerificationPassed, Required: true},
					{Name: "api_key=optional-secret", Status: symphony.VerificationFailed},
				},
			},
			want: []string{
				"validation: passed (2 gate(s))",
				"failed_optional_gates: api_key=[REDACTED]",
			},
		},
		{
			name: "failed required",
			report: &symphony.VerificationReport{
				Configured:     true,
				Passed:         false,
				FailedRequired: []string{"unit", "api_key=gate-secret", " "},
				Gates:          []symphony.VerificationGateResult{{Name: "unit"}},
			},
			want: []string{
				"validation: failed (1 gate(s))",
				"failed_required_gates: unit, api_key=[REDACTED]",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, issueImplementValidationSummary(tt.report))
		})
	}
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestPrintIssueImplementResultReportsDraftValidationFailure(t *testing.T) {
	output := captureIssueImplementStdout(t, func() {
		printIssueImplementResult(symphony.RunResult{
			Status:        symphony.AttemptFailed,
			WorkspacePath: "/tmp/symphony/GH-12",
			Publish: &symphony.PublishResult{
				Branch:                       "symphony/GH-12",
				PullRequestURL:               "https://github.com/owner/repo/pull/7",
				DraftDueToFailedVerification: true,
				Verification: &symphony.VerificationReport{
					Configured:     true,
					Passed:         false,
					FailedRequired: []string{"unit"},
					Gates: []symphony.VerificationGateResult{{
						Name:     "unit",
						Status:   symphony.VerificationFailed,
						Required: true,
					}},
				},
			},
		})
	})

	assert.Contains(t, output, "issue implementation: failed")
	assert.Contains(t, output, "workspace: /tmp/symphony/GH-12")
	assert.Contains(t, output, "branch: symphony/GH-12")
	assert.Contains(t, output, "pull_request: https://github.com/owner/repo/pull/7")
	assert.Contains(t, output, "validation: failed (1 gate(s))")
	assert.Contains(t, output, "failed_required_gates: unit")
	assert.Contains(t, output, "draft_reason: required verification gate failed")
}

//nolint:paralleltest // Temporarily redirects process stdout.
func TestPrintIssueImplementResultReportsIncompleteDraft(t *testing.T) {
	output := captureIssueImplementStdout(t, func() {
		printIssueImplementResult(symphony.RunResult{
			Status:        symphony.AttemptFailed,
			WorkspacePath: "/tmp/symphony/GH-12",
			Publish: &symphony.PublishResult{
				Branch:               "symphony/GH-12",
				PullRequestURL:       "https://github.com/owner/repo/pull/7",
				DraftDueToRunFailure: true,
				Verification: &symphony.VerificationReport{
					Configured:     true,
					Passed:         false,
					FailedRequired: []string{"worker_run"},
					Gates: []symphony.VerificationGateResult{{
						Name:     "worker_run",
						Status:   symphony.VerificationFailed,
						Required: true,
					}},
				},
			},
		})
	})

	assert.Contains(t, output, "issue implementation: failed")
	assert.Contains(t, output, "pull_request: https://github.com/owner/repo/pull/7")
	assert.Contains(t, output, "validation: failed (1 gate(s))")
	assert.Contains(t, output, "failed_required_gates: worker_run")
	assert.Contains(t, output, "draft_reason: implementation incomplete")
}

func captureIssueImplementStdout(t *testing.T, fn func()) string {
	t.Helper()

	original := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = writer

	defer func() {
		os.Stdout = original
		_ = reader.Close()
		_ = writer.Close()
	}()

	fn()

	require.NoError(t, writer.Close())

	data, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(data)
}
