package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

const fingerprintLength = 16

func completeFindingIdentity(finding Finding) Finding {
	finding.Path = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(finding.Path)), "./")
	finding.Kind = strings.TrimSpace(finding.Kind)
	finding.RuleID = strings.TrimSpace(finding.RuleID)
	finding.Owner = strings.TrimSpace(finding.Owner)

	if finding.RuleID == "" && finding.Kind != "" {
		finding.RuleID = "watch." + finding.Kind
	}

	if finding.Fingerprint == "" {
		finding.Fingerprint = fingerprintFinding(finding)
	}

	if finding.ID == "" && finding.RuleID != "" && finding.Fingerprint != "" {
		finding.ID = finding.RuleID + ":" + finding.Fingerprint
	}

	return finding
}

func fingerprintFinding(finding Finding) string {
	identity := strings.Join([]string{
		strings.TrimSpace(finding.RuleID),
		strings.TrimSpace(finding.Kind),
		strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(finding.Path)), "./"),
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))

	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

type suppressionMatcher struct {
	byID          map[string]string
	byFingerprint map[string]string
	scoped        []Suppression
}

func newSuppressionMatcher(suppressions []Suppression) (suppressionMatcher, error) {
	matcher := suppressionMatcher{
		byID:          make(map[string]string),
		byFingerprint: make(map[string]string),
	}

	for i := range suppressions {
		suppression := normalizeSuppression(suppressions[i])
		if suppression.Reason == "" {
			return suppressionMatcher{}, fmt.Errorf("watch suppression %d: reason is required", i)
		}

		if suppression.ID == "" && suppression.Fingerprint == "" && (suppression.RuleID == "" || suppression.Path == "") {
			return suppressionMatcher{}, fmt.Errorf("watch suppression %d: id, fingerprint, or rule_id+path is required", i)
		}

		if suppression.ID != "" {
			matcher.byID[suppression.ID] = suppression.Reason
		}

		if suppression.Fingerprint != "" {
			matcher.byFingerprint[suppression.Fingerprint] = suppression.Reason
		}

		if suppression.RuleID != "" && suppression.Path != "" {
			matcher.scoped = append(matcher.scoped, suppression)
		}
	}

	return matcher, nil
}

func normalizeSuppression(suppression Suppression) Suppression {
	suppression.ID = strings.TrimSpace(suppression.ID)
	suppression.Fingerprint = strings.TrimSpace(suppression.Fingerprint)
	suppression.RuleID = strings.TrimSpace(suppression.RuleID)
	suppression.Path = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(suppression.Path)), "./")
	suppression.Reason = strings.TrimSpace(suppression.Reason)

	return suppression
}

func (m suppressionMatcher) apply(finding Finding) Finding {
	finding = completeFindingIdentity(finding)
	if reason := m.reason(finding); reason != "" {
		finding.Suppressed = true
		finding.SuppressionReason = reason
	}

	return finding
}

func (m suppressionMatcher) reason(finding Finding) string {
	if finding.ID != "" {
		if reason := m.byID[finding.ID]; reason != "" {
			return reason
		}
	}

	if finding.Fingerprint != "" {
		if reason := m.byFingerprint[finding.Fingerprint]; reason != "" {
			return reason
		}
	}

	for _, suppression := range m.scoped {
		if suppression.RuleID == finding.RuleID && suppression.Path == finding.Path {
			return suppression.Reason
		}
	}

	return ""
}
