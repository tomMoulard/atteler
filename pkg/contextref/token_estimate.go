package contextref

import (
	"fmt"
	"strings"

	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/llm"
)

func estimateReferenceContent(opts Options, content []byte) (estimate contextpack.TokenEstimate, estimatorSummary string) {
	estimator := opts.TokenEstimator
	if estimator == nil {
		estimator = contextpack.DefaultEstimator()
	}

	estimate = estimator.EstimateMessage(llm.Message{
		Role:    llm.RoleSystem,
		Content: string(content),
	})

	return estimate, referenceEstimatorSummary(estimator.Profile())
}

func referenceEstimatorSummary(profile contextpack.EstimatorProfile) string {
	parts := []string{
		profile.Name,
		"provider=" + profile.Provider,
		fmt.Sprintf("cpt=%d", profile.CharsPerToken),
		fmt.Sprintf("overhead=%d", profile.MessageOverheadTokens),
		fmt.Sprintf("err=%d%%", profile.ErrorBoundPercent),
	}
	if strings.TrimSpace(profile.Calibration) != "" {
		parts = append(parts, "calibration="+strings.TrimSpace(profile.Calibration))
	}

	if strings.TrimSpace(profile.Model) != "" {
		parts = append(parts, "model="+strings.TrimSpace(profile.Model))
	}

	return strings.Join(parts, ";")
}
