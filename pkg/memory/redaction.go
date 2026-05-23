//nolint:wsl_v5 // Redaction rule setup is clearer with compact guard clauses.
package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const redactionReplacementPrefix = "[REDACTED:"

const secretAssignmentNamePattern = `(?:[a-z0-9]+[_-])*(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|client[_-]?secret|private[_-]?key|auth(?:orization)?|secret|password)` //nolint:gosec // Redaction keyword regexp, not a credential.

const secretAssignmentPattern = `(?i)(?:"` + secretAssignmentNamePattern + `"|'` + secretAssignmentNamePattern + `'|\b` + secretAssignmentNamePattern + `\b)\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^,;\r\n}]+)`

// RedactionRule describes one regexp-based text redaction rule.
type RedactionRule struct {
	Name    string
	Pattern string
}

// RedactionDecision records which redaction rules changed indexed text.
//
//nolint:govet // JSON/API field readability is preferred over pointer packing.
type RedactionDecision struct {
	Redacted bool     `json:"redacted"`
	Rules    []string `json:"rules,omitempty"`
}

//nolint:govet // Name-first order keeps rule diagnostics readable.
type compiledRedactionRule struct {
	name string
	re   *regexp.Regexp
}

// Redactor redacts obvious secrets from text before it is indexed or printed.
type Redactor struct {
	rules []compiledRedactionRule
}

// DefaultRedactionRules returns the built-in secret redaction rules.
func DefaultRedactionRules() []RedactionRule {
	return []RedactionRule{
		{Name: "anthropic_api_key", Pattern: `\bsk-ant-[A-Za-z0-9_-]{16,}\b`},
		{Name: "openai_api_key", Pattern: `\bsk-[A-Za-z0-9_-]{16,}\b`},
		{Name: "aws_access_key_id", Pattern: `\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`},
		{Name: "google_api_key", Pattern: `\bAIza[0-9A-Za-z_-]{35}\b`},
		{Name: "github_token", Pattern: `\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{16,}\b`},
		{Name: "github_fine_grained_token", Pattern: `\bgithub_pat_[A-Za-z0-9_]{20,}\b`},
		{Name: "stripe_secret_key", Pattern: `\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}\b`},
		{Name: "slack_token", Pattern: `\bxox[baprs]-[A-Za-z0-9-]{10,}\b`},
		{Name: "jwt", Pattern: `\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`},
		{Name: "bearer_token", Pattern: `(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}`},
		{Name: "secret_assignment", Pattern: secretAssignmentPattern},
	}
}

// NewRedactor returns a redactor with the built-in rules plus custom RE2 regexps.
func NewRedactor(customPatterns ...string) (*Redactor, error) {
	defaultRules := DefaultRedactionRules()
	compiled := make([]compiledRedactionRule, 0, len(defaultRules)+len(customPatterns))
	for _, rule := range defaultRules {
		compiledRule, err := compileRedactionRule(rule)
		if err != nil {
			return nil, fmt.Errorf("compile redaction rule %s: %w", rule.Name, err)
		}

		compiled = append(compiled, compiledRule)
	}

	customRuleNumber := 0
	for _, pattern := range customPatterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		customRuleNumber++

		rule := RedactionRule{
			Name:    fmt.Sprintf("custom_%d", customRuleNumber),
			Pattern: pattern,
		}
		compiledRule, err := compileRedactionRule(rule)
		if err != nil {
			return nil, redactRedactionCompileError(rule.Name, err, compiled)
		}

		compiled = append(compiled, compiledRule)
	}

	return &Redactor{rules: compiled}, nil
}

func compileRedactionRule(rule RedactionRule) (compiledRedactionRule, error) {
	re, err := regexp.Compile(rule.Pattern)
	if err != nil {
		return compiledRedactionRule{}, fmt.Errorf("compile regexp: %w", err)
	}

	return compiledRedactionRule{name: rule.Name, re: re}, nil
}

func redactRedactionCompileError(name string, err error, compiled []compiledRedactionRule) error {
	if err == nil {
		return nil
	}

	message := "invalid regexp"
	if len(compiled) > 0 {
		if redacted, decision := (&Redactor{rules: compiled}).Redact(err.Error()); decision.Redacted {
			message = redacted
		}
	}

	// Do not wrap the original regexp error: regexp.SyntaxError may include the
	// user-supplied pattern, and custom patterns are sometimes pasted around
	// the exact secret a user is trying to hide.
	return fmt.Errorf("compile redaction rule %s: %s", name, message)
}

// Redact returns text with all configured secret-like values replaced.
func (r *Redactor) Redact(text string) (string, RedactionDecision) {
	if r == nil || text == "" {
		return text, RedactionDecision{}
	}

	out := text
	applied := make(map[string]struct{})
	for _, rule := range r.rules {
		if !rule.re.MatchString(out) {
			continue
		}

		replacement := redactionReplacementPrefix + rule.name + "]"
		out = rule.re.ReplaceAllString(out, replacement)
		applied[rule.name] = struct{}{}
	}

	if len(applied) == 0 {
		return out, RedactionDecision{}
	}

	rules := make([]string, 0, len(applied))
	for name := range applied {
		rules = append(rules, name)
	}
	sort.Strings(rules)

	return out, RedactionDecision{Redacted: true, Rules: rules}
}
