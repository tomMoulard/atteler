package main

func buildInlineCommandRegistry() []command {
	contracts := inlineCommandContractsByName()
	commands := []command{
		{
			name:  "print-config-template",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.printConfigTemplate },
		},
		{
			name:  "show-version",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.showVersion },
		},
		{
			name:  "init-config",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.initConfigPath != "" },
		},
		{
			name:  "list-config-paths",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.listConfigPaths },
		},
		{
			name:  "validate-config",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.validateConfig },
		},
		{
			name:  "explain-config",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.explainConfig },
		},
		{
			name:  "command-surface-json",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.commandSurfaceJSON },
		},
		{
			name:  "command-surface-docs",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.commandSurfaceDocs },
		},
		{
			name:  "doctor-offline",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.doctorOffline },
		},
		{
			name:  "list-providers",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.listProviders },
		},
		{
			name:  "list-known-models",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.listKnownModels },
		},
		{
			name:  commandOllamaStatus,
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.ollamaStatus },
		},
		{
			name:  commandOllamaStop,
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.ollamaStop },
		},
		{
			name:  "list-worktrees",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.listWorktrees },
		},
		{
			name:  "merge-worktree",
			tier:  tierInline,
			match: func(o cliOptions) bool { return o.mergeWorktreeRef != "" },
		},
	}

	for i := range commands {
		contract, ok := contracts[commands[i].name]
		if !ok {
			panic("missing inline CLI command contract for " + commands[i].name)
		}

		contract.fillDerivedFields(commands[i].name)
		commands[i].contract = contract
	}

	mustValidateCommandContracts(commands)

	return commands
}
