package events

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventDocs_CoverSupportedEventTypes(t *testing.T) {
	t.Parallel()

	docs := EventDocs()
	supported := SupportedEventTypes()
	require.Len(t, docs, len(supported))

	for i, doc := range docs {
		assert.Equal(t, supported[i].Type, doc.Type)
		assert.Equal(t, supported[i].Description, doc.Description)
		assert.Equal(t, EventSchemaVersion, doc.SchemaVersion)

		for name, example := range map[string]Event{
			"metadata": doc.MetadataExample,
			"summary":  doc.SummaryExample,
			"full":     doc.FullExample,
		} {
			assert.Equal(t, doc.Type, example.Type, name)
			assert.Equal(t, EventSchemaVersion, example.SchemaVersion, name)
			assert.NotEmpty(t, example.PayloadMode, name)
		}
	}
}

func TestEventDocsMarkdown_IsGeneratedAndCurrent(t *testing.T) {
	t.Parallel()

	generated, err := os.ReadFile("../../docs/lifecycle-events.md")
	require.NoError(t, err)

	assert.Equal(t, EventDocsMarkdown(), string(generated))
}
