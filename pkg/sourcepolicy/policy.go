// Package sourcepolicy classifies and enforces research/retrieval source trust policy.
//
//nolint:wsl_v5 // Classifier helpers stay compact to keep source-type rules near their scores.
package sourcepolicy

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const domainGitHub = "github.com"

const (
	// SourceTypeOfficialDocs identifies first-party documentation.
	SourceTypeOfficialDocs = "official_docs"
	// SourceTypeSourceCode identifies source repositories or local source files.
	SourceTypeSourceCode = "source_code"
	// SourceTypeStandardOrSpec identifies standards, RFCs, and specifications.
	SourceTypeStandardOrSpec = "standard_or_spec"
	// SourceTypeAcademic identifies academic papers or university-hosted material.
	SourceTypeAcademic = "academic"
	// SourceTypeVendorBlog identifies vendor-owned blog/marketing material.
	SourceTypeVendorBlog = "vendor_blog"
	// SourceTypeProjectBlog identifies project-owned blog material.
	SourceTypeProjectBlog = "project_blog"
	// SourceTypeIssueDiscussion identifies issue trackers, pull requests, and discussions.
	SourceTypeIssueDiscussion = "issue_discussion"
	// SourceTypeForum identifies community/forum material.
	SourceTypeForum = "forum"
	// SourceTypeNews identifies news or magazine material.
	SourceTypeNews = "news"
	// SourceTypeUnknown identifies sources that cannot be classified confidently.
	SourceTypeUnknown = "unknown"
	// SourceTypeProjectGuidance identifies harness instruction files such as AGENTS.md.
	SourceTypeProjectGuidance = "project_guidance"
)

const (
	// TrustLevelHigh means the source is strong evidence for technical claims.
	TrustLevelHigh = "high"
	// TrustLevelMedium means the source may be useful but should usually be corroborated.
	TrustLevelMedium = "medium"
	// TrustLevelLow means the source is weak evidence and should be marked clearly.
	TrustLevelLow = "low"
	// TrustLevelDenied means policy excludes the source.
	TrustLevelDenied = "denied"
	// TrustLevelUnknown means trust could not be assessed.
	TrustLevelUnknown = "unknown"
)

const (
	// PolicyMatchTrustedDomain means a source matched a configured trusted domain.
	PolicyMatchTrustedDomain = "trusted_domain"
	// PolicyMatchDeniedDomain means a source matched a configured denied domain.
	PolicyMatchDeniedDomain = "denied_domain"
	// PolicyMatchPreferredType means a source matched a configured preferred source type.
	PolicyMatchPreferredType = "preferred_source_type"
	// PolicyMatchHarnessGuidance means a source came from project/harness guidance.
	PolicyMatchHarnessGuidance = "harness_guidance"
	// PolicyMatchLocalRepository means a source came from the local repository.
	PolicyMatchLocalRepository = "local_repository"
	// PolicyMatchBuiltInClassifier means only the built-in classifier matched.
	PolicyMatchBuiltInClassifier = "built_in_classifier"
	// PolicyMatchLowTrustDisallowed means low-trust sources are blocked by policy.
	PolicyMatchLowTrustDisallowed = "low_trust_disallowed"
	// PolicyMatchNone means no specific policy rule matched.
	PolicyMatchNone = "none"
)

// Policy configures source trust and quality preferences.
//
// Nil booleans mean unset; Effective fills safe, usable defaults. Slice fields
// intentionally distinguish nil (inherit/unspecified) from an explicitly empty
// list in config merging code.
//
//nolint:govet // Field order follows the public YAML/JSON policy shape.
type Policy struct {
	TrustedDomains                     []string `json:"trusted_domains,omitempty" yaml:"trusted_domains,omitempty"`
	DeniedDomains                      []string `json:"denied_domains,omitempty" yaml:"denied_domains,omitempty"`
	PreferSourceTypes                  []string `json:"prefer_source_types,omitempty" yaml:"prefer_source_types,omitempty"`
	AllowLowTrustSources               *bool    `json:"allow_low_trust_sources,omitempty" yaml:"allow_low_trust_sources,omitempty"`
	WarnOnLowTrustSources              *bool    `json:"warn_on_low_trust_sources,omitempty" yaml:"warn_on_low_trust_sources,omitempty"`
	RequireEvidenceForHighImpactClaims *bool    `json:"require_evidence_for_high_impact_claims,omitempty" yaml:"require_evidence_for_high_impact_claims,omitempty"`
}

// EffectivePolicy is a fully defaulted policy used by runtime enforcement and
// persisted run metadata.
type EffectivePolicy struct {
	TrustedDomains                     []string `json:"trusted_domains,omitempty" yaml:"trusted_domains,omitempty"`
	DeniedDomains                      []string `json:"denied_domains,omitempty" yaml:"denied_domains,omitempty"`
	PreferSourceTypes                  []string `json:"prefer_source_types,omitempty" yaml:"prefer_source_types,omitempty"`
	AllowLowTrustSources               bool     `json:"allow_low_trust_sources" yaml:"allow_low_trust_sources"`
	WarnOnLowTrustSources              bool     `json:"warn_on_low_trust_sources" yaml:"warn_on_low_trust_sources"`
	RequireEvidenceForHighImpactClaims bool     `json:"require_evidence_for_high_impact_claims" yaml:"require_evidence_for_high_impact_claims"`
}

// Source describes a candidate source to classify.
type Source struct {
	URL        string
	Path       string
	Title      string
	SourceType string
}

// Quality is the source-quality metadata persisted on research and retrieval artifacts.
//
//nolint:govet // Field order follows the documented artifact shape.
type Quality struct {
	Domain      string  `json:"domain,omitempty" yaml:"domain,omitempty"`
	SourceType  string  `json:"source_type" yaml:"source_type"`
	TrustLevel  string  `json:"trust_level" yaml:"trust_level"`
	TrustScore  float64 `json:"trust_score" yaml:"trust_score"`
	PolicyMatch string  `json:"policy_match,omitempty" yaml:"policy_match,omitempty"`
}

// Evaluation is the result of classifying a source against a policy.
type Evaluation struct {
	Quality  Quality
	Warnings []string
	Allowed  bool
}

var bareDomainPattern = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:[a-z]{2,})\b`)

// Bool returns a bool pointer for policy literals.
func Bool(value bool) *bool {
	v := value
	return &v
}

// Default returns Atteler's built-in source policy defaults.
func Default() Policy {
	return Policy{
		PreferSourceTypes:                  []string{SourceTypeOfficialDocs, SourceTypeSourceCode, SourceTypeStandardOrSpec},
		AllowLowTrustSources:               Bool(true),
		WarnOnLowTrustSources:              Bool(true),
		RequireEvidenceForHighImpactClaims: Bool(false),
	}
}

// Clone returns a deep copy of policy.
func Clone(policy Policy) Policy {
	return Policy{
		TrustedDomains:                     cloneStrings(policy.TrustedDomains),
		DeniedDomains:                      cloneStrings(policy.DeniedDomains),
		PreferSourceTypes:                  cloneStrings(policy.PreferSourceTypes),
		AllowLowTrustSources:               cloneBool(policy.AllowLowTrustSources),
		WarnOnLowTrustSources:              cloneBool(policy.WarnOnLowTrustSources),
		RequireEvidenceForHighImpactClaims: cloneBool(policy.RequireEvidenceForHighImpactClaims),
	}
}

// Normalize canonicalizes domains/source types and de-duplicates lists.
func Normalize(policy Policy) Policy {
	policy = Clone(policy)
	if policy.TrustedDomains != nil {
		policy.TrustedDomains = NormalizeDomains(policy.TrustedDomains)
	}
	if policy.DeniedDomains != nil {
		policy.DeniedDomains = NormalizeDomains(policy.DeniedDomains)
	}
	if policy.PreferSourceTypes != nil {
		policy.PreferSourceTypes = NormalizeSourceTypes(policy.PreferSourceTypes)
	}

	return policy
}

// Effective normalizes policy and fills nil fields with built-in defaults.
func Effective(policy Policy) EffectivePolicy {
	defaults := Default()
	merged := Merge(defaults, policy)
	merged = Normalize(merged)

	return EffectivePolicy{
		TrustedDomains:                     cloneStrings(merged.TrustedDomains),
		DeniedDomains:                      cloneStrings(merged.DeniedDomains),
		PreferSourceTypes:                  cloneStrings(merged.PreferSourceTypes),
		AllowLowTrustSources:               boolValue(merged.AllowLowTrustSources, true),
		WarnOnLowTrustSources:              boolValue(merged.WarnOnLowTrustSources, true),
		RequireEvidenceForHighImpactClaims: boolValue(merged.RequireEvidenceForHighImpactClaims, false),
	}
}

// Merge overlays override on base. Non-nil slices replace entire lists; non-nil
// booleans replace their base value.
func Merge(base, override Policy) Policy {
	out := Clone(base)
	if override.TrustedDomains != nil {
		out.TrustedDomains = cloneStrings(override.TrustedDomains)
	}
	if override.DeniedDomains != nil {
		out.DeniedDomains = cloneStrings(override.DeniedDomains)
	}
	if override.PreferSourceTypes != nil {
		out.PreferSourceTypes = cloneStrings(override.PreferSourceTypes)
	}
	if override.AllowLowTrustSources != nil {
		out.AllowLowTrustSources = cloneBool(override.AllowLowTrustSources)
	}
	if override.WarnOnLowTrustSources != nil {
		out.WarnOnLowTrustSources = cloneBool(override.WarnOnLowTrustSources)
	}
	if override.RequireEvidenceForHighImpactClaims != nil {
		out.RequireEvidenceForHighImpactClaims = cloneBool(override.RequireEvidenceForHighImpactClaims)
	}

	return Normalize(out)
}

// Extend appends list fields and applies any explicit boolean overrides.
func Extend(base, extra Policy) Policy {
	out := Clone(base)
	out.TrustedDomains = appendUnique(out.TrustedDomains, extra.TrustedDomains, NormalizeDomain)
	out.DeniedDomains = appendUnique(out.DeniedDomains, extra.DeniedDomains, NormalizeDomain)
	out.PreferSourceTypes = appendUnique(out.PreferSourceTypes, extra.PreferSourceTypes, NormalizeSourceType)
	if extra.AllowLowTrustSources != nil {
		out.AllowLowTrustSources = cloneBool(extra.AllowLowTrustSources)
	}
	if extra.WarnOnLowTrustSources != nil {
		out.WarnOnLowTrustSources = cloneBool(extra.WarnOnLowTrustSources)
	}
	if extra.RequireEvidenceForHighImpactClaims != nil {
		out.RequireEvidenceForHighImpactClaims = cloneBool(extra.RequireEvidenceForHighImpactClaims)
	}

	return Normalize(out)
}

// Evaluate classifies source against policy and returns whether it is allowed.
func Evaluate(source Source, policy Policy) Evaluation {
	effective := Effective(policy)
	quality := classifySource(source)

	if domainMatches(quality.Domain, effective.DeniedDomains) {
		quality.TrustLevel = TrustLevelDenied
		quality.TrustScore = 0
		quality.PolicyMatch = PolicyMatchDeniedDomain

		return Evaluation{Quality: quality, Allowed: false, Warnings: []string{"source denied by source policy"}}
	}

	if domainMatches(quality.Domain, effective.TrustedDomains) {
		quality.TrustLevel = TrustLevelHigh
		quality.TrustScore = maxScore(quality.TrustScore, 0.95)
		quality.PolicyMatch = PolicyMatchTrustedDomain
	} else if sourceTypePreferred(quality.SourceType, effective.PreferSourceTypes) {
		quality.TrustScore = maxScore(quality.TrustScore, preferredTypeScore(quality.SourceType))
		quality.TrustLevel = trustLevelForScore(quality.TrustScore)
		if quality.PolicyMatch == "" || quality.PolicyMatch == PolicyMatchNone || quality.PolicyMatch == PolicyMatchBuiltInClassifier {
			quality.PolicyMatch = PolicyMatchPreferredType
		}
	}

	allowed := true
	warnings := []string(nil)
	if quality.TrustLevel == TrustLevelLow {
		if effective.WarnOnLowTrustSources {
			warnings = append(warnings, "low-trust source; corroborate before relying on high-impact claims")
		}
		if !effective.AllowLowTrustSources {
			quality.PolicyMatch = PolicyMatchLowTrustDisallowed
			allowed = false
			warnings = append(warnings, "low-trust source excluded by source policy")
		}
	}

	return Evaluation{Quality: quality, Allowed: allowed, Warnings: warnings}
}

// PolicyFromGuidance extracts simple source-policy hints from harness guidance text.
// It intentionally favors transparent, conservative heuristics over pretending
// to understand arbitrary natural language perfectly.
func PolicyFromGuidance(path, content string) Policy {
	var policy Policy
	for rawLine := range strings.SplitSeq(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		policy = extendPolicyFromGuidanceLine(policy, line)
	}

	_ = path // Kept for future path-specific guidance without changing the API.

	return Normalize(policy)
}

func extendPolicyFromGuidanceLine(policy Policy, line string) Policy {
	lower := strings.ToLower(line)
	deniedDomains := domainsMatchingGuidanceIntent(line, mentionsDeny)
	if len(deniedDomains) > 0 {
		policy.DeniedDomains = append(policy.DeniedDomains, deniedDomains...)
	}

	trustedDomains := domainsMatchingGuidanceIntent(line, mentionsTrustOrPreference)
	if len(trustedDomains) > 0 {
		policy.TrustedDomains = append(policy.TrustedDomains, withoutDomains(trustedDomains, deniedDomains)...)
	}

	policy.PreferSourceTypes = append(policy.PreferSourceTypes, sourceTypesMentioned(lower)...)

	if mentionsCitationRequirement(lower) {
		policy.RequireEvidenceForHighImpactClaims = Bool(true)
	}

	if mentionsLowTrustWarning(lower) {
		policy.WarnOnLowTrustSources = Bool(true)
	}

	if mentionsLowTrustBlock(lower) {
		policy.AllowLowTrustSources = Bool(false)
	}

	return policy
}

// NormalizeDomains canonicalizes and sorts domains.
func NormalizeDomains(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		domain := NormalizeDomain(value)
		if domain == "" || seen[domain] {
			continue
		}
		seen[domain] = true
		out = append(out, domain)
	}
	sort.Strings(out)

	return out
}

// RemoveDomains returns values without domains present in remove.
func RemoveDomains(values, remove []string) []string {
	if values == nil {
		return nil
	}

	if len(values) == 0 || len(remove) == 0 {
		return NormalizeDomains(values)
	}

	removeSet := make(map[string]bool, len(remove))
	for _, value := range NormalizeDomains(remove) {
		removeSet[value] = true
	}

	filtered := make([]string, 0, len(values))
	for _, value := range NormalizeDomains(values) {
		if removeSet[value] || domainConflictsAny(value, removeSet) {
			continue
		}
		filtered = append(filtered, value)
	}

	return filtered
}

func domainConflictsAny(domain string, domains map[string]bool) bool {
	for other := range domains {
		if domainsOverlap(domain, other) {
			return true
		}
	}

	return false
}

func domainsOverlap(left, right string) bool {
	left = NormalizeDomain(left)
	right = NormalizeDomain(right)
	if left == "" || right == "" {
		return false
	}

	return left == right || strings.HasSuffix(left, "."+right) || strings.HasSuffix(right, "."+left)
}

// NormalizeDomain returns a comparable lower-case host/domain.
func NormalizeDomain(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Hostname()
	}

	value = strings.TrimPrefix(value, "www.")
	value = strings.Trim(value, " /\t\r\n")
	if host, _, ok := strings.Cut(value, "/"); ok {
		value = host
	}
	if host, _, ok := strings.Cut(value, ":"); ok {
		value = host
	}

	return value
}

// NormalizeSourceTypes canonicalizes source type names and sorts them.
func NormalizeSourceTypes(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		typeName := NormalizeSourceType(value)
		if typeName == "" || seen[typeName] {
			continue
		}
		seen[typeName] = true
		out = append(out, typeName)
	}
	sort.Strings(out)

	return out
}

// NormalizeSourceType maps aliases to source policy source types.
func NormalizeSourceType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.ReplaceAll(value, " ", "_")
	switch value {
	case "official", "official_doc", "official_docs", "official_documentation", "documentation", "docs":
		return SourceTypeOfficialDocs
	case "source", "source_repo", "source_repos", "source_repository", "source_repositories", "source_code", "code", "repository_file", "repo", "repos":
		return SourceTypeSourceCode
	case "standard", "standards", "spec", "specs", "specification", "specifications", "standard_or_spec", "rfc", "rfcs":
		return SourceTypeStandardOrSpec
	case SourceTypeAcademic, "paper", "papers", "research_paper", "university":
		return SourceTypeAcademic
	case "vendor", "vendor_blog", "vendor_blogs":
		return SourceTypeVendorBlog
	case "project_blog", "project_blogs", "blog":
		return SourceTypeProjectBlog
	case "issue", "issues", "issue_discussion", "issue_discussions", "pull_request", "pull_requests", "discussion", "discussions":
		return SourceTypeIssueDiscussion
	case "forum", "forums", "community", "q_a", "qa":
		return SourceTypeForum
	case "news", "article", "magazine":
		return SourceTypeNews
	case "unknown", "url", "trusted_url":
		return SourceTypeUnknown
	case SourceTypeProjectGuidance, "agents_instructions", "claude_instructions", "cursor_rules", "harness_guidance":
		return SourceTypeProjectGuidance
	default:
		return value
	}
}

func classifySource(source Source) Quality {
	sourceType := NormalizeSourceType(source.SourceType)
	domain := domainForSource(source)

	if sourceType == "" || sourceType == SourceTypeUnknown {
		sourceType = classifySourceType(domain, source.URL, source.Path)
	}

	quality := Quality{
		Domain:      domain,
		SourceType:  sourceType,
		TrustScore:  scoreForType(sourceType, domain, source.Path),
		PolicyMatch: PolicyMatchBuiltInClassifier,
	}
	quality.TrustLevel = trustLevelForScore(quality.TrustScore)

	switch sourceType {
	case SourceTypeProjectGuidance:
		quality.TrustScore = 0.90
		quality.TrustLevel = TrustLevelHigh
		quality.PolicyMatch = PolicyMatchHarnessGuidance
	case SourceTypeSourceCode:
		if source.Path != "" && domain == "" {
			quality.TrustScore = 0.80
			quality.TrustLevel = TrustLevelHigh
			quality.PolicyMatch = PolicyMatchLocalRepository
		}
	}

	if quality.PolicyMatch == "" {
		quality.PolicyMatch = PolicyMatchNone
	}

	return quality
}

func domainForSource(source Source) string {
	if source.URL != "" {
		if parsed, err := url.Parse(strings.TrimSpace(source.URL)); err == nil && parsed.Host != "" {
			return NormalizeDomain(parsed.Hostname())
		}
	}

	if strings.HasPrefix(source.Path, "http://") || strings.HasPrefix(source.Path, "https://") {
		return domainForSource(Source{URL: source.Path})
	}

	return ""
}

func classifySourceType(domain, rawURL, path string) string {
	domain = NormalizeDomain(domain)
	pathLower := strings.ToLower(path)
	if rawURL != "" {
		if parsed, err := url.Parse(rawURL); err == nil {
			pathLower = strings.ToLower(parsed.Path)
		}
	}

	if domain == "" && strings.TrimSpace(path) != "" {
		return SourceTypeSourceCode
	}

	if isOfficialDocs(domain, pathLower) {
		return SourceTypeOfficialDocs
	}
	if isIssueDiscussion(domain, pathLower) {
		return SourceTypeIssueDiscussion
	}
	if isSourceRepository(domain, pathLower) {
		return SourceTypeSourceCode
	}
	if isStandardHost(domain) {
		return SourceTypeStandardOrSpec
	}
	if isAcademicHost(domain) {
		return SourceTypeAcademic
	}
	if isForumHost(domain) {
		return SourceTypeForum
	}
	if isNewsHost(domain) {
		return SourceTypeNews
	}
	if strings.Contains(domain, "blog") || strings.Contains(pathLower, "/blog") || strings.Contains(pathLower, "/posts/") {
		return SourceTypeVendorBlog
	}

	return SourceTypeUnknown
}

func scoreForType(sourceType, domain, path string) float64 {
	switch sourceType {
	case SourceTypeOfficialDocs:
		return 0.90
	case SourceTypeSourceCode:
		if path != "" && domain == "" {
			return 0.80
		}
		return 0.85
	case SourceTypeStandardOrSpec:
		return 0.90
	case SourceTypeAcademic:
		return 0.85
	case SourceTypeProjectGuidance:
		return 0.90
	case SourceTypeIssueDiscussion:
		return 0.70
	case SourceTypeProjectBlog:
		return 0.70
	case SourceTypeVendorBlog:
		return 0.60
	case SourceTypeNews:
		return 0.55
	case SourceTypeForum:
		return 0.45
	default:
		return 0.50
	}
}

func preferredTypeScore(sourceType string) float64 {
	switch sourceType {
	case SourceTypeOfficialDocs, SourceTypeStandardOrSpec:
		return 0.90
	case SourceTypeSourceCode, SourceTypeAcademic:
		return 0.85
	case SourceTypeIssueDiscussion, SourceTypeProjectBlog:
		return 0.70
	default:
		return 0.60
	}
}

func trustLevelForScore(score float64) string {
	switch {
	case score >= 0.75:
		return TrustLevelHigh
	case score >= 0.55:
		return TrustLevelMedium
	case score > 0:
		return TrustLevelLow
	default:
		return TrustLevelUnknown
	}
}

func isOfficialDocs(domain, path string) bool {
	if strings.HasPrefix(domain, "docs.") || strings.Contains(domain, ".docs.") || strings.HasPrefix(domain, "developer.") {
		return true
	}
	switch domain {
	case "go.dev", "pkg.go.dev", "docs.github.com", "developer.mozilla.org", "docs.anthropic.com", "platform.openai.com", "docs.aws.amazon.com", "learn.microsoft.com":
		return true
	case domainGitHub:
		return strings.HasPrefix(path, "/docs/")
	default:
		return strings.Contains(path, "/docs/") || strings.Contains(path, "/documentation/")
	}
}

func isSourceRepository(domain, path string) bool {
	switch domain {
	case domainGitHub, "gitlab.com", "bitbucket.org", "sourcehut.org", "sr.ht":
		return true
	default:
		return strings.Contains(path, "/src/") || strings.Contains(path, "/source/")
	}
}

func isStandardHost(domain string) bool {
	switch domain {
	case "ietf.org", "rfc-editor.org", "www.rfc-editor.org", "w3.org", "www.w3.org", "whatwg.org", "tc39.es", "ecma-international.org", "iso.org":
		return true
	default:
		return strings.HasSuffix(domain, ".ietf.org") || strings.HasSuffix(domain, ".w3.org")
	}
}

func isAcademicHost(domain string) bool {
	return strings.HasSuffix(domain, ".edu") || domain == "arxiv.org" || domain == "doi.org" || domain == "acm.org" || domain == "ieeexplore.ieee.org" || strings.HasSuffix(domain, ".acm.org")
}

func isIssueDiscussion(domain, path string) bool {
	if domain == domainGitHub || domain == "gitlab.com" {
		return strings.Contains(path, "/issues/") || strings.Contains(path, "/pull/") || strings.Contains(path, "/merge_requests/") || strings.Contains(path, "/discussions/")
	}

	return strings.Contains(path, "/issues/") || strings.Contains(path, "/discussions/")
}

func isForumHost(domain string) bool {
	return domain == "stackoverflow.com" || domain == "serverfault.com" || domain == "superuser.com" || domain == "reddit.com" || strings.HasSuffix(domain, ".reddit.com") || strings.Contains(domain, "discourse") || strings.Contains(domain, "forum")
}

func isNewsHost(domain string) bool {
	switch domain {
	case "news.ycombinator.com", "theverge.com", "wired.com", "techcrunch.com", "arstechnica.com":
		return true
	default:
		return false
	}
}

func domainMatches(domain string, policyDomains []string) bool {
	domain = NormalizeDomain(domain)
	if domain == "" {
		return false
	}
	for _, policyDomain := range policyDomains {
		policyDomain = NormalizeDomain(policyDomain)
		if policyDomain == "" {
			continue
		}
		if domain == policyDomain || strings.HasSuffix(domain, "."+policyDomain) {
			return true
		}
	}

	return false
}

func sourceTypePreferred(sourceType string, prefer []string) bool {
	sourceType = NormalizeSourceType(sourceType)
	for _, preferred := range prefer {
		if sourceType == NormalizeSourceType(preferred) {
			return true
		}
	}

	return false
}

func domainsInText(text string) []string {
	matches := bareDomainPattern.FindAllString(text, -1)
	return NormalizeDomains(matches)
}

func domainsMatchingGuidanceIntent(line string, matches func(string) bool) []string {
	var domains []string
	for _, clause := range guidanceClauses(line) {
		if !matches(strings.ToLower(clause)) {
			continue
		}

		domains = append(domains, domainsInText(clause)...)
	}

	return NormalizeDomains(domains)
}

func guidanceClauses(line string) []string {
	line = strings.ToLower(line)
	normalized := strings.NewReplacer(
		";", "\n",
		", prefer ", "\nprefer ",
		", trusted ", "\ntrusted ",
		", trust ", "\ntrust ",
		", but prefer ", "\nprefer ",
		", but trusted ", "\ntrusted ",
		", but trust ", "\ntrust ",
		", do not use ", "\ndo not use ",
		", don't use ", "\ndon't use ",
		", deny ", "\ndeny ",
		", denied ", "\ndenied ",
		", block ", "\nblock ",
		" and prefer ", "\nprefer ",
		" and trusted ", "\ntrusted ",
		" and trust ", "\ntrust ",
		" but prefer ", "\nprefer ",
		" but trusted ", "\ntrusted ",
		" but trust ", "\ntrust ",
		" and do not use ", "\ndo not use ",
		" and don't use ", "\ndon't use ",
		" and deny ", "\ndeny ",
		" and denied ", "\ndenied ",
		" and block ", "\nblock ",
		" but do not use ", "\ndo not use ",
		" but don't use ", "\ndon't use ",
		" but deny ", "\ndeny ",
		" but denied ", "\ndenied ",
		" but block ", "\nblock ",
	).Replace(line)

	return strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '\n'
	})
}

func withoutDomains(values, denied []string) []string {
	if len(values) == 0 || len(denied) == 0 {
		return values
	}

	deniedSet := make(map[string]bool, len(denied))
	for _, domain := range NormalizeDomains(denied) {
		deniedSet[domain] = true
	}

	filtered := make([]string, 0, len(values))
	for _, domain := range NormalizeDomains(values) {
		if deniedSet[domain] {
			continue
		}
		filtered = append(filtered, domain)
	}

	return filtered
}

func mentionsDeny(lower string) bool {
	return strings.Contains(lower, "deny") || strings.Contains(lower, "denied") || strings.Contains(lower, "blocked") || strings.Contains(lower, "blocklist") || strings.Contains(lower, "avoid") || strings.Contains(lower, "do not use") || strings.Contains(lower, "don't use") || strings.Contains(lower, "untrusted") || strings.Contains(lower, "content farm")
}

func mentionsTrustOrPreference(lower string) bool {
	return strings.Contains(lower, "trusted") || strings.Contains(lower, "prefer") || strings.Contains(lower, "allowed") || strings.Contains(lower, "allowlist") || strings.Contains(lower, "official") || strings.Contains(lower, "source restriction") || strings.Contains(lower, "source policy")
}

func mentionsCitationRequirement(lower string) bool {
	return strings.Contains(lower, "require citations") || strings.Contains(lower, "requires citations") || strings.Contains(lower, "citation required") || strings.Contains(lower, "citations required") || strings.Contains(lower, "must cite") || strings.Contains(lower, "cite evidence") || strings.Contains(lower, "use citations")
}

func mentionsLowTrustWarning(lower string) bool {
	return strings.Contains(lower, "warn low trust") || strings.Contains(lower, "warn low-trust") || strings.Contains(lower, "warn on low trust") || strings.Contains(lower, "warn on low-trust") || strings.Contains(lower, "mark low-trust") || strings.Contains(lower, "mark low trust") || strings.Contains(lower, "flag low-trust") || strings.Contains(lower, "flag low trust")
}

func mentionsLowTrustBlock(lower string) bool {
	return strings.Contains(lower, "block low trust") || strings.Contains(lower, "block low-trust") || strings.Contains(lower, "exclude low trust") || strings.Contains(lower, "exclude low-trust") || strings.Contains(lower, "deny low trust") || strings.Contains(lower, "deny low-trust")
}

//nolint:cyclop // Source-type keyword matching is intentionally transparent and local.
func sourceTypesMentioned(lower string) []string {
	var out []string
	if strings.Contains(lower, "official doc") || strings.Contains(lower, "official documentation") || strings.Contains(lower, "first-party doc") {
		out = append(out, SourceTypeOfficialDocs)
	}
	if strings.Contains(lower, "source code") || strings.Contains(lower, "source repos") || strings.Contains(lower, "source repositories") || strings.Contains(lower, "repository") {
		out = append(out, SourceTypeSourceCode)
	}
	if strings.Contains(lower, "standard") || strings.Contains(lower, "specification") || strings.Contains(lower, "spec ") || strings.Contains(lower, "rfc") {
		out = append(out, SourceTypeStandardOrSpec)
	}
	if strings.Contains(lower, "issue discussion") || strings.Contains(lower, "issue tracker") || strings.Contains(lower, "pull request") || strings.Contains(lower, "github issue") {
		out = append(out, SourceTypeIssueDiscussion)
	}
	if strings.Contains(lower, "academic") || strings.Contains(lower, "paper") || strings.Contains(lower, "journal") {
		out = append(out, SourceTypeAcademic)
	}

	return out
}

func maxScore(left, right float64) float64 {
	if right > left {
		return right
	}
	return left
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func appendUnique(values, extra []string, normalize func(string) string) []string {
	if values == nil && extra == nil {
		return nil
	}

	seen := make(map[string]bool, len(values)+len(extra))
	out := make([]string, 0, len(values)+len(extra))
	for _, value := range values {
		normalized := normalize(value)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	for _, value := range extra {
		normalized := normalize(value)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	sort.Strings(out)

	return out
}
