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
	RecordFailure             string
	FailureReason             string
	FailureCommit             string
	FailureTaskType           string
	FailureSeverity           string
	RecordEvaluation          string
	EvaluationOutcome         string
	EvaluationNotes           string
	EvaluationReference       string
	EvaluationSource          string
	EvaluationEvaluator       string
	EvaluationRubricVersion   string
	EvaluationTaskType        string
	EvaluationDifficulty      string
	EvaluationExpectedOutcome string
	EvaluationModel           string
	EvaluationAgentVersion    string
	EvaluationScore           int
	EvaluationDurationMillis  int
	EvaluationCost            float64
	EvaluationConfidence      float64
	RecordArtifact            string
	ArtifactKind              string
	ArtifactLogicalPath       string
	ArtifactReviewStatus      string
	ArtifactSummary           string
	FeedbackApplyConfig       string
	FeedbackHistoryPath       string
}

func sessionWriteCommandInputFromOptions(opts cliOptions) sessionWriteCommandInput {
	return sessionWriteCommandInput{
		RecordFailure:             opts.recordFailure,
		FailureReason:             opts.failureReason,
		FailureCommit:             opts.failureCommit,
		FailureTaskType:           opts.failureTaskType,
		FailureSeverity:           opts.failureSeverity,
		RecordEvaluation:          opts.recordEvaluation,
		EvaluationOutcome:         opts.evaluationOutcome,
		EvaluationNotes:           opts.evaluationNotes,
		EvaluationReference:       opts.evaluationReference,
		EvaluationSource:          opts.evaluationSource,
		EvaluationEvaluator:       opts.evaluationEvaluator,
		EvaluationRubricVersion:   opts.evaluationRubricVersion,
		EvaluationTaskType:        opts.evaluationTaskType,
		EvaluationDifficulty:      opts.evaluationDifficulty,
		EvaluationExpectedOutcome: opts.evaluationExpectedOutcome,
		EvaluationModel:           opts.evaluationModel,
		EvaluationAgentVersion:    opts.evaluationAgentVersion,
		EvaluationScore:           opts.evaluationScore.value,
		EvaluationDurationMillis:  opts.evaluationDurationMillis.value,
		EvaluationCost:            opts.evaluationCost.value,
		EvaluationConfidence:      opts.evaluationConfidence.value,
		RecordArtifact:            opts.recordArtifact,
		ArtifactKind:              opts.artifactKind,
		ArtifactLogicalPath:       opts.artifactLogicalPath,
		ArtifactReviewStatus:      opts.artifactReviewStatus,
		ArtifactSummary:           opts.artifactSummary,
		FeedbackApplyConfig:       opts.feedbackApplyConfig,
		FeedbackHistoryPath:       opts.feedbackHistoryPath,
	}
}
