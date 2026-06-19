package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/modelroute"
)

const testImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func TestBuildOpenAIRequest_MapsImageContentParts(t *testing.T) {
	t.Parallel()

	req, err := buildOpenAIRequest(CompleteParams{
		Model: "gpt-4.1",
		Messages: []Message{{
			Role: RoleUser,
			ContentParts: []MessageContentPart{
				TextContentPart("What is in this screenshot?"),
				ImageContentPart("image/png", testImageBase64),
			},
		}},
	})
	require.NoError(t, err)

	data, err := json.Marshal(req.Messages[0].Content)
	require.NoError(t, err)

	assert.JSONEq(t, `[
		{"type":"text","text":"What is in this screenshot?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,`+testImageBase64+`"}}
	]`, string(data))
}

func TestBuildOpenAIRequest_NormalizesImageMediaType(t *testing.T) {
	t.Parallel()

	req, err := buildOpenAIRequest(CompleteParams{
		Model: "gpt-4.1",
		Messages: []Message{{
			Role: RoleUser,
			ContentParts: []MessageContentPart{{
				Type: MessageContentPartImage,
				Image: &ImageSource{
					MediaType:  " IMAGE/PNG ",
					DataBase64: testImageBase64,
				},
			}},
		}},
	})
	require.NoError(t, err)

	data, err := json.Marshal(req.Messages[0].Content)
	require.NoError(t, err)

	assert.Contains(t, string(data), "data:image/png;base64,"+testImageBase64)
	assert.NotContains(t, string(data), " IMAGE/PNG ")
}

func TestBuildAnthropicRequest_MapsImageContentParts(t *testing.T) {
	t.Parallel()

	for _, providerName := range []string{providerAnthropic, providerClaudeCode} {
		t.Run(providerName, func(t *testing.T) {
			t.Parallel()

			req, err := buildAnthropicRequestForProvider(providerName, CompleteParams{
				Model: "claude-sonnet-4-20250514",
				Messages: []Message{{
					Role: RoleUser,
					ContentParts: []MessageContentPart{
						TextContentPart("Describe this image."),
						ImageContentPart("image/png", testImageBase64),
					},
				}},
			})
			require.NoError(t, err)
			require.Len(t, req.Messages, 1)

			assert.JSONEq(t, `[
				{"type":"text","text":"Describe this image."},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+testImageBase64+`"}}
			]`, string(req.Messages[0].Content))
		})
	}
}

func TestBuildCodexResponsesRequest_MapsImageContentParts(t *testing.T) {
	t.Parallel()

	req, err := buildCodexResponsesRequest(CompleteParams{
		Model: "gpt-5.5",
		Messages: []Message{{
			Role: RoleUser,
			ContentParts: []MessageContentPart{
				TextContentPart("What changed?"),
				ImageContentPart("image/png", testImageBase64),
			},
		}},
	})
	require.NoError(t, err)
	require.Len(t, req.Input, 1)

	assert.Equal(t, []codexInputContent{
		{Type: "input_text", Text: "What changed?"},
		{Type: "input_image", ImageURL: "data:image/png;base64," + testImageBase64},
	}, req.Input[0].Content)
}

func TestBuildOllamaChatRequest_MapsImageContentParts(t *testing.T) {
	t.Parallel()

	req, err := buildOllamaChatRequest(CompleteParams{
		Model: "llava",
		Messages: []Message{{
			Role: RoleUser,
			ContentParts: []MessageContentPart{
				TextContentPart("What is in this picture?"),
				ImageContentPart("image/png", testImageBase64),
			},
		}},
	})
	require.NoError(t, err)
	require.Len(t, req.Messages, 1)

	assert.Equal(t, "What is in this picture?", req.Messages[0].Content)
	assert.Equal(t, []string{testImageBase64}, req.Messages[0].Images)
}

func TestValidateCompleteParams_ImagePartsRequireMultimodalProvider(t *testing.T) {
	t.Parallel()

	err := validateCompleteParamsAgainstCapabilities("text-only", ProviderCapabilities{
		SupportsChatCompletions: true,
		CompleteParams: map[string]CompleteParamSupport{
			"Messages": supported("test"),
		},
	}, CompleteParams{
		Messages: []Message{{
			Role:         RoleUser,
			ContentParts: []MessageContentPart{ImageContentPart("image/png", testImageBase64)},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multimodal input")
}

func TestValidateCompleteParams_RejectsInvalidImagePart(t *testing.T) {
	t.Parallel()

	_, err := buildOpenAIRequest(CompleteParams{
		Model: "gpt-4.1",
		Messages: []Message{{
			Role:         RoleAssistant,
			ContentParts: []MessageContentPart{ImageContentPart("image/png", testImageBase64)},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only supported on user messages")

	_, err = buildOpenAIRequest(CompleteParams{
		Model: "gpt-4.1",
		Messages: []Message{{
			Role:         RoleUser,
			ContentParts: []MessageContentPart{ImageContentPart("image/bmp", testImageBase64)},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MediaType")
}

func TestCompleteParamsRequiredCapabilities_ImagePartsRequireVision(t *testing.T) {
	t.Parallel()

	capabilities := completeParamsRequiredCapabilities(CompleteParams{
		Messages: []Message{{
			Role:         RoleUser,
			ContentParts: []MessageContentPart{ImageContentPart("image/png", testImageBase64)},
		}},
	})

	assert.Contains(t, capabilities, modelroute.CapabilityVision)
}

func TestEstimateTokens_ImagePartsUsePlaceholderInsteadOfBase64Text(t *testing.T) {
	t.Parallel()

	largeBase64Payload := strings.Repeat("AAAA", 10_000)
	estimate := EstimateTokens([]Message{{
		Role: RoleUser,
		ContentParts: []MessageContentPart{
			TextContentPart("Describe this image."),
			ImageContentPart("image/png", largeBase64Payload),
		},
	}})

	assert.GreaterOrEqual(t, estimate, 1000)
	assert.Less(t, estimate, 2000)
}

func TestRegistry_ModelRoleInfersRequiredVisionCapabilityFromImageParts(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	textOnly := &capabilityFakeProvider{
		capabilities: ProviderCapabilities{SupportsChatCompletions: true},
		fakeProvider: fakeProvider{
			name:   "textonly",
			models: []string{"plain"},
			resp:   &Response{Content: "text"},
		},
	}
	openAI := &fakeProvider{
		name:   providerOpenAI,
		models: []string{"gpt-4.1-mini"},
		resp:   &Response{Content: "vision"},
	}

	r.Register(textOnly)
	r.Register(openAI)

	require.NoError(t, r.SetModelRole("vision_router", ModelRole{
		Preferred:      "textonly/plain",
		FallbackModels: []string{"openai/gpt-4.1-mini"},
	}))

	params := CompleteParams{
		Model: "vision_router",
		Messages: []Message{{
			Role: RoleUser,
			ContentParts: []MessageContentPart{
				TextContentPart("Describe this screenshot."),
				ImageContentPart("image/png", testImageBase64),
			},
		}},
	}

	resp, err := r.CompleteWithFallback(context.Background(), params, nil)
	require.NoError(t, err)

	assert.Equal(t, "vision", resp.Content)
	assert.Empty(t, textOnly.calls)
	require.Len(t, openAI.calls, 1)
	assert.Equal(t, "gpt-4.1-mini", openAI.calls[0].Model)

	resolution, ok, err := r.ResolveModelRole("vision_router", params, nil)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-4.1-mini", resolution.SelectedModel)
	assert.Contains(t, resolution.Decision.Policy.RequiredCapabilities, modelroute.CapabilityVision)
	assertRejectionContains(t, resolution.Decision, "textonly/plain", modelroute.ReasonMissingCapability)
	assertRejectionContains(t, resolution.Decision, "textonly/plain", modelroute.CapabilityVision)
}
