package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/tommoulard/atteler/pkg/config"
	"github.com/tommoulard/atteler/pkg/contextpack"
	"github.com/tommoulard/atteler/pkg/contextref"
	"github.com/tommoulard/atteler/pkg/llm"
	"github.com/tommoulard/atteler/pkg/privacy"
	"github.com/tommoulard/atteler/pkg/retrieval"
	"github.com/tommoulard/atteler/pkg/vector"
)

const (
	defaultWorkspaceVectorLimit   = 4
	workspaceVectorStore          = "workspace"
	workspaceVectorReferenceScope = "workspace-vector"
)

var workspaceVectorWarningURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func workspaceVectorReferenceContextWithWarning(
	ctx context.Context,
	cwd string,
	cfg appconfig.VectorConfig,
	prompt string,
	warn bool,
	contextOptions ...contextref.Options,
) configuredReferenceContext {
	refCtx, _, err := buildWorkspaceVectorReferenceContext(ctx, cwd, cfg, prompt, contextOptions...)
	if err != nil {
		if warn {
			warnWorkspaceVectorContextOmitted(err)
		}

		return configuredReferenceContext{}
	}

	return refCtx
}

func warnWorkspaceVectorContextOmitted(err error) {
	if err == nil ||
		errors.Is(err, vector.ErrNoSources) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return
	}

	fmt.Fprintln(os.Stderr, "warning: workspace vector context omitted: "+redactWorkspaceVectorWarning(err.Error()))
}

func redactWorkspaceVectorWarning(value string) string {
	value = privacy.RedactIdentifier(value)

	return workspaceVectorWarningURLPattern.ReplaceAllStringFunc(value, redactWorkspaceVectorWarningURL)
}

func redactWorkspaceVectorWarningURL(raw string) string {
	urlText := strings.TrimRight(raw, ".,;)")

	suffix := strings.TrimPrefix(raw, urlText)
	if urlText == "" {
		return raw
	}

	parsed, err := url.Parse(urlText)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}

	return vector.NewEmbeddingMetadata("", "", urlText, 0).BaseURL + suffix
}

func buildWorkspaceVectorReferenceContext(
	ctx context.Context,
	cwd string,
	cfg appconfig.VectorConfig,
	prompt string,
	contextOptions ...contextref.Options,
) (configuredReferenceContext, vector.WorkspaceRefreshResult, error) {
	if !workspaceVectorEnabled(cfg) || strings.TrimSpace(prompt) == "" {
		return configuredReferenceContext{}, vector.WorkspaceRefreshResult{}, nil
	}

	settings, opts, err := workspaceVectorSettings(cwd, cfg)
	if err != nil {
		return configuredReferenceContext{}, vector.WorkspaceRefreshResult{}, err
	}

	if settings.Vectorizer == vector.VectorizerKindEmbedding &&
		!workspaceRemoteEmbeddingAllowed(settings.BaseURL, cfg.WorkspaceAllowRemoteEmbeddings) {
		if settings.FallbackPolicy == vector.VectorizerKindLexical {
			return buildWorkspaceVectorReferenceContextWithLexicalFallback(ctx, settings, opts, prompt, contextOptions...)
		}

		return configuredReferenceContext{}, vector.WorkspaceRefreshResult{}, fmt.Errorf("workspace vector: remote embedding endpoint %s is not allowed without vector.workspace_allow_remote_embeddings", workspaceDisplayEmbeddingEndpoint(settings.BaseURL))
	}

	refCtx, result, err := buildWorkspaceVectorReferenceContextOnce(ctx, settings, opts, prompt, contextOptions...)
	if err == nil ||
		settings.Vectorizer != vector.VectorizerKindEmbedding ||
		settings.FallbackPolicy != vector.VectorizerKindLexical ||
		!vectorSearchEmbeddingFailure(err) {
		return refCtx, result, err
	}

	return buildWorkspaceVectorReferenceContextWithLexicalFallback(ctx, settings, opts, prompt, contextOptions...)
}

func buildWorkspaceVectorReferenceContextWithLexicalFallback(
	ctx context.Context,
	settings vectorSearchSettings,
	opts vector.WorkspaceOptions,
	prompt string,
	contextOptions ...contextref.Options,
) (configuredReferenceContext, vector.WorkspaceRefreshResult, error) {
	settings, opts, err := workspaceLexicalFallbackSettings(settings, opts)
	if err != nil {
		return configuredReferenceContext{}, vector.WorkspaceRefreshResult{}, err
	}

	return buildWorkspaceVectorReferenceContextOnce(ctx, settings, opts, prompt, contextOptions...)
}

func workspaceLexicalFallbackSettings(
	settings vectorSearchSettings,
	opts vector.WorkspaceOptions,
) (vectorSearchSettings, vector.WorkspaceOptions, error) {
	settings.Vectorizer = vector.VectorizerKindLexical
	settings.Provider = ""
	settings.BaseURL = ""
	settings.Model = vector.LexicalFallbackModel
	settings.FallbackPolicy = vectorFallbackPolicyFail
	settings.IndexPath = lexicalFallbackIndexPath(settings.IndexPath)

	vectorizer, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return vectorSearchSettings{}, vector.WorkspaceOptions{}, err
	}

	opts.Vectorizer = vectorizer
	opts.VectorizerMetadata = metadata
	opts.IndexPath = settings.IndexPath

	return settings, opts, nil
}

func buildWorkspaceVectorReferenceContextOnce(
	ctx context.Context,
	settings vectorSearchSettings,
	opts vector.WorkspaceOptions,
	prompt string,
	contextOptions ...contextref.Options,
) (configuredReferenceContext, vector.WorkspaceRefreshResult, error) {
	idx, refresh, err := refreshWorkspaceVectorIndex(ctx, opts)
	if err != nil {
		return configuredReferenceContext{}, refresh, err
	}

	searcher, err := newVectorIndexRetrievalSearcher(
		idx,
		opts.Vectorizer,
		retrieval.Source{
			Type: retrieval.SourceVector,
			Name: "workspace",
			URI:  workspaceVectorSourceURI(opts),
		},
		settings.Vectorizer+"-workspace-ann",
	)
	if err != nil {
		return configuredReferenceContext{}, refresh, err
	}

	results, err := retrieval.Search(ctx, retrieval.Query{
		Text:  prompt,
		Limit: settings.Limit,
	}, searcher)
	if err != nil {
		return configuredReferenceContext{}, refresh, fmt.Errorf("search workspace vector index: %w", err)
	}

	displayRefresh := refresh
	displayRefresh.IndexPath = workspaceDisplayIndexPath(opts.Root, refresh.IndexPath)

	content := formatWorkspaceVectorReferenceContext(results, displayRefresh, idx.Vectorizer)
	if content == "" {
		return configuredReferenceContext{}, refresh, nil
	}

	manifest := workspaceVectorReferenceManifest(content, displayRefresh, len(results), idx.Vectorizer, firstContextOptions(contextOptions))

	return configuredReferenceContext{
		Content:   content,
		Manifest:  manifest,
		Estimator: manifest.TokenEstimator,
	}, refresh, nil
}

func refreshWorkspaceVectorIndex(ctx context.Context, opts vector.WorkspaceOptions) (*vector.Index, vector.WorkspaceRefreshResult, error) {
	result, err := vector.RefreshWorkspaceIndex(ctx, opts)
	if err != nil {
		return nil, result, fmt.Errorf("refresh workspace vector index: %w", err)
	}

	return result.Index, result, nil
}

func workspaceVectorSettings(cwd string, cfg appconfig.VectorConfig) (vectorSearchSettings, vector.WorkspaceOptions, error) {
	resolved := cfg.ResolveVectorizerConfig(appconfig.VectorScope{
		Store:  workspaceVectorStore,
		Source: vector.SourceKindFile,
	})
	storeConfig := scopedVectorizerConfig(cfg.Stores, workspaceVectorStore)
	sourceConfig := scopedVectorizerConfig(cfg.Sources, vector.SourceKindFile)

	settings := vectorSearchSettings{
		Vectorizer:     firstNonEmpty(resolved.Vectorizer, vector.VectorizerKindLexical),
		Provider:       firstNonEmpty(resolved.Provider, vectorDefaultProvider),
		Model:          firstNonEmpty(resolved.Model),
		BaseURL:        firstNonEmpty(resolved.BaseURL),
		FallbackPolicy: firstNonEmpty(resolved.FallbackPolicy, vectorFallbackPolicyFail),
		IndexPath:      firstNonEmpty(sourceConfig.IndexPath, storeConfig.IndexPath, cfg.WorkspaceIndexPath, vector.DefaultWorkspaceIndexPath),
		Limit:          cfg.WorkspaceLimit,
		Chunk: vector.ChunkOptions{
			MaxRunes:     resolved.ChunkMaxRunes,
			OverlapRunes: resolved.ChunkOverlapRunes,
		},
	}
	if settings.Limit <= 0 {
		settings.Limit = defaultWorkspaceVectorLimit
	}

	if resolved.TimeoutSeconds > 0 {
		settings.Timeout = time.Duration(resolved.TimeoutSeconds) * time.Second
	}

	settings.Chunk = settings.Chunk.Normalize()

	var err error

	settings.Vectorizer, err = normalizeVectorizerKind(settings.Vectorizer)
	if err != nil {
		return vectorSearchSettings{}, vector.WorkspaceOptions{}, err
	}

	settings.FallbackPolicy, err = normalizeVectorFallbackPolicy(settings.FallbackPolicy)
	if err != nil {
		return vectorSearchSettings{}, vector.WorkspaceOptions{}, err
	}

	vectorizer, metadata, err := newVectorSearchVectorizer(settings)
	if err != nil {
		return vectorSearchSettings{}, vector.WorkspaceOptions{}, err
	}

	opts := vector.WorkspaceOptions{
		Root:               cwd,
		IndexPath:          settings.IndexPath,
		Vectorizer:         vectorizer,
		VectorizerMetadata: metadata,
		Chunk:              settings.Chunk,
		IncludePatterns:    append([]string(nil), cfg.WorkspaceInclude...),
		ExcludePatterns:    append([]string(nil), cfg.WorkspaceExclude...),
		MaxFileBytes:       int64(cfg.WorkspaceMaxFileBytes),
		MaxFiles:           cfg.WorkspaceMaxFiles,
	}

	return settings, opts, nil
}

func workspaceVectorEnabled(cfg appconfig.VectorConfig) bool {
	return cfg.WorkspaceEnabled != nil && *cfg.WorkspaceEnabled
}

func workspaceRemoteEmbeddingAllowed(rawBaseURL string, allow *bool) bool {
	if allow != nil && *allow {
		return true
	}

	if strings.TrimSpace(rawBaseURL) == "" {
		rawBaseURL = vectorDefaultBaseURL
	}

	parsed, err := url.Parse(rawBaseURL)
	if err != nil {
		return false
	}

	host := parsed.Hostname()
	if host == "" {
		return false
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	return ip.IsLoopback()
}

func workspaceDisplayEmbeddingEndpoint(rawBaseURL string) string {
	if strings.TrimSpace(rawBaseURL) == "" {
		rawBaseURL = vectorDefaultBaseURL
	}

	return vector.NewEmbeddingMetadata(vectorDefaultProvider, vectorDefaultModel, rawBaseURL, 0).BaseURL
}

func workspaceDisplayIndexPath(root, indexPath string) string {
	indexPath = strings.TrimSpace(indexPath)
	if indexPath == "" {
		return ""
	}

	root = strings.TrimSpace(root)
	if root == "" {
		if filepath.IsAbs(indexPath) {
			return ""
		}

		return filepath.ToSlash(privacy.RedactIdentifier(indexPath))
	}

	absRoot, err := filepath.Abs(root)
	if err == nil {
		root = absRoot
	}

	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}

	rel, err := filepath.Rel(root, indexPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return ""
	}

	return filepath.ToSlash(privacy.RedactIdentifier(rel))
}

func workspaceVectorSourceURI(opts vector.WorkspaceOptions) string {
	if display := workspaceDisplayIndexPath(opts.Root, opts.IndexPath); display != "" {
		return display
	}

	return "workspace"
}

func formatWorkspaceVectorReferenceContext(results []retrieval.Result, refresh vector.WorkspaceRefreshResult, metadata vector.VectorizerMetadata) string {
	if len(results) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<workspace_vector_context")

	if refresh.IndexPath != "" {
		b.WriteString(` index_path="`)
		b.WriteString(escapeContextAttr(refresh.IndexPath))
		b.WriteString(`"`)
	}

	b.WriteString(` vectorizer="`)
	b.WriteString(escapeContextAttr(metadata.Label()))
	b.WriteString(`"`)

	if metadata.Model != "" {
		b.WriteString(` model="`)
		b.WriteString(escapeContextAttr(metadata.Model))
		b.WriteString(`"`)
	}

	if refresh.Index != nil {
		if createdAt := formatVectorIndexTimestamp(refresh.Index.CreatedAt); createdAt != "" {
			b.WriteString(` created_at="`)
			b.WriteString(escapeContextAttr(createdAt))
			b.WriteString(`"`)
		}

		if updatedAt := formatVectorIndexTimestamp(refresh.Index.UpdatedAt); updatedAt != "" {
			b.WriteString(` updated_at="`)
			b.WriteString(escapeContextAttr(updatedAt))
			b.WriteString(`"`)
		}
	}

	b.WriteString(">\n")

	for i := range results {
		result := retrieval.NormalizeResult(results[i])
		path := firstNonEmpty(result.Metadata["path"], result.DocumentID)

		b.WriteString(`<chunk rank="`)
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(`" path="`)
		b.WriteString(escapeContextAttr(path))
		b.WriteString(`" score="`)
		_, _ = fmt.Fprintf(&b, "%.4f", result.Score)
		b.WriteString(`" range="`)
		b.WriteString(strconv.Itoa(result.Chunk.Range.Start))
		b.WriteString(":")
		b.WriteString(strconv.Itoa(result.Chunk.Range.End))
		b.WriteString(`">`)
		b.WriteString("\n")
		b.WriteString(escapeContextText(result.Snippet))
		b.WriteString("\n</chunk>\n")
	}

	b.WriteString("</workspace_vector_context>")

	return b.String()
}

func workspaceVectorReferenceManifest(
	content string,
	refresh vector.WorkspaceRefreshResult,
	resultCount int,
	metadata vector.VectorizerMetadata,
	opts contextref.Options,
) contextref.ReferenceManifest {
	content = strings.TrimSpace(content)
	if content == "" {
		return contextref.ReferenceManifest{}
	}

	estimator := opts.TokenEstimator
	if estimator == nil {
		estimator = contextpack.DefaultEstimator()
	}

	estimate := estimator.EstimateMessage(llm.Message{Role: llm.RoleSystem, Content: content})
	estimatorSummary := contextEstimatorSummary(estimator.Profile())
	sum := sha256.Sum256([]byte(content))

	manifest := contextref.BuildReferenceManifest([]contextref.ReferenceEvent{{
		Source:         "workspace-vector",
		ResolvedSource: refresh.IndexPath,
		Kind:           "vector",
		Scope:          workspaceVectorReferenceScope,
		Location:       "local",
		TokenEstimator: estimatorSummary,
		Bytes:          len([]byte(content)),
		DigestSHA256:   hex.EncodeToString(sum[:]),
		PolicyDecision: contextref.ReferenceDecisionLoaded,
		PolicyReason:   fmt.Sprintf("retrieved %d workspace vector chunk(s) with %s", resultCount, metadata.Label()),
		TokenEstimate:  estimate,
	}})
	if manifest.TokenEstimator == "" {
		manifest.TokenEstimator = estimatorSummary
	}

	return manifest
}

func firstContextOptions(options []contextref.Options) contextref.Options {
	if len(options) == 0 {
		return contextref.Options{}
	}

	return options[0]
}

func escapeContextAttr(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")

	return value
}

func escapeContextText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")

	return value
}
