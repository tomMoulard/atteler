package main

type sessionReadCommandInput struct {
	ReplayRef         string
	ShowSessionRef    string
	SummarySessionRef string
	ExportRef         string
	ExportFormat      string
	ListArtifacts     bool
	ListEvaluations   bool
	ListFailures      bool
	ListMessages      bool
}

func sessionReadCommandInputFromOptions(opts cliOptions) sessionReadCommandInput {
	return sessionReadCommandInput{
		ReplayRef:         opts.replayRef,
		ShowSessionRef:    opts.showSessionRef,
		SummarySessionRef: opts.summarySessionRef,
		ExportRef:         opts.exportRef,
		ExportFormat:      opts.exportFormat,
		ListArtifacts:     opts.listArtifacts,
		ListEvaluations:   opts.listEvaluations,
		ListFailures:      opts.listFailures,
		ListMessages:      opts.listMessages,
	}
}

//nolint:govet // field order follows session write flags; value is short-lived.
type sessionWriteCommandInput struct {
	RecordFailure       string
	FailureReason       string
	FailureCommit       string
	RecordEvaluation    string
	EvaluationOutcome   string
	EvaluationNotes     string
	EvaluationReference string
	EvaluationScore     int
	RecordArtifact      string
	ArtifactKind        string
	ArtifactSummary     string
	FeedbackApplyConfig string
	FeedbackHistoryPath string
}

func sessionWriteCommandInputFromOptions(opts cliOptions) sessionWriteCommandInput {
	return sessionWriteCommandInput{
		RecordFailure:       opts.recordFailure,
		FailureReason:       opts.failureReason,
		FailureCommit:       opts.failureCommit,
		RecordEvaluation:    opts.recordEvaluation,
		EvaluationOutcome:   opts.evaluationOutcome,
		EvaluationNotes:     opts.evaluationNotes,
		EvaluationReference: opts.evaluationReference,
		EvaluationScore:     opts.evaluationScore.value,
		RecordArtifact:      opts.recordArtifact,
		ArtifactKind:        opts.artifactKind,
		ArtifactSummary:     opts.artifactSummary,
		FeedbackApplyConfig: opts.feedbackApplyConfig,
		FeedbackHistoryPath: opts.feedbackHistoryPath,
	}
}
