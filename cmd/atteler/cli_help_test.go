package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tommoulard/atteler/pkg/watch"
)

func TestPrintTopLevelHelp_ShrinksFlagCatalogToDomains(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printTopLevelHelp(&buf)
	out := buf.String()

	assert.Contains(t, out, "atteler help [domain]")
	assert.Contains(t, out, "atteler help legacy")
	assert.Contains(t, out, noLegacyDeprecationNotice)
	assert.NotContains(t, out, "--code-package-symbol-prefix-file-summary")
	assert.LessOrEqual(t, len(strings.Split(strings.TrimSpace(out), "\n")), 40)

	for _, domain := range []string{
		"chat/session",
		"config",
		"providers",
		"agents",
		"memory/rag",
		"code-intel",
		"review",
		"watch",
		"plugins",
		"worktrees",
		"eval",
	} {
		assert.Contains(t, out, domain)
	}
}

func TestPrintTopLevelHelp_ExamplesStayParseable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printTopLevelHelp(&buf)

	examples := topLevelHelpExamplesForTest(buf.String())
	require.NotEmpty(t, examples)

	for _, example := range examples {
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			args := splitCommandLineForTest(t, example)
			require.NotEmpty(t, args)
			require.Equal(t, "atteler", args[0])

			fs := newRegisteredFlagSetForTest(t)
			plan := translateCLIArgsWithFlagSet(args[1:], fs)
			require.NoError(t, plan.Err)
			require.False(t, plan.Help)
			require.NoError(t, fs.Parse(plan.Args), "top-level help example should parse after translation: %s -> %#v", example, plan.Args)
		})
	}
}

func TestPrintCLIHelp_SpecialHelpSelectors(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("code-summary", false, "test flag")

	for _, selector := range []string{"", "domains", "overview"} {
		t.Run("overview/"+selector, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			err := printCLIHelp(&buf, fs, selector)
			require.NoError(t, err)

			out := buf.String()
			assert.Contains(t, out, "Domains:")
			assert.Contains(t, out, "atteler help legacy")
			assert.Contains(t, out, noLegacyDeprecationNotice)
			assert.NotContains(t, out, "Compatibility flag catalog:")
			assert.NotContains(t, out, "--code-summary")
		})
	}

	for _, selector := range []string{"legacy", "flags", "all"} {
		t.Run("legacy/"+selector, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			err := printCLIHelp(&buf, fs, selector)
			require.NoError(t, err)

			out := buf.String()
			assert.Contains(t, out, "Compatibility flag catalog:")
			assert.Contains(t, out, noLegacyDeprecationNotice)
			assert.Contains(t, out, "--code-summary")
		})
	}
}

func TestImplicitFlagDefaults_UseSharedPackageDefaults(t *testing.T) {
	t.Parallel()

	wantLargeFileBytes := strconv.FormatInt(watch.DefaultLargeFileBytes, 10)

	assert.Equal(t, wantLargeFileBytes, implicitFlagDefaults["watch-large-file-bytes"])
	assert.Equal(t, wantLargeFileBytes, implicitFlagDefaults["merge-artifact-max-bytes"])
}

func TestPrintCLIHelp_UnknownDomainGuidesToOverview(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	err := printCLIHelp(&buf, flag.NewFlagSet("test", flag.ContinueOnError), "wat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown help domain")
	assert.Contains(t, err.Error(), "atteler help")
	assert.Empty(t, buf.String())
}

func TestPrintCLIHelp_LegacyFlagSelectorRoutesToOwningDomain(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("code-summary", false, "test flag")
	fs.Bool("review-scan", false, "test flag")

	var buf bytes.Buffer

	err := printCLIHelp(&buf, fs, "--code-summary")
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Code intelligence")
	assert.Contains(t, out, "--code-summary")
	assert.NotContains(t, out, "--review-scan")
}

func TestPrintCLIHelp_AllDomainsRenderFocusedHelp(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			fs := newRegisteredFlagSetForTest(t)

			var buf bytes.Buffer

			err := printCLIHelp(&buf, fs, domain.Name)
			require.NoError(t, err)

			out := buf.String()
			assert.Contains(t, out, domain.Title)
			assert.Contains(t, out, "Usage: atteler "+usageNameForDomain(domain)+" <command> [args]")
			assert.Contains(t, out, "Examples:")
			assert.Contains(t, out, "Compatible legacy flags:")
			assert.Contains(t, out, "Compatibility: existing top-level --flag aliases continue to work.")
			assert.Contains(t, out, noLegacyDeprecationNotice)
			assert.NotContains(t, out, "Compatibility flag catalog:")

			legacy := firstLegacyAlias(domain)
			require.NotEmpty(t, legacy, "domain %s should document at least one legacy alias", domain.Name)
			assert.Contains(t, out, legacy)
		})
	}
}

func TestPrintCLIHelp_AllDomainAliasesRenderFocusedHelp(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		for _, alias := range domain.Aliases {
			t.Run(domain.Name+"/"+alias, func(t *testing.T) {
				t.Parallel()

				fs := newRegisteredFlagSetForTest(t)

				var buf bytes.Buffer

				err := printCLIHelp(&buf, fs, alias)
				require.NoError(t, err)

				out := buf.String()
				assert.Contains(t, out, domain.Title)
				assert.Contains(t, out, "Usage: atteler "+alias+" <command> [args]")
				assert.Contains(t, out, "Examples:")
				assert.Contains(t, out, "Aliases:")
				assert.NotContains(t, out, "Compatibility flag catalog:")
			})
		}
	}
}

func TestREADME_CLIDocumentationDefersToGeneratedHelp(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	readme := string(data)
	assert.Contains(t, readme, "Grouped command surface:")
	assert.Contains(t, readme, "atteler help <domain>")
	assert.Contains(t, readme, "atteler help legacy")
	assert.Contains(t, readme, "atteler help --code-summary")
	assert.Contains(t, readme, "Domain help is rendered from structured command metadata")
	assert.Contains(t, readme, "covered by routing tests")
	assert.Contains(t, readme, "No legacy flag is deprecated")

	for _, domain := range []string{
		"chat",
		"config",
		"providers",
		"agents",
		"memory",
		"code-intel",
		"review",
		"watch",
		"plugins",
		"worktrees",
		"eval",
	} {
		assert.Contains(t, readme, "atteler "+domain)
	}

	// README should show representative examples and defer the exhaustive
	// compatibility catalog to generated/tested help output.
	legacyFlagMentions := regexp.MustCompile(`--[a-z0-9][a-z0-9-]+`).FindAllString(readme, -1)
	assert.LessOrEqual(t, len(legacyFlagMentions), 80)
}

func TestREADME_GroupedDomainTableMatchesHelpMetadata(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	got, ok := markedReadmeBlockForTest(string(data), "atteler:cli-domains")
	require.True(t, ok, "README should contain generated CLI domain markers")

	assert.Equal(t, strings.TrimSpace(readmeDomainTableForTest()), strings.TrimSpace(got))
}

func TestREADME_GroupedCommandExamplesStayParseable(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	examples := readmeGroupedCommandExamplesForTest(string(data))
	require.NotEmpty(t, examples)

	for _, example := range examples {
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			args := splitCommandLineForTest(t, example)
			require.NotEmpty(t, args)
			require.Equal(t, "atteler", args[0])

			fs := newRegisteredFlagSetForTest(t)
			plan := translateCLIArgsWithFlagSet(args[1:], fs)
			require.NoError(t, plan.Err)
			require.False(t, plan.Help)
			require.NoError(t, fs.Parse(plan.Args), "README grouped example should parse after translation: %s -> %#v", example, plan.Args)
		})
	}
}

func TestREADME_SimpleAttelerExamplesStayParseable(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)

	examples := readmeSimpleAttelerExamplesForTest(string(data))
	require.NotEmpty(t, examples)

	for _, example := range examples {
		t.Run(example, func(t *testing.T) {
			t.Parallel()

			args := splitCommandLineForTest(t, example)
			require.NotEmpty(t, args)
			require.Equal(t, "atteler", args[0])

			fs := newRegisteredFlagSetForTest(t)
			plan := translateCLIArgsWithFlagSet(args[1:], fs)
			require.NoError(t, plan.Err)

			if plan.Help {
				return
			}

			require.NoError(t, fs.Parse(plan.Args), "README atteler example should parse after translation: %s -> %#v", example, plan.Args)
		})
	}
}

func TestCLIHelpDomains_ExamplesStayParseable(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()
			require.NotEmpty(t, domain.Examples, "%s should include focused examples", domain.Name)

			seenExamples := make(map[string]bool, len(domain.Examples))
			for _, example := range domain.Examples {
				assert.False(t, seenExamples[example], "duplicate example %q in %s", example, domain.Name)
				seenExamples[example] = true
			}

			for _, example := range domain.Examples {
				t.Run(example, func(t *testing.T) {
					t.Parallel()

					args := splitCommandLineForTest(t, example)
					require.NotEmpty(t, args)
					require.Equal(t, "atteler", args[0])

					fs := newRegisteredFlagSetForTest(t)
					plan := translateCLIArgsWithFlagSet(args[1:], fs)
					require.NoError(t, plan.Err)
					require.False(t, plan.Help)
					require.NoError(t, fs.Parse(plan.Args), "domain help example should parse after translation: %s -> %#v", example, plan.Args)
				})
			}
		})
	}
}

func TestCLIHelpDomains_MatchAcceptanceDomains(t *testing.T) {
	t.Parallel()

	var names []string
	for _, domain := range cliHelpDomains {
		names = append(names, domain.Name)
	}

	assert.Equal(t, []string{
		"chat/session",
		"config",
		"providers",
		"agents",
		"memory/rag",
		"code-intel",
		"review",
		"watch",
		"plugins",
		"worktrees",
		"eval",
	}, names)
}

func TestCLIHelpDomains_DoNotRepeatCommandNames(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			seen := make(map[string]bool)

			for _, command := range domain.Commands {
				name := normalizeHelpName(command.Name)
				assert.False(t, seen[name], "duplicate command %q in %s", command.Name, domain.Name)
				seen[name] = true
			}
		})
	}
}

func TestCLIHelpDomains_DoNotRepeatDomainNamesOrAliases(t *testing.T) {
	t.Parallel()

	seen := make(map[string]string)

	for _, domain := range cliHelpDomains {
		names := append([]string{domain.Name}, domain.Aliases...)
		for _, name := range names {
			normalized := normalizeHelpName(name)
			require.NotEmpty(t, normalized, "empty domain name or alias in %s", domain.Name)

			previous, exists := seen[normalized]
			assert.False(t, exists, "domain token %q is used by both %s and %s", name, previous, domain.Name)
			seen[normalized] = domain.Name
		}
	}
}

func TestCLIHelpDomains_AliasesAvoidAmbiguousSubdomains(t *testing.T) {
	t.Parallel()

	// Domain aliases are advertised in focused help, so keep them as true
	// synonyms. Narrow nouns such as "task" or "model" make generic commands
	// like `atteler task list` look valid while routing through a broader
	// domain with different "list" semantics.
	ambiguousAliases := map[string]bool{
		"fixture":  true,
		"fixtures": true,
		"mcp":      true,
		"model":    true,
		"models":   true,
		"task":     true,
		"tasks":    true,
	}

	for _, domain := range cliHelpDomains {
		for _, alias := range domain.Aliases {
			assert.False(t, ambiguousAliases[normalizeHelpName(alias)], "ambiguous alias %q should be an explicit command alias instead", alias)
		}
	}
}

func TestCLIHelpDomains_DoNotRepeatCommandNamesOrAliases(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			seen := make(map[string]string)

			for _, command := range domain.Commands {
				names := append([]string{command.Name}, command.Aliases...)
				for _, name := range names {
					normalized := normalizeHelpName(name)
					require.NotEmpty(t, normalized, "empty command name or alias in %s", domain.Name)

					previous, exists := seen[normalized]
					assert.False(t, exists, "command token %q is used by both %s and %s in %s", name, previous, command.Name, domain.Name)
					seen[normalized] = command.Name
				}
			}
		})
	}
}

func TestCLIHelpDomains_LegacyAliasesStayRegisteredFlags(t *testing.T) {
	t.Parallel()

	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, command := range domain.Commands {
				for _, legacy := range command.Legacy {
					name := strings.TrimLeft(legacy, "-")
					assert.True(t, registered[name], "legacy alias %q for %s %s is not a registered flag", legacy, domain.Name, command.Name)
				}
			}
		})
	}
}

func TestCLIHelpDomains_LegacyAliasesStayInOwningDomain(t *testing.T) {
	t.Parallel()

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			for _, command := range domain.Commands {
				for _, legacy := range command.Legacy {
					name := strings.TrimLeft(legacy, "-")
					got, ok := lookupFlagDomain(name)
					require.True(t, ok, "legacy alias %q for %s %s is missing from CLI help domain mapping", legacy, domain.Name, command.Name)
					assert.Equal(t, domain.Name, got, "legacy alias %q for %s %s should stay in its owning help domain", legacy, domain.Name, command.Name)
				}
			}
		})
	}
}

func TestRegisteredFlagsHaveExplicitAcceptedHelpDomains(t *testing.T) {
	t.Parallel()

	accepted := acceptedHelpDomainNames()
	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	for name := range registered {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			domain, ok := lookupFlagDomain(name)
			assert.True(t, ok, "registered flag %q is missing from CLI help domain mapping", name)
			assert.True(t, accepted[domain], "registered flag %q maps to unknown domain %q", name, domain)
		})
	}
}

func TestRegisteredFlagsCanSelectFocusedHelp(t *testing.T) {
	t.Parallel()

	accepted := acceptedHelpDomainNames()
	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	fs := newRegisteredFlagSetForTest(t)

	for name := range registered {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			domain, ok := helpDomainForSelector("--"+name, fs)
			require.True(t, ok, "registered flag %q should select focused help", name)
			assert.True(t, accepted[domain], "registered flag %q maps to unknown help domain %q", name, domain)
		})
	}
}

func TestRegisteredFlagsRouteThroughHelpCommand(t *testing.T) {
	t.Parallel()

	accepted := acceptedHelpDomainNames()
	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	fs := newRegisteredFlagSetForTest(t)

	for name := range registered {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := translateCLIArgsWithFlagSet([]string{helpCommandName, "--" + name}, fs)
			require.NoError(t, got.Err)
			require.True(t, got.Help, "registered flag %q should route through help command", name)
			assert.True(t, accepted[got.HelpDomain], "registered flag %q maps to unknown help domain %q", name, got.HelpDomain)
			assert.Empty(t, got.Args)
		})
	}
}

func TestRegisteredFlagsRenderFocusedHelp(t *testing.T) {
	t.Parallel()

	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	fs := newRegisteredFlagSetForTest(t)

	for name := range registered {
		var buf bytes.Buffer

		err := printCLIHelp(&buf, fs, "--"+name)
		require.NoError(t, err, "registered flag %q should render focused help", name)

		out := buf.String()
		assert.Contains(t, out, "--"+name, "focused help for registered flag %q should include the flag", name)
		assert.Contains(t, out, "Compatibility: existing top-level --flag aliases continue to work.")
		assert.Contains(t, out, noLegacyDeprecationNotice)
		assert.NotContains(t, out, "Compatibility flag catalog:", "registered flag %q should not render full legacy catalog", name)
	}
}

func TestLegacyHelpIncludesEveryRegisteredFlag(t *testing.T) {
	t.Parallel()

	registered := registeredFlagNamesForTest(t)
	require.Greater(t, len(registered), 100)

	fs := newRegisteredFlagSetForTest(t)

	var buf bytes.Buffer
	printLegacyFlagHelp(&buf, fs)
	out := buf.String()
	assert.Contains(t, out, noLegacyDeprecationNotice)

	for name := range registered {
		assert.Contains(t, out, "--"+name, "legacy help should include registered flag %q", name)
	}
}

func acceptedHelpDomainNames() map[string]bool {
	names := make(map[string]bool, len(cliHelpDomains))

	for _, domain := range cliHelpDomains {
		names[domain.Name] = true
	}

	return names
}

func registeredFlagNamesForTest(t *testing.T) map[string]bool {
	t.Helper()

	fs := newRegisteredFlagSetForTest(t)
	names := make(map[string]bool)

	fs.VisitAll(func(f *flag.Flag) {
		names[f.Name] = true
	})

	return names
}

func TestPrintCLIHelp_DomainIncludesCommandsExamplesAndGeneratedFlags(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("code-summary", false, "print compact Go code index and import graph counts and exit")
	fs.String("code-symbol", "", "find Go symbols by exact name in the current repository and exit")
	fs.Bool("review-scan", false, "scan the current repository and print a structured review report and exit")

	var buf bytes.Buffer

	err := printCLIHelp(&buf, fs, "code-intel")
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Code intelligence")
	assert.Contains(t, out, "summary")
	assert.Contains(t, out, "atteler code-intel summary")
	assert.Contains(t, out, "--code-summary")
	assert.Contains(t, out, "--code-symbol")
	assert.NotContains(t, out, "--review-scan")
}

func TestPrintCLIHelp_MultipleCompatibilityAliasesAreReadable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	err := printCLIHelp(&buf, flag.NewFlagSet("test", flag.ContinueOnError), "watch")
	require.NoError(t, err)

	assert.Equal(t, "alias: --watch-scan", legacyAliasSummary([]string{"--watch-scan"}))

	out := buf.String()
	assert.Contains(t, out, "json                         scan once and emit findings as JSON (aliases: --watch-scan, --watch-json)")
	assert.NotContains(t, out, "alias: --watch-scan --watch-json")
}

func TestPrintCLIHelp_CommandAliasesAreReadable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	err := printCLIHelp(&buf, flag.NewFlagSet("test", flag.ContinueOnError), "plugins")
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "mcp-manifest <path>")
	assert.Contains(t, out, "command alias: manifest; alias: --mcp-manifest")
	assert.Contains(t, out, "mcp-tool <tool>")
	assert.Contains(t, out, "command alias: tool; alias: --mcp-tool")
	assert.Contains(t, out, "atteler plugins manifest .atteler/mcp.yaml")
}

func TestPrintCLIHelp_AllDomainsRenderFocusedExamples(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	for name := range registeredFlagNamesForTest(t) {
		fs.Bool(name, false, "test flag")
	}

	for _, domain := range cliHelpDomains {
		t.Run(domain.Name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			err := printCLIHelp(&buf, fs, domain.Name)
			require.NoError(t, err)

			out := buf.String()
			assert.Contains(t, out, domain.Title)
			assert.Contains(t, out, "Commands:")
			assert.Contains(t, out, "Examples:")
			assert.Contains(t, out, "Compatibility: existing top-level --flag aliases continue to work.")
			assert.Contains(t, out, noLegacyDeprecationNotice)
			assert.NotContains(t, out, "Compatibility flag catalog:")
			assert.LessOrEqual(t, len(strings.Split(strings.TrimSpace(out), "\n")), 80, "%s help should stay focused", domain.Name)

			for _, example := range domain.Examples {
				assert.Contains(t, out, example)
			}
		})
	}
}

func TestPrintCLIHelp_DomainAliasesRenderSameFocusedHelp(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	for name := range registeredFlagNamesForTest(t) {
		fs.Bool(name, false, "test flag")
	}

	for _, domain := range cliHelpDomains {
		for _, alias := range domain.Aliases {
			t.Run(domain.Name+"/"+alias, func(t *testing.T) {
				t.Parallel()

				var buf bytes.Buffer

				err := printCLIHelp(&buf, fs, alias)
				require.NoError(t, err)

				out := buf.String()
				assert.Contains(t, out, domain.Title)
				assert.Contains(t, out, "Usage: atteler "+alias+" <command> [args]")
				assert.Contains(t, out, "Examples:")
			})
		}
	}
}

func TestPrintCLIHelp_RequestedAliasDrivesUsage(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"session":  "Usage: atteler session <command> [args]",
		"rag":      "Usage: atteler rag <command> [args]",
		"cfg":      "Usage: atteler cfg <command> [args]",
		"provider": "Usage: atteler provider <command> [args]",
	}

	for selector, want := range tests {
		t.Run(selector, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			err := printCLIHelp(&buf, flag.NewFlagSet("test", flag.ContinueOnError), selector)
			require.NoError(t, err)

			assert.Contains(t, buf.String(), want)
		})
	}
}

func TestPrintCLIHelp_SlashDomainsPreferHumanAliasInUsage(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"chat/session": "Usage: atteler chat <command> [args]",
		"memory/rag":   "Usage: atteler memory <command> [args]",
	}

	for domainName, want := range tests {
		t.Run(domainName, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			err := printCLIHelp(&buf, flag.NewFlagSet("test", flag.ContinueOnError), domainName)
			require.NoError(t, err)

			out := buf.String()
			assert.Contains(t, out, want)
			assert.NotContains(t, out, "Usage: atteler "+domainName+" <command> [args]")
		})
	}
}

func TestFlagDomain_CoversAcceptanceDomains(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"once":                      "chat/session",
		"validate-config":           "config",
		"list-providers":            "providers",
		"plan-agents":               "agents",
		"memory-search":             "memory/rag",
		"code-symbol-prefix":        "code-intel",
		"review-plan":               "review",
		"watch-scan":                "watch",
		"run-plugin":                "plugins",
		"worktree":                  "worktrees",
		"record-evaluation":         "eval",
		"route-cache-reuse":         "providers",
		"agent-memory-index":        "memory/rag",
		"lsp-workspace-symbols":     "code-intel",
		"merge-worktree":            "worktrees",
		"plugin-timeout-seconds":    "plugins",
		"evaluation-reference":      "eval",
		"watch-max-iterations":      "watch",
		"code-file-import-prefix":   "code-intel",
		"agent-performance-summary": "agents",
	}

	for flagName, want := range tests {
		t.Run(flagName, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, flagDomain(flagName))
		})
	}
}

func readmeGroupedCommandExamplesForTest(readme string) []string {
	var examples []string

	inDomainTable := false

	commandRE := regexp.MustCompile("`(atteler [^`]+)`")

	for line := range strings.SplitSeq(readme, "\n") {
		if strings.HasPrefix(line, "| Domain | Examples |") {
			inDomainTable = true
			continue
		}

		if !inDomainTable {
			continue
		}

		if strings.TrimSpace(line) == "" {
			break
		}

		for _, match := range commandRE.FindAllStringSubmatch(line, -1) {
			examples = append(examples, match[1])
		}
	}

	return examples
}

func readmeSimpleAttelerExamplesForTest(readme string) []string {
	var examples []string

	for line := range strings.SplitSeq(readme, "\n") {
		example := strings.TrimSpace(line)
		if !strings.HasPrefix(example, "atteler ") {
			continue
		}

		// Multi-line examples, shell redirection, placeholder tokens, and
		// single-quoted shell literals are documented for humans; focused help
		// and grouped-table tests cover those command families separately.
		if strings.HasSuffix(example, `\`) ||
			strings.ContainsAny(example, "<>'") ||
			strings.Contains(example, " [") {
			continue
		}

		examples = append(examples, example)
	}

	return examples
}

func topLevelHelpExamplesForTest(help string) []string {
	var examples []string

	inExamples := false

	for line := range strings.SplitSeq(help, "\n") {
		switch {
		case strings.TrimSpace(line) == "Examples:":
			inExamples = true
			continue
		case inExamples && strings.TrimSpace(line) == "":
			return examples
		case !inExamples:
			continue
		}

		example := strings.TrimSpace(line)
		if strings.HasPrefix(example, "atteler ") {
			examples = append(examples, example)
		}
	}

	return examples
}

func splitCommandLineForTest(t *testing.T, command string) []string {
	t.Helper()

	var (
		args    []string
		current strings.Builder
	)

	inDoubleQuote := false

	for _, r := range command {
		switch {
		case r == '"':
			inDoubleQuote = !inDoubleQuote
		case r == ' ' && !inDoubleQuote:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	require.False(t, inDoubleQuote, "unclosed double quote in command example %q", command)

	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}

func domainTokenForTest(domain cliHelpDomain) string {
	for _, alias := range domain.Aliases {
		if alias != "" {
			return alias
		}
	}

	return domain.Name
}

func documentedCommandArgsForTest(domain cliHelpDomain, command cliCommandAlias) []string {
	return documentedCommandArgsForTokens(domainTokenForTest(domain), command.Name, command)
}

func documentedCommandArgsForTokens(domainToken, commandToken string, command cliCommandAlias) []string {
	args := []string{domainToken, commandToken}
	if command.Args != "" {
		args = append(args, "value")
	}

	if command.PromptAfterValue {
		args = append(args, "prompt")
	}

	return args
}

func assertCommandLegacyPrefix(t *testing.T, command cliCommandAlias, args []string) {
	t.Helper()

	if len(command.Legacy) == 0 {
		return
	}

	require.GreaterOrEqual(t, len(args), len(command.Legacy))
	assert.Equal(t, command.Legacy, args[:len(command.Legacy)])
}

func firstLegacyAlias(domain cliHelpDomain) string {
	for _, command := range domain.Commands {
		if len(command.Legacy) > 0 {
			return command.Legacy[0]
		}
	}

	return ""
}

func markedReadmeBlockForTest(readme, name string) (string, bool) {
	start := "<!-- " + name + ":start -->"
	end := "<!-- " + name + ":end -->"

	startIndex := strings.Index(readme, start)
	if startIndex < 0 {
		return "", false
	}

	endIndex := strings.Index(readme[startIndex:], end)
	if endIndex < 0 {
		return "", false
	}

	endIndex += startIndex + len(end)

	return readme[startIndex:endIndex], true
}

func readmeDomainTableForTest() string {
	var table strings.Builder

	table.WriteString("<!-- atteler:cli-domains:start -->\n")
	table.WriteString("| Domain | Examples |\n")
	table.WriteString("|--------|----------|\n")

	for _, domain := range cliHelpDomains {
		table.WriteString("| ")
		table.WriteString(readmeDomainLabelForTest(domain))
		table.WriteString(" | ")
		table.WriteString(readmeExamplesForTest(domain.Examples))
		table.WriteString(" |\n")
	}

	table.WriteString("<!-- atteler:cli-domains:end -->")

	return table.String()
}

func readmeDomainLabelForTest(domain cliHelpDomain) string {
	parts := strings.Split(domain.Name, "/")
	if len(parts) > 1 {
		labels := make([]string, 0, len(parts))
		for _, part := range parts {
			labels = append(labels, "`"+part+"`")
		}

		return strings.Join(labels, " / ")
	}

	return "`" + domain.Name + "`"
}

func readmeExamplesForTest(examples []string) string {
	quoted := make([]string, 0, len(examples))
	for _, example := range examples {
		quoted = append(quoted, "`"+example+"`")
	}

	return strings.Join(quoted, ", ")
}

func newRegisteredFlagSetForTest(t *testing.T) *flag.FlagSet {
	t.Helper()

	_, fs := newCLIOptionsAndFlagSetForTest(t)

	return fs
}

func newCLIOptionsAndFlagSetForTest(t *testing.T) (*cliOptions, *flag.FlagSet) {
	t.Helper()

	var opts cliOptions

	opts.temperature = floatFlag{name: "temperature", min: 0}
	opts.topP = floatFlag{name: "top-p", min: 0, max: 1, hasMax: true}
	opts.routeBudget = floatFlag{name: "route-budget", min: 0}
	opts.routeCacheReuse = floatFlag{name: "route-cache-reuse", min: 0, max: 1, hasMax: true}
	opts.maxTokens = positiveIntFlag{name: "max-tokens"}
	opts.maxInputTokens = positiveIntFlag{name: "max-input-tokens"}
	opts.seed = nonNegativeIntFlag{name: "seed"}
	opts.mcpTimeout = positiveIntFlag{name: "mcp-timeout-seconds"}
	opts.spawnTimeout = positiveIntFlag{name: "spawn-timeout-seconds"}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	registerCLIFlagsWithFlagSet(fs, &opts)

	return &opts, fs
}
