package symphony

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderPrompt_StrictVariablesAndLoops(t *testing.T) {
	t.Parallel()

	attempt := 2
	body := `Issue {{ issue.identifier }}: {{ issue.title }}
{% if attempt %}Attempt {{ attempt }}{% endif %}
Labels:{% for label in issue.labels %} {{ label }}{% endfor %}`

	rendered, err := RenderPrompt(body, Issue{
		ID:         "1",
		Identifier: "GH-1",
		Title:      "Fix it",
		State:      "OPEN",
		Labels:     []string{"bug", "p1"},
	}, &attempt)
	require.NoError(t, err)

	assert.Contains(t, rendered, "Issue GH-1: Fix it")
	assert.Contains(t, rendered, "Attempt 2")
	assert.Contains(t, rendered, "Labels: bug p1")
}

func TestRenderPrompt_UnknownVariableFails(t *testing.T) {
	t.Parallel()

	_, err := RenderPrompt("{{ issue.nope }}", Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, nil)
	require.Error(t, err)

	var classed *ClassedError
	require.ErrorAs(t, err, &classed)
	assert.Equal(t, ErrTemplateRender, classed.Class)
}

func TestRenderPrompt_UnknownFilterFails(t *testing.T) {
	t.Parallel()

	_, err := RenderPrompt("{{ issue.title | upcase }}", Issue{ID: "1", Identifier: "GH-1", Title: "Fix", State: "OPEN"}, nil)
	require.Error(t, err)

	var classed *ClassedError
	require.ErrorAs(t, err, &classed)
	assert.Equal(t, ErrTemplateRender, classed.Class)
}
