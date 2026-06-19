package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/agent"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
)

const tuiRequestTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func TestExpandReferences_AttachesInlineImageToLastUserMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data, err := base64.StdEncoding.DecodeString(tuiRequestTestPNGBase64)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "screenshot.png"), data, 0o600))

	messages, refs, events, err := expandReferences([]llm.Message{
		{Role: llm.RoleSystem, Content: "be concise"},
		{Role: llm.RoleUser, Content: "describe @screenshot.png"},
	}, contextref.Options{Root: dir})
	require.NoError(t, err)

	require.Len(t, refs, 1)
	assert.Equal(t, "image", refs[0].Kind)
	require.Len(t, events, 1)
	assert.Equal(t, "image", events[0].Kind)

	require.Len(t, messages, 2)
	require.Len(t, messages[1].ContentParts, 2)
	assert.Equal(t, llm.MessageContentPartText, messages[1].ContentParts[0].Type)
	assert.Contains(t, messages[1].ContentParts[0].Text, `<image path="screenshot.png"`)
	assert.Equal(t, llm.MessageContentPartImage, messages[1].ContentParts[1].Type)
	require.NotNil(t, messages[1].ContentParts[1].Image)
	assert.Equal(t, "image/png", messages[1].ContentParts[1].Image.MediaType)
	assert.Equal(t, tuiRequestTestPNGBase64, messages[1].ContentParts[1].Image.DataBase64)
}

func TestPrepareRunOnceRequest_AttachesInlineImageToOneShotMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data, err := base64.StdEncoding.DecodeString(tuiRequestTestPNGBase64)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "screenshot.png"), data, 0o600))

	registry := llm.NewRegistry()
	registry.Register(routeFakeProvider{name: "openai", models: []string{"gpt-4.1-mini"}})

	prepared, err := prepareRunOnceRequest(
		context.Background(),
		registry,
		agent.NewRegistry(nil),
		contextref.Options{Root: dir},
		"",
		"openai/gpt-4.1-mini",
		"",
		nil,
		nil,
		generationSettings{},
		generationSettings{},
		true,
		false,
		"describe @screenshot.png",
	)
	require.NoError(t, err)

	require.Len(t, prepared.requestMessages, 1)
	msg := prepared.requestMessages[0]
	assert.Equal(t, llm.RoleUser, msg.Role)
	assert.Contains(t, msg.Content, `<image path="screenshot.png"`)
	require.Len(t, msg.ContentParts, 2)
	assert.Equal(t, llm.MessageContentPartText, msg.ContentParts[0].Type)
	assert.Equal(t, llm.MessageContentPartImage, msg.ContentParts[1].Type)
	require.NotNil(t, msg.ContentParts[1].Image)
	assert.Equal(t, "image/png", msg.ContentParts[1].Image.MediaType)
	assert.Equal(t, tuiRequestTestPNGBase64, msg.ContentParts[1].Image.DataBase64)
}
