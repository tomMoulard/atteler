package skill

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/events"
)

func TestScratch_DuplicateStepSuggestionWedgesLearning(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	storeDir := filepath.Join(root, "learning")
	skillDir := filepath.Join(root, "skills")
	learner := NewLearner(LearningOptions{
		StoreDir:       storeDir,
		SkillDir:       skillDir,
		MaxSteps:       6,
		MinOccurrences: 2,
	})

	// User reruns the same command repeatedly (very common: flaky test loop).
	var lastErr error
	for range 10 {
		lastErr = learner.ObserveEvent(t.Context(), events.Event{
			Type:      events.CommandExecute,
			Timestamp: time.Now().UTC(),
			Metadata:  map[string]string{"command": "go build ./..."},
		})
	}

	fmt.Println("last observe err:", lastErr)

	// Now a genuine multi-step workflow repeats; can it still become a skill?
	workflow := []string{
		"git status --short",
		"go test ./pkg/skill",
		"git status --short",
		"go test ./pkg/skill",
	}
	for _, command := range workflow {
		err := learner.ObserveEvent(t.Context(), events.Event{
			Type:      events.CommandExecute,
			Timestamp: time.Now().UTC(),
			Metadata:  map[string]string{"command": command},
		})
		fmt.Println("workflow observe err:", err)
	}

	store := NewLearningStore(storeDir)
	state, err := store.Load()
	require.NoError(t, err)
	fmt.Println("skills recorded:", len(state.Skills))

	for _, s := range state.Skills {
		fmt.Println(" -", s.Slug, s.Steps)
	}
}

func TestScratch_MetadataTailLeak(t *testing.T) {
	t.Parallel()

	out := sanitizeCommand("kubectl label pod mypod secret-token='abc def ghi'")
	fmt.Printf("sanitized label: %q\n", out)

	out2 := sanitizeCommand("kubectl annotate pod mypod api-key='topsecret value two'")
	fmt.Printf("sanitized annotate: %q\n", out2)
}

func TestScratch_DuplicateStepTriggerEvalFails(t *testing.T) {
	t.Parallel()

	suggestion, ok := Suggest([]string{"run go build", "run go build", "run go build", "run go build"})
	require.True(t, ok)
	fmt.Printf("suggestion: slug=%q steps=%v occ=%d\n", suggestion.Slug, suggestion.Steps, suggestion.Occurrences)

	_, err := ValidateTriggerEvals(suggestion)
	fmt.Println("trigger eval err:", err)
}
