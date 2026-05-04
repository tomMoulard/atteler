package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/tommoulard/atteler/pkg/llm"
)

// Markdown renders a session as a shareable Markdown transcript.
func Markdown(session Session) string {
	var b strings.Builder
	if session.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", session.Title)
		writeMetadataString(&b, "Session", fallback(session.ID, "untitled"))
	} else {
		fmt.Fprintf(&b, "# Atteler Session %s\n\n", fallback(session.ID, "untitled"))
	}

	writeMetadata(&b, "Created", session.CreatedAt)
	writeMetadata(&b, "Updated", session.UpdatedAt)
	writeMetadataString(&b, "Agent", session.DefaultAgent)
	writeMetadataString(&b, "Model", session.DefaultModel)
	writeMetadataString(&b, "Tags", strings.Join(session.Tags, ", "))

	writeNegativeKnowledge(&b, session.NegativeKnowledge)
	writeEvaluations(&b, session.Evaluations)
	writeArtifacts(&b, session.Artifacts)

	if len(session.Messages) == 0 {
		b.WriteString("\n_No messages._\n")
		return b.String()
	}

	b.WriteString("\n## Transcript\n\n")

	for _, message := range session.Messages {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", roleTitle(message.Role), strings.TrimSpace(message.Content))
	}

	return b.String()
}

func writeEvaluations(b *strings.Builder, entries []AgentEvaluation) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Agent Evaluations\n\n")

	for _, entry := range entries {
		if entry.Agent == "" && entry.Outcome == "" {
			continue
		}

		fmt.Fprintf(b, "- **Agent:** %s\n", strings.TrimSpace(entry.Agent))

		if entry.Outcome != "" {
			fmt.Fprintf(b, "  - **Outcome:** %s\n", strings.TrimSpace(entry.Outcome))
		}

		if entry.Score != 0 {
			fmt.Fprintf(b, "  - **Score:** %d\n", entry.Score)
		}

		if entry.Reference != "" {
			fmt.Fprintf(b, "  - **Reference:** %s\n", strings.TrimSpace(entry.Reference))
		}

		if entry.Notes != "" {
			fmt.Fprintf(b, "  - **Notes:** %s\n", strings.TrimSpace(entry.Notes))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeArtifacts(b *strings.Builder, entries []Artifact) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Artifacts\n\n")

	for _, entry := range entries {
		if entry.Path == "" && entry.Kind == "" {
			continue
		}

		fmt.Fprintf(b, "- **Path:** %s\n", strings.TrimSpace(entry.Path))

		if entry.Kind != "" {
			fmt.Fprintf(b, "  - **Kind:** %s\n", strings.TrimSpace(entry.Kind))
		}

		if entry.Summary != "" {
			fmt.Fprintf(b, "  - **Summary:** %s\n", strings.TrimSpace(entry.Summary))
		}

		if entry.SourceAgent != "" {
			fmt.Fprintf(b, "  - **Source Agent:** %s\n", strings.TrimSpace(entry.SourceAgent))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeNegativeKnowledge(b *strings.Builder, entries []NegativeKnowledge) {
	if len(entries) == 0 {
		return
	}

	b.WriteString("\n## Negative Knowledge\n\n")

	for _, entry := range entries {
		if entry.Approach == "" && entry.Reason == "" {
			continue
		}

		fmt.Fprintf(b, "- **Approach:** %s\n", strings.TrimSpace(entry.Approach))

		if entry.Reason != "" {
			fmt.Fprintf(b, "  - **Reason:** %s\n", strings.TrimSpace(entry.Reason))
		}

		if entry.Commit != "" {
			fmt.Fprintf(b, "  - **Commit:** %s\n", strings.TrimSpace(entry.Commit))
		}

		if entry.Agent != "" {
			fmt.Fprintf(b, "  - **Agent:** %s\n", strings.TrimSpace(entry.Agent))
		}

		if !entry.CreatedAt.IsZero() {
			fmt.Fprintf(b, "  - **Created:** %s\n", entry.CreatedAt.UTC().Format(time.RFC3339))
		}
	}
}

func writeMetadata(b *strings.Builder, label string, value time.Time) {
	if value.IsZero() {
		return
	}

	fmt.Fprintf(b, "- **%s:** %s\n", label, value.UTC().Format(time.RFC3339))
}

func writeMetadataString(b *strings.Builder, label, value string) {
	if value == "" {
		return
	}

	fmt.Fprintf(b, "- **%s:** %s\n", label, value)
}

func roleTitle(role llm.Role) string {
	switch role {
	case llm.RoleUser:
		return "User"
	case llm.RoleAssistant:
		return "Assistant"
	case llm.RoleSystem:
		return "System"
	default:
		value := strings.TrimSpace(string(role))
		if value == "" {
			return "Unknown"
		}

		return strings.ToUpper(value[:1]) + value[1:]
	}
}

func fallback(value, fallbackValue string) string {
	if value == "" {
		return fallbackValue
	}

	return value
}
