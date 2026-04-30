package llm

import "log"

// AutoRegister tries to create every known provider and registers the ones
// whose credentials are available. It returns a ready-to-use Registry.
// Providers that fail to initialize (missing credentials) are silently skipped.
func AutoRegister() *Registry {
	r := NewRegistry()

	if p, err := NewAnthropicProvider(); err == nil {
		r.Register(p)
	} else {
		log.Printf("llm: anthropic skipped: %v", err)
	}

	if p, err := NewOpenAIProvider(); err == nil {
		r.Register(p)
	} else {
		log.Printf("llm: openai skipped: %v", err)
	}

	return r
}
