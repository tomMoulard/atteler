package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCredentialJSON mimics the serialized OAuth credential blob written back
// to the keychain: it contains spaces, double quotes, backslashes, and JSON
// \uXXXX escapes, all of which must survive security(1) interactive quoting.
//
//nolint:gosec // fake credential material, never a real token.
const testCredentialJSON = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-access token","expiresAt":1765000000000,"refreshToken":"sk-ant-ort01-refresh\\token","scopes":["user:inference"],"note":"a \"quoted\" value & more"}}`

func TestBuildKeychainWritebackCommand_SecretNeverInArgv(t *testing.T) {
	t.Parallel()

	args, stdin, err := buildKeychainWritebackCommand("user@example.com", testCredentialJSON)
	require.NoError(t, err)

	// argv must carry no secret material: only the interactive-mode flag.
	assert.Equal(t, []string{"-i"}, args)

	for _, arg := range args {
		assert.NotContains(t, arg, testCredentialJSON)
		assert.NotContains(t, arg, "sk-ant-oat01")
		assert.NotContains(t, arg, "sk-ant-ort01")
		assert.NotContains(t, arg, "accessToken")
		assert.NotContains(t, arg, "refreshToken")
	}

	// The secret travels via the stdin payload instead.
	assert.Contains(t, stdin, "add-generic-password")
	assert.Contains(t, stdin, "sk-ant-oat01")
	assert.Contains(t, stdin, "sk-ant-ort01")
}

func TestBuildKeychainWritebackCommand_StdinRoundTripsSecret(t *testing.T) {
	t.Parallel()

	_, stdin, err := buildKeychainWritebackCommand("user@example.com", testCredentialJSON)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(stdin, "\n"), "stdin payload must end with a newline so security -i executes it")

	tokens := parseSecurityInteractiveTokens(t, strings.TrimSuffix(stdin, "\n"))

	require.NotEmpty(t, tokens)
	assert.Equal(t, "add-generic-password", tokens[0])
	assert.Contains(t, tokens, "-U")
	assert.Equal(t, keychainService, tokenAfterFlag(t, tokens, "-s"))
	assert.Equal(t, "user@example.com", tokenAfterFlag(t, tokens, "-a"))
	//nolint:testifylint // byte-for-byte round-trip fidelity matters, not JSON equivalence.
	assert.Equal(t, testCredentialJSON, tokenAfterFlag(t, tokens, "-w"))
}

func TestBuildKeychainWritebackCommand_OmitsAccountWhenEmpty(t *testing.T) {
	t.Parallel()

	_, stdin, err := buildKeychainWritebackCommand("", testCredentialJSON)
	require.NoError(t, err)

	tokens := parseSecurityInteractiveTokens(t, strings.TrimSuffix(stdin, "\n"))
	assert.NotContains(t, tokens, "-a")
}

func TestBuildKeychainWritebackCommand_RejectsLineBreaksInSecret(t *testing.T) {
	t.Parallel()

	_, _, err := buildKeychainWritebackCommand("user@example.com", "line1\nadd-generic-password injected")
	require.ErrorIs(t, err, errSecurityTokenLineBreak)

	_, _, err = buildKeychainWritebackCommand("evil\raccount", testCredentialJSON)
	require.ErrorIs(t, err, errSecurityTokenLineBreak)
}

func TestQuoteSecurityInteractiveToken_EscapesQuotesAndBackslashes(t *testing.T) {
	t.Parallel()

	quoted, err := quoteSecurityInteractiveToken(`a "b" \c`)
	require.NoError(t, err)
	assert.Equal(t, `"a \"b\" \\c"`, quoted)
}

// tokenAfterFlag returns the token immediately following flag.
func tokenAfterFlag(t *testing.T, tokens []string, flag string) string {
	t.Helper()

	for i, token := range tokens {
		if token == flag {
			require.Less(t, i+1, len(tokens), "flag %s has no value", flag)

			return tokens[i+1]
		}
	}

	require.Failf(t, "flag not found", "flag %s not present in tokens %v", flag, tokens)

	return ""
}

// parseSecurityInteractiveTokens is a reference implementation of the
// security(1) interactive-mode tokenizer: tokens are split on unquoted
// whitespace, and inside double quotes a backslash escapes the next byte.
// This behavior was verified by round-tripping payloads through the real
// `security -i` on macOS.
func parseSecurityInteractiveTokens(t *testing.T, line string) []string {
	t.Helper()

	var (
		tokens  []string
		current strings.Builder
		inQuote bool
		inToken bool
	)

	for i := 0; i < len(line); i++ {
		c := line[i]

		switch {
		case c == '\\' && inQuote:
			i++
			require.Less(t, i, len(line), "dangling backslash escape")
			current.WriteByte(line[i])
		case c == '"':
			inQuote = !inQuote
			inToken = true
		case (c == ' ' || c == '\t') && !inQuote:
			if inToken {
				tokens = append(tokens, current.String())
				current.Reset()

				inToken = false
			}
		default:
			current.WriteByte(c)

			inToken = true
		}
	}

	require.False(t, inQuote, "unterminated quote in command line")

	if inToken {
		tokens = append(tokens, current.String())
	}

	return tokens
}
