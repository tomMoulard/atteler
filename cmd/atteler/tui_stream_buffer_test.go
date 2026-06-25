package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitStreamLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		buf       string
		complete  string
		remainder string
		ok        bool
	}{
		{name: "no newline buffers everything", buf: "Yes", complete: "", remainder: "Yes", ok: false},
		{name: "empty buffer", buf: "", complete: "", remainder: "", ok: false},
		{name: "single complete line", buf: "Yes\n", complete: "Yes", remainder: "", ok: true},
		{name: "complete line with partial tail", buf: "Yes\nand mor", complete: "Yes", remainder: "and mor", ok: true},
		{name: "multiple complete lines", buf: "a\nb\nc\n", complete: "a\nb\nc", remainder: "", ok: true},
		{name: "blank line only", buf: "\n", complete: "", remainder: "", ok: true},
		{name: "blank line between content", buf: "intro\n\nbody", complete: "intro\n", remainder: "body", ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			complete, remainder, ok := splitStreamLines(tt.buf)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.complete, complete)
			assert.Equal(t, tt.remainder, remainder)
		})
	}
}

// TestModel_BufferStreamDelta_CoalescesTokensIntoLines is the regression test
// for streamed responses rendering one token per line: partial tokens must be
// buffered (no print command) until a newline arrives.
func TestModel_BufferStreamDelta_CoalescesTokensIntoLines(t *testing.T) {
	t.Parallel()

	m := &model{}

	// Word-sized deltas without newlines accumulate without printing.
	for _, token := range []string{"Yes", " —", " the", " Symphony", " process"} {
		assert.Nil(t, m.bufferStreamDelta(token), "partial line must not emit a print command for %q", token)
	}

	assert.Equal(t, "Yes — the Symphony process", m.streamLineBuffer)

	// A delta that completes the line emits a command and retains the tail.
	cmd := m.bufferStreamDelta(" is running.\nNext")
	assert.NotNil(t, cmd, "completed line must emit a print command")
	assert.Equal(t, "Next", m.streamLineBuffer, "partial line after the newline stays buffered")
}

func TestModel_FlushStreamLineBuffer(t *testing.T) {
	t.Parallel()

	empty := &model{}
	assert.Nil(t, empty.flushStreamLineBuffer(), "empty buffer must not emit a command")

	buffered := &model{streamLineBuffer: "trailing line"}
	assert.NotNil(t, buffered.flushStreamLineBuffer(), "buffered content must emit a command")
	assert.Empty(t, buffered.streamLineBuffer, "flush must clear the buffer")
}
