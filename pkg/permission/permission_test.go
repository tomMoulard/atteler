package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyCommand_CoversCentralOperationKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		program string
		args    []string
		command string
		want    []OperationKind
	}{
		{name: "read inspection", command: "git status --short", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "write shell", command: "touch generated.txt", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "backtick write substitution", command: "printf before `touch generated.txt`", want: []OperationKind{OperationRead, OperationWrite, OperationExecute}},
		{name: "dollar write substitution", command: `printf "%s" "$(touch generated.txt)"`, want: []OperationKind{OperationRead, OperationWrite, OperationExecute}},
		{name: "single quoted backtick stays literal", command: "printf '%s' '`touch literal.txt`'", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "single quoted dollar substitution stays literal", command: `printf '%s' '$(touch literal.txt)'`, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "single quoted redirection stays literal", command: "printf '%s' 'a>b'", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "find delete mutates", command: "find . -name '*.tmp' -delete", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "find exec rm mutates", command: "find . -name '*.tmp' -exec rm {} ;", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "find exec network command", command: "find . -name '*.txt' -exec curl https://example.com/{} ;", want: []OperationKind{OperationRead, OperationExecute, OperationNetwork}},
		{name: "find exec unknown command fails closed", command: "find . -name '*.txt' -exec custom-helper {} ;", want: []OperationKind{OperationRead, OperationWrite, OperationExecute}},
		{name: "xargs rm mutates", command: "printf 'generated.txt\n' | xargs rm -f", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "xargs with options unwraps mutating command", command: "find . -name '*.tmp' -print0 | xargs -0 --max-args 1 rm -f", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "xargs network command", command: "printf 'https://example.com\n' | xargs curl", want: []OperationKind{OperationRead, OperationExecute, OperationNetwork}},
		{name: "xargs read-only command", command: "printf 'README.md\n' | xargs cat", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "xargs unknown command fails closed", command: "printf 'target\n' | xargs custom-helper", want: []OperationKind{OperationRead, OperationWrite, OperationExecute}},
		{name: "sed in place mutates", command: "sed -i.bak 's/a/b/' file.txt", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "sed without in place reads", command: "sed 's/a/b/' file.txt", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "bash args unwrap command after long options", program: "bash", args: []string{"--noprofile", "--norc", "-lc", "cat README.md"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "bash args unwrap write command after long options", program: "bash", args: []string{"--noprofile", "--norc", "-lc", "touch generated.txt"}, want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "network", command: "curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork}},
		{name: "curl output flag writes", command: "curl -o download.txt https://example.com", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "curl grouped output flag writes", command: "curl -fsSLo download.txt https://example.com", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "curl remote-name writes", command: "curl -O https://example.com/download.txt", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "curl cookie jar writes credentials", command: "curl --cookie-jar .cookies https://example.com", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "wget default download writes", command: "wget https://example.com/download.txt", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "network with attached redirection", command: "curl https://example.com>download.txt", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "leading redirection before network", command: "2>download.err curl https://example.com", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "fd duplication redirection is not file write", command: `printf '%s' ok 2>&1`, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "github cli network uses stored credentials", command: "gh api user", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "kubectl network uses stored credentials", command: "kubectl get pods", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "aws cli network uses stored credentials", command: "aws sts get-caller-identity", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "gcloud cli network uses stored credentials", command: "gcloud logging read projects/example/logs/app", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "azure cli network uses stored credentials", command: "az account show", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "ssh network uses credentials", command: "ssh git@example.com true", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "go get dependency change uses network write", command: "go get example.com/module", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "npm install dependency change uses network write", command: "npm --prefix web install", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork}},
		{name: "credential assignment", command: "API_TOKEN=secret go test ./...", want: []OperationKind{OperationExecute, OperationCredentialAccess}},
		{name: "assignment before network command", command: "API_TOKEN=secret curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "quoted assignment before network command", command: "API_TOKEN='secret value' curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "assignment before write command", command: "MODE=test touch generated.txt", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "assignment before shell write command", command: "MODE=test bash -lc 'touch generated.txt'", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "env assignment before network command", command: "env API_TOKEN=secret curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "env option before network command", command: "env -i API_TOKEN=secret curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "eval write command", command: "eval 'touch generated.txt'", want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "eval network command", command: `eval "curl https://example.com"`, want: []OperationKind{OperationExecute, OperationNetwork}},
		{name: "dynamic eval fails closed", command: `eval "$ATTELER_SCRIPT"`, want: []OperationKind{OperationWrite, OperationExecute}},
		{name: "command wrapper mutates", command: "command rm generated.txt", want: []OperationKind{OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "time wrapper network", command: "time curl https://example.com", want: []OperationKind{OperationExecute, OperationNetwork}},
		{name: "sudo wrapper accesses credentials", command: "sudo true", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "sudo wrapper unwraps long option value", command: "sudo --user root cat README.md", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "sudo wrapper mutates", command: "sudo rm generated.txt", want: []OperationKind{OperationWrite, OperationExecute, OperationCredentialAccess, OperationMergeDelete}},
		{name: "macos security read accesses credentials", command: "security find-generic-password -s Claude", want: []OperationKind{OperationExecute, OperationCredentialAccess}},
		{name: "macos security write accesses credentials", command: "security add-generic-password -s Claude -w secret", want: []OperationKind{OperationWrite, OperationExecute, OperationCredentialAccess}},
		{name: "shell git network mutation unwraps quoted command", command: "bash -lc 'git fetch origin'", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation}},
		{name: "shell script with read then delete classifies delete", command: "bash -lc 'cat README.md; rm generated.txt'", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationMergeDelete}},
		{name: "single quoted separator stays literal", command: "printf '%s' 'touch literal.txt; rm other'", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "credential variable reference", command: `printf "%s" "$OPENAI_API_KEY"`, want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "credential flag reference", command: "tool --api-key=$OPENAI_API_KEY", want: []OperationKind{OperationExecute, OperationCredentialAccess}},
		{name: "credential env path", command: "cat .env", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "less credential path reads", command: "less .env", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "source credential path fails closed as read write", command: "source .env", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationCredentialAccess}},
		{name: "dot source credential path fails closed as read write", command: ". .env", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationCredentialAccess}},
		{name: "input redirection reads", command: "custom-helper < README.md", want: []OperationKind{OperationRead, OperationExecute}},
		{name: "input redirection from credential path", command: "custom-helper < .env", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "attached input redirection from credential path", command: "custom-helper<.env", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "output redirection to credential path", command: "printf hello > .env", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationCredentialAccess}},
		{name: "attached output redirection to credential path", command: "printf hello>.env", want: []OperationKind{OperationRead, OperationWrite, OperationExecute, OperationCredentialAccess}},
		{name: "ssh private key path", command: "head -n1 ~/.ssh/id_ed25519", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "codex auth path", command: "cat ~/.codex/auth.json", want: []OperationKind{OperationRead, OperationExecute, OperationCredentialAccess}},
		{name: "git mutation", program: "git", args: []string{"commit", "-m", "msg"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git network mutation", program: "git", args: []string{"fetch", "origin"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation}},
		{name: "git network mutation with attached redirection", command: "git fetch>fetch.log", want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation}},
		{name: "git global flags before network mutation", program: "git", args: []string{"-C", ".", "fetch", "origin"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation}},
		{name: "git global config before read", program: "git", args: []string{"-c", "color.ui=false", "status", "--short"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git branch create mutates", program: "git", args: []string{"branch", "feature"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git branch list reads", program: "git", args: []string{"branch", "--list", "feature/*"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git config get reads", program: "git", args: []string{"config", "--get", "user.email"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git config set mutates", program: "git", args: []string{"config", "user.email", "agent@example.com"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git rm deletes tracked files", program: "git", args: []string{"rm", "tracked.txt"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation, OperationMergeDelete}},
		{name: "git mv mutates tracked files", program: "git", args: []string{"mv", "old.txt", "new.txt"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git init mutates repository state", program: "git", args: []string{"init"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git remote verbose reads", program: "git", args: []string{"remote", "-v"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git remote show uses network credentials", program: "git", args: []string{"remote", "show", "origin"}, want: []OperationKind{OperationRead, OperationExecute, OperationNetwork, OperationCredentialAccess}},
		{name: "git remote add mutates", program: "git", args: []string{"remote", "add", "origin", "https://example.com/repo.git"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "git submodule status reads", program: "git", args: []string{"submodule", "status"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git submodule update uses network mutation", program: "git", args: []string{"submodule", "update", "--init", "--recursive"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation}},
		{name: "git submodule deinit deletes worktree metadata", program: "git", args: []string{"submodule", "deinit", "-f", "vendor/lib"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation, OperationMergeDelete}},
		{name: "git apply check reads", program: "git", args: []string{"apply", "--check", "-"}, want: []OperationKind{OperationRead, OperationExecute}},
		{name: "git apply mutates", program: "git", args: []string{"apply", "-"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation}},
		{name: "merge delete", program: "git", args: []string{"branch", "-D", "atteler/session"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation, OperationMergeDelete}},
		{name: "git tag delete is merge delete", program: "git", args: []string{"tag", "-d", "v1.0.0"}, want: []OperationKind{OperationWrite, OperationExecute, OperationGitMutation, OperationMergeDelete}},
		{name: "git push delete is network merge delete", program: "git", args: []string{"push", "origin", "--delete", "feature"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation, OperationMergeDelete}},
		{name: "git push colon ref delete is network merge delete", program: "git", args: []string{"push", "origin", ":feature"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation, OperationMergeDelete}},
		{name: "git push force is network merge delete", program: "git", args: []string{"push", "--force-with-lease", "origin", "main"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation, OperationMergeDelete}},
		{name: "git push force refspec is network merge delete", program: "git", args: []string{"push", "origin", "+main"}, want: []OperationKind{OperationWrite, OperationExecute, OperationNetwork, OperationCredentialAccess, OperationGitMutation, OperationMergeDelete}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyCommand(tt.program, tt.args, tt.command)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEvaluate_DenyAskAndAllow(t *testing.T) {
	t.Parallel()

	policy := DefaultPolicy()
	policy.SetMode(OperationWrite, ModeDeny)
	policy.SetMode(OperationNetwork, ModeAsk)

	denied := Evaluate(context.Background(), &policy, Request{
		Action:     "touch generated.txt",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationWrite}},
	})
	require.False(t, denied.Allowed)
	assert.Equal(t, OperationWrite, denied.Kind)
	assert.Equal(t, "permission.write.deny", denied.Rule)

	askNoConfirmer := Evaluate(context.Background(), &policy, Request{
		Action:     "curl https://example.com",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationNetwork}},
	})
	require.False(t, askNoConfirmer.Allowed)
	assert.True(t, askNoConfirmer.NeedsApproval)
	assert.Contains(t, askNoConfirmer.Reason, "no interactive confirmer")

	ctx := ContextWithConfirmer(context.Background(), func(context.Context, Request, Decision) bool {
		return true
	})
	asked := Evaluate(ctx, &policy, Request{
		Action:     "curl https://example.com",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationNetwork}},
	})
	require.True(t, asked.Allowed)
	assert.True(t, asked.Confirmed)
	assert.Equal(t, "permission.network.allow", asked.Rule)
}

func TestEvaluate_PrioritizesSpecificSideEffectOverExecution(t *testing.T) {
	t.Parallel()

	policy := ReadOnlyPolicy()

	networkDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "curl https://example.com",
		Operations: CommandOperations("bash", []string{"-lc", "curl https://example.com"}, "curl https://example.com", ".", "test"),
	})
	require.False(t, networkDecision.Allowed)
	assert.Equal(t, OperationNetwork, networkDecision.Kind)
	assert.Equal(t, "permission.network.deny", networkDecision.Rule)

	credentialDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "cat .env",
		Operations: CommandOperations("bash", []string{"-lc", "cat .env"}, "cat .env", ".", "test"),
	})
	require.False(t, credentialDecision.Allowed)
	assert.Equal(t, OperationCredentialAccess, credentialDecision.Kind)
	assert.Equal(t, "permission.credential_access.deny", credentialDecision.Rule)

	mergeDeleteDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "rm generated.txt",
		Operations: CommandOperations("bash", []string{"-lc", "rm generated.txt"}, "rm generated.txt", ".", "test"),
	})
	require.False(t, mergeDeleteDecision.Allowed)
	assert.Equal(t, OperationMergeDelete, mergeDeleteDecision.Kind)
	assert.Equal(t, "permission.merge_delete.deny", mergeDeleteDecision.Rule)

	findNetworkDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "find . -exec curl https://example.com/{} ;",
		Operations: CommandOperations("bash", []string{"-lc", "find . -exec curl https://example.com/{} ;"}, "find . -exec curl https://example.com/{} ;", ".", "test"),
	})
	require.False(t, findNetworkDecision.Allowed)
	assert.Equal(t, OperationNetwork, findNetworkDecision.Kind)
	assert.Equal(t, "permission.network.deny", findNetworkDecision.Rule)
}

func TestEvaluate_AuditsAllowedAndDeniedSideEffects(t *testing.T) {
	auditDir := t.TempDir()
	t.Setenv(EnvAuditDir, auditDir)

	policy := DefaultPolicy()
	policy.SetMode(OperationWrite, ModeDeny)

	denied := Evaluate(context.Background(), &policy, Request{
		Action:     "touch denied",
		Source:     "test",
		Target:     ".",
		Operations: []Operation{{Kind: OperationExecute}, {Kind: OperationWrite}},
	})
	require.False(t, denied.Allowed)

	allowed := Evaluate(context.Background(), &policy, Request{
		Action:     "cat README.md",
		Source:     "test",
		Target:     "README.md",
		Operations: []Operation{{Kind: OperationRead}},
	})
	require.True(t, allowed.Allowed)

	records := readPermissionAuditRecords(t, auditDir)
	require.Len(t, records, 2)
	assert.Equal(t, "denied", records[0].Decision)
	assert.Equal(t, "allow,write:deny", records[0].Policy)
	assert.Equal(t, "permission.write.deny", records[0].Rule)
	assert.Contains(t, records[0].Reason, "write operation")
	assert.Contains(t, records[0].OperationKinds, string(OperationWrite))

	assert.Equal(t, "allowed", records[1].Decision)
	assert.Equal(t, "allow,write:deny", records[1].Policy)
	assert.Equal(t, "permission.allow", records[1].Rule)
	assert.Contains(t, records[1].OperationKinds, string(OperationRead))
}

func TestEvaluate_AuditIncludesSessionMetadataFromContext(t *testing.T) {
	auditDir := t.TempDir()
	t.Setenv(EnvAuditDir, auditDir)

	ctx := ContextWithAuditMetadata(context.Background(), map[string]string{
		"session_id":       "session-123",
		"session_path":     "/tmp/session.json",
		"issue_id":         "8",
		"issue_identifier": "GH-8",
		"agent":            "executor",
		"model":            "gpt-test",
	})

	decision := Evaluate(ctx, nil, Request{
		Action:     "git status --short",
		Source:     "test",
		Target:     ".",
		Operations: []Operation{{Kind: OperationRead}},
	})
	require.True(t, decision.Allowed)

	records := readPermissionAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	assert.Equal(t, "session-123", records[0].SessionID)
	assert.Equal(t, "/tmp/session.json", records[0].SessionPath)
	assert.Equal(t, "8", records[0].IssueID)
	assert.Equal(t, "GH-8", records[0].IssueIdentifier)
	assert.Equal(t, "executor", records[0].Agent)
	assert.Equal(t, "gpt-test", records[0].Model)
	require.Len(t, records[0].Operations, 1)
	assert.Equal(t, "session-123", records[0].Operations[0].Metadata["session_id"])
	assert.Equal(t, "executor", records[0].Operations[0].Metadata["agent"])
	assert.Equal(t, "gpt-test", records[0].Operations[0].Metadata["model"])
}

func TestEvaluate_AuditPreservesMultipleOperationsWithSameKind(t *testing.T) {
	auditDir := t.TempDir()
	t.Setenv(EnvAuditDir, auditDir)

	policy := DefaultPolicy()
	policy.SetMode(OperationWrite, ModeDeny)

	decision := Evaluate(context.Background(), &policy, Request{
		Action: "write generated artifacts",
		Source: "test",
		Operations: []Operation{
			{Kind: OperationWrite, Target: "generated-a.txt"},
			{Kind: OperationWrite, Target: "generated-b.txt"},
		},
	})
	require.False(t, decision.Allowed)
	require.Len(t, decision.Operations, 2)
	assert.Equal(t, "generated-a.txt", decision.Operations[0].Target)
	assert.Equal(t, "generated-b.txt", decision.Operations[1].Target)

	records := readPermissionAuditRecords(t, auditDir)
	require.Len(t, records, 1)
	require.Len(t, records[0].Operations, 2)
	assert.Equal(t, "generated-a.txt", records[0].Operations[0].Target)
	assert.Equal(t, "generated-b.txt", records[0].Operations[1].Target)
	assert.Equal(t, []string{string(OperationWrite)}, records[0].OperationKinds)
}

func readPermissionAuditRecords(t *testing.T, auditDir string) []AuditRecord {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(auditDir, sideEffectLedgerFileName))
	require.NoError(t, err)

	var records []AuditRecord

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var record AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}

	return records
}

func TestReadOnlyPolicy_AllowsInspectionAndDeniesWrites(t *testing.T) {
	t.Parallel()

	policy := ReadOnlyPolicy()

	readDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "git status --short",
		Operations: CommandOperations("git", []string{"status", "--short"}, "", ".", "test"),
	})
	require.True(t, readDecision.Allowed)

	writeDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "touch generated.txt",
		Operations: CommandOperations("bash", []string{"-lc", "touch generated.txt"}, "touch generated.txt", ".", "test"),
	})
	require.False(t, writeDecision.Allowed)
	assert.Equal(t, OperationWrite, writeDecision.Kind)

	unknownExecuteDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "custom-tool --version",
		Operations: CommandOperations("custom-tool", []string{"--version"}, "", ".", "test"),
	})
	require.False(t, unknownExecuteDecision.Allowed)
	assert.Equal(t, OperationExecute, unknownExecuteDecision.Kind)

	unknownAfterReadDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "cat README.md && custom-tool --version",
		Operations: CommandOperations("bash", []string{"-lc", "cat README.md && custom-tool --version"}, "cat README.md && custom-tool --version", ".", "test"),
	})
	require.False(t, unknownAfterReadDecision.Allowed)
	assert.Equal(t, OperationExecute, unknownAfterReadDecision.Kind)

	unknownWithInputRedirectionDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "custom-tool < README.md",
		Operations: CommandOperations("bash", []string{"-lc", "custom-tool < README.md"}, "custom-tool < README.md", ".", "test"),
	})
	require.False(t, unknownWithInputRedirectionDecision.Allowed)
	assert.Equal(t, OperationExecute, unknownWithInputRedirectionDecision.Kind)
	assert.True(t, hasOperationKind(unknownWithInputRedirectionDecision.Operations, OperationRead))

	undeclaredReadExecuteDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "plugin entrypoint",
		Operations: []Operation{{Kind: OperationRead}, {Kind: OperationExecute}},
	})
	require.False(t, undeclaredReadExecuteDecision.Allowed)
	assert.Equal(t, OperationExecute, undeclaredReadExecuteDecision.Kind)

	credentialReadDecision := Evaluate(context.Background(), &policy, Request{
		Action:     "cat .env",
		Operations: CommandOperations("bash", []string{"-lc", "cat .env"}, "cat .env", ".", "test"),
	})
	require.False(t, credentialReadDecision.Allowed)
	assert.True(t, hasOperationKind(credentialReadDecision.Operations, OperationCredentialAccess))
	assert.Equal(t, OperationCredentialAccess, credentialReadDecision.Kind)

	credentialPolicy := DefaultPolicy()
	credentialPolicy.SetMode(OperationCredentialAccess, ModeDeny)
	credentialDeniedDecision := Evaluate(context.Background(), &credentialPolicy, Request{
		Action:     "cat .env",
		Operations: CommandOperations("bash", []string{"-lc", "cat .env"}, "cat .env", ".", "test"),
	})
	require.False(t, credentialDeniedDecision.Allowed)
	assert.Equal(t, OperationCredentialAccess, credentialDeniedDecision.Kind)
}

func TestAllowsReadOnlyExecution_RequiresClassifiedInspectionMetadata(t *testing.T) {
	t.Parallel()

	policy := ReadOnlyPolicy()

	inspectionOps := CommandOperations("bash", []string{"-lc", "printf '%s' ok"}, "printf '%s' ok", ".", "test")
	assert.True(t, AllowsReadOnlyExecution(policy, inspectionOps))

	unknownReadOps := CommandOperations("bash", []string{"-lc", "custom-helper < README.md"}, "custom-helper < README.md", ".", "test")
	assert.False(t, AllowsReadOnlyExecution(policy, unknownReadOps))

	undeclaredOps := []Operation{{Kind: OperationRead}, {Kind: OperationExecute}}
	assert.False(t, AllowsReadOnlyExecution(policy, undeclaredOps))

	mixedExecuteOps := append([]Operation{{Kind: OperationExecute, Action: "plugin entrypoint"}}, inspectionOps...)
	assert.False(t, AllowsReadOnlyExecution(policy, mixedExecuteOps))
}

func TestPolicySummary_NamedPolicyShowsOverrides(t *testing.T) {
	t.Parallel()

	readOnly := ReadOnlyPolicy()
	assert.Equal(t, "read-only", readOnly.Summary())

	readOnly.SetMode(OperationNetwork, ModeAllow)
	readOnly.AllowReadExecution = false
	assert.Equal(t, "read-only,network:allow,read_execution:deny", readOnly.Summary())

	ask := DefaultPolicy()
	ask.Name = string(ModeAsk)
	ask.Default = ModeAsk
	ask.SetMode(OperationRead, ModeAllow)
	assert.Equal(t, string(ModeAsk), ask.Summary())

	ask.SetMode(OperationExecute, ModeDeny)
	assert.Equal(t, string(ModeAsk)+",execute:deny", ask.Summary())
}

func hasOperationKind(ops []Operation, kind OperationKind) bool {
	for _, op := range ops {
		if op.Kind == kind {
			return true
		}
	}

	return false
}

func TestParseOperationKinds_AcceptsCommaSeparatedValues(t *testing.T) {
	t.Parallel()

	got, err := ParseOperationKinds([]string{"read,write", "credential-access"})
	require.NoError(t, err)
	assert.Equal(t, []OperationKind{OperationRead, OperationWrite, OperationCredentialAccess}, got)

	_, err = ParseOperationKinds([]string{"unknown"})
	require.Error(t, err)
}
