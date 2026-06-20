// Package contextref expands local @file prompt references into LLM context.
package contextref

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/tommoulard/atteler/pkg/contextpack"
)

const (
	// DefaultMaxFileBytes is the default per-file reference cap.
	DefaultMaxFileBytes = 64 * 1024
	// DefaultMaxTotalBytes is the default aggregate reference cap.
	DefaultMaxTotalBytes = 256 * 1024
	// DefaultMaxImageBytes is the default cap for one inline image reference.
	DefaultMaxImageBytes = 5 * 1024 * 1024
	maxDirectoryEntries  = 200

	// kindFile is the reference kind for regular files.
	kindFile = "file"
	// kindImage is the reference kind for inline image attachments.
	kindImage = "image"
)

var errDirectoryLimit = errors.New("directory reference limit reached")

// Options configures reference expansion.
//
//nolint:govet // field order keeps root/size options first for callers.
type Options struct {
	Root          string
	MaxFileBytes  int
	MaxTotalBytes int
	MaxImageBytes int
	// TokenEstimator records provider/model-calibrated token estimates for
	// reference manifests. If nil, a conservative provider-agnostic estimator is
	// used.
	TokenEstimator contextpack.Estimator
	// ReferencePolicy constrains configured reference ingestion. It is used by
	// LoadReferences; inline @path expansion remains rooted under Root.
	ReferencePolicy ReferencePolicy
	// ReferenceScope records where configured references came from, for example
	// "global" or "agent:reviewer".
	ReferenceScope string
}

// Reference describes one expanded local file, directory tree, or inline image.
type Reference struct {
	Path           string
	Kind           string
	MediaType      string
	TokenEstimator string
	DigestSHA256   string
	Bytes          int
	Truncated      bool
	TokenEstimate  contextpack.TokenEstimate
}

// Result is the expanded prompt and reference metadata.
type Result struct {
	Prompt     string
	References []Reference
	Images     []ImageReference
	Events     []ReferenceEvent
}

// ImageReference is an inline image attachment resolved from an @path token.
// DataBase64 contains the full image bytes encoded with standard base64.
type ImageReference struct {
	Path         string
	MediaType    string
	DataBase64   string
	DigestSHA256 string
	Bytes        int
}

// Expand appends referenced local file contents or directory trees to prompt
// and resolves image @path tokens as inline image attachments. References are
// written as @path tokens and must resolve under Options.Root.
func Expand(prompt string, opts Options) (Result, error) {
	result, _, err := ExpandWithReport(prompt, opts)
	return result, err
}

// ExpandWithReport is Expand plus per-inline-reference audit events. It
// returns the events collected before an error so callers can still emit a
// manifest for rejected @path attempts that abort request assembly.
func ExpandWithReport(prompt string, opts Options) (Result, []ReferenceEvent, error) {
	opts = normalizeOptions(opts)

	candidates := parseCandidates(prompt)
	if len(candidates) == 0 {
		return Result{Prompt: prompt}, nil, nil
	}

	seen := make(map[string]bool, len(candidates))

	var (
		refs   []expandedReference
		events []ReferenceEvent
	)

	total := 0
	for _, candidate := range candidates {
		ref, nextTotal, ok, err := expandCandidate(candidate, opts, total, seen)
		if err != nil {
			events = append(events, rejectedInlineReferenceEvent(candidate, opts, err))
			return Result{}, events, err
		}

		if !ok {
			continue
		}

		total = nextTotal

		refs = append(refs, ref)
		events = append(events, inlineReferenceEvent(ref.Reference()))
	}

	if len(refs) == 0 {
		return Result{Prompt: prompt, Events: events}, events, nil
	}

	result := Result{
		Prompt:     appendReferences(prompt, refs),
		References: references(refs),
		Images:     imageReferences(refs),
		Events:     events,
	}

	return result, events, nil
}

type expandedReference struct {
	content         string
	Path            string
	Kind            string
	MediaType       string
	ImageDataBase64 string
	TokenEstimator  string
	DigestSHA256    string
	Bytes           int
	Truncated       bool
	TokenEstimate   contextpack.TokenEstimate
}

func (r expandedReference) Reference() Reference {
	return Reference{
		Path:           r.Path,
		Kind:           r.Kind,
		MediaType:      r.MediaType,
		TokenEstimator: r.TokenEstimator,
		DigestSHA256:   r.DigestSHA256,
		Bytes:          r.Bytes,
		Truncated:      r.Truncated,
		TokenEstimate:  r.TokenEstimate,
	}
}

func (r expandedReference) ImageReference() (ImageReference, bool) {
	if r.Kind != kindImage || r.ImageDataBase64 == "" {
		return ImageReference{}, false
	}

	return ImageReference{
		Path:         r.Path,
		MediaType:    r.MediaType,
		DataBase64:   r.ImageDataBase64,
		DigestSHA256: r.DigestSHA256,
		Bytes:        r.Bytes,
	}, true
}

func normalizeOptions(opts Options) Options {
	if opts.Root == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.Root = cwd
		}
	}

	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}

	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}

	if opts.MaxImageBytes <= 0 {
		opts.MaxImageBytes = DefaultMaxImageBytes
	}

	if opts.TokenEstimator == nil {
		opts.TokenEstimator = contextpack.DefaultEstimator()
	}

	return opts
}

func parseCandidates(prompt string) []string {
	var out []string

	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '@' {
			continue
		}

		if i > 0 && isWord(rune(prompt[i-1])) {
			continue
		}

		j := i + 1
		for j < len(prompt) && isPathRune(rune(prompt[j])) {
			j++
		}

		if j == i+1 {
			continue
		}

		candidate := strings.TrimRight(prompt[i+1:j], ".,;:!?)]}")
		if candidate != "" {
			out = append(out, candidate)
		}

		i = j - 1
	}

	return out
}

func isPathRune(r rune) bool {
	return unicode.IsLetter(r) ||
		unicode.IsDigit(r) ||
		r == '/' ||
		r == '\\' ||
		r == '.' ||
		r == '_' ||
		r == '-'
}

func isWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func pathLike(candidate string) bool {
	return strings.ContainsAny(candidate, `/\.`)
}

func expandCandidate(
	candidate string,
	opts Options,
	total int,
	seen map[string]bool,
) (ref expandedReference, nextTotal int, ok bool, err error) {
	resolved, displayPath, err := resolve(opts.Root, candidate)
	if err != nil {
		return expandedReference{}, total, false, err
	}

	_, err = os.Lstat(resolved)
	if err != nil {
		if !pathLike(candidate) && errors.Is(err, os.ErrNotExist) {
			return expandedReference{}, total, false, nil
		}

		return expandedReference{}, total, false, fmt.Errorf("context: stat @%s: %s", candidate, safePathErrorMessage(err))
	}

	resolved, err = resolveSymlinksInsideRoot(opts.Root, resolved, candidate)
	if err != nil {
		return expandedReference{}, total, false, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return expandedReference{}, total, false, fmt.Errorf("context: stat @%s: %s", candidate, safePathErrorMessage(err))
	}

	if seen[resolved] {
		return expandedReference{}, total, false, nil
	}

	seen[resolved] = true

	if info.IsDir() {
		remaining := opts.MaxTotalBytes - total
		if remaining <= 0 {
			return expandedReference{}, total, false, errors.New("context: max_total_bytes exceeded")
		}

		limit := min(opts.MaxFileBytes, remaining)

		content, truncated, treeErr := directoryTree(resolved, limit)
		if treeErr != nil {
			return expandedReference{}, total, false, fmt.Errorf("context: list @%s: %s", candidate, safePathErrorMessage(treeErr))
		}

		tokenEstimate, tokenEstimator := estimateReferenceContent(opts, content)

		return expandedReference{
			Path:           displayPath,
			Kind:           "directory",
			TokenEstimator: tokenEstimator,
			DigestSHA256:   digestHex(content),
			Bytes:          len(content),
			Truncated:      truncated,
			TokenEstimate:  tokenEstimate,
			content:        string(content),
		}, total + len(content), true, nil
	}

	if !info.Mode().IsRegular() {
		return expandedReference{}, total, false, fmt.Errorf("context: @%s is not a regular file", candidate)
	}

	return expandFileCandidate(candidate, resolved, displayPath, info, opts, total)
}

func expandFileCandidate(
	candidate string,
	resolved string,
	displayPath string,
	info fs.FileInfo,
	opts Options,
	total int,
) (ref expandedReference, nextTotal int, ok bool, err error) {
	if mediaType, ok := imageMediaTypeFromExtension(resolved); ok {
		ref, imageErr := readImageReference(resolved, displayPath, info.Size(), mediaType, opts)
		if imageErr != nil {
			return expandedReference{}, total, false, fmt.Errorf("context: read image @%s: %s", candidate, safePathErrorMessage(imageErr))
		}

		return ref, total, true, nil
	}

	remaining := opts.MaxTotalBytes - total
	if remaining <= 0 {
		return expandedReference{}, total, false, errors.New("context: max_total_bytes exceeded")
	}

	limit := min(opts.MaxFileBytes, remaining)

	content, truncated, err := readLimited(resolved, limit)
	if err != nil {
		return expandedReference{}, total, false, fmt.Errorf("context: read @%s: %s", candidate, safePathErrorMessage(err))
	}

	if isBinary(content) {
		return expandedReference{}, total, false, fmt.Errorf("context: @%s is a binary file", candidate)
	}

	tokenEstimate, tokenEstimator := estimateReferenceContent(opts, content)

	return expandedReference{
		Path:           displayPath,
		Kind:           kindFile,
		TokenEstimator: tokenEstimator,
		DigestSHA256:   digestHex(content),
		Bytes:          len(content),
		Truncated:      truncated,
		TokenEstimate:  tokenEstimate,
		content:        string(content),
	}, total + len(content), true, nil
}

func resolve(root, candidate string) (resolved, displayPath string, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("context: resolve root: %w", err)
	}

	path := candidate
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(candidate))
	}

	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("context: @%s escapes root", candidate)
	}

	return path, filepath.ToSlash(rel), nil
}

func resolveSymlinksInsideRoot(root, path, candidate string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("context: resolve root: %w", err)
	}

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("context: resolve root symlinks: %s", safePathErrorMessage(err))
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("context: resolve @%s symlinks: %s", candidate, safePathErrorMessage(err))
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("context: resolve @%s: %w", candidate, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("context: @%s escapes root", candidate)
	}

	return resolved, nil
}

func directoryTree(root string, limit int) (data []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, false, nil
	}

	var b strings.Builder

	entries := 0

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path == root {
			return nil
		}

		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}

		if entries >= maxDirectoryEntries {
			truncated = true
			return errDirectoryLimit
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("rel: %w", relErr)
		}

		line := filepath.ToSlash(rel)
		if entry.IsDir() {
			line += "/"
		}

		line += "\n"
		if b.Len()+len(line) > limit {
			truncated = true
			return errDirectoryLimit
		}

		b.WriteString(line)

		entries++

		return nil
	})
	if errors.Is(err, errDirectoryLimit) {
		err = nil
	}

	return []byte(b.String()), truncated, err
}

func readLimited(path string, limit int) (data []byte, truncated bool, err error) {
	if limit <= 0 {
		return nil, false, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open: %w", err)
	}
	defer file.Close()

	data, err = io.ReadAll(io.LimitReader(file, int64(limit+1)))
	if err != nil {
		return nil, false, fmt.Errorf("read: %w", err)
	}

	if len(data) > limit {
		return data[:limit], true, nil
	}

	return data, false, nil
}

func readImageReference(path, displayPath string, size int64, mediaType string, opts Options) (expandedReference, error) {
	limit := opts.MaxImageBytes
	if limit <= 0 {
		limit = DefaultMaxImageBytes
	}

	if size > int64(limit) {
		return expandedReference{}, fmt.Errorf("image exceeds max_image_bytes (%d > %d)", size, limit)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return expandedReference{}, fmt.Errorf("read image file: %w", err)
	}

	if len(data) > limit {
		return expandedReference{}, fmt.Errorf("image exceeds max_image_bytes (%d > %d)", len(data), limit)
	}

	sniffedMediaType := imageMediaTypeFromData(data)
	if sniffedMediaType == "" {
		return expandedReference{}, fmt.Errorf("unsupported image data for media type %q", mediaType)
	}

	mediaType = sniffedMediaType

	return expandedReference{
		Path:            displayPath,
		Kind:            kindImage,
		MediaType:       mediaType,
		ImageDataBase64: base64.StdEncoding.EncodeToString(data),
		DigestSHA256:    digestHex(data),
		Bytes:           len(data),
	}, nil
}

func imageMediaTypeFromExtension(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	}

	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mediaType == "" {
		return "", false
	}

	mediaType, _, _ = strings.Cut(mediaType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	return mediaType, supportedImageMediaType(mediaType)
}

func imageMediaTypeFromData(data []byte) string {
	mediaType := http.DetectContentType(data)
	mediaType, _, _ = strings.Cut(mediaType, ";")

	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if supportedImageMediaType(mediaType) {
		return mediaType
	}

	return ""
}

func supportedImageMediaType(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func safePathErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Op + ": " + linkErr.Err.Error()
	}

	return err.Error()
}

func appendReferences(prompt string, refs []expandedReference) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n<context_references>\n")

	for i := range refs {
		ref := &refs[i]
		tag := ref.Kind

		if tag == "" {
			tag = kindFile
		}

		b.WriteString(`<`)
		b.WriteString(tag)
		b.WriteString(` path="`)
		b.WriteString(escapeAttr(ref.Path))
		b.WriteString(`" truncated="`)
		b.WriteString(strconv.FormatBool(ref.Truncated))
		b.WriteString(`" bytes="`)
		b.WriteString(strconv.Itoa(ref.Bytes))

		if ref.MediaType != "" {
			b.WriteString(`" media_type="`)
			b.WriteString(escapeAttr(ref.MediaType))
		}

		if ref.TokenEstimate.Tokens > 0 || ref.TokenEstimate.UpperBoundTokens > 0 {
			b.WriteString(`" estimated_tokens="`)
			b.WriteString(strconv.Itoa(ref.TokenEstimate.Tokens))
			b.WriteString(`" estimated_token_error_bound="`)
			b.WriteString(strconv.Itoa(ref.TokenEstimate.ErrorBoundTokens))
			b.WriteString(`" estimated_token_upper_bound="`)
			b.WriteString(strconv.Itoa(ref.TokenEstimate.UpperBoundTokens))
		}

		if ref.TokenEstimator != "" {
			b.WriteString(`" token_estimator="`)
			b.WriteString(escapeAttr(ref.TokenEstimator))
		}

		if ref.DigestSHA256 != "" {
			b.WriteString(`" digest_sha256="`)
			b.WriteString(escapeAttr(ref.DigestSHA256))
		}

		if ref.Kind == kindImage {
			b.WriteString(`" attached="true">`)
			b.WriteString("\n</")
			b.WriteString(tag)
			b.WriteString(">\n")

			continue
		}

		b.WriteString("\">\n")
		b.WriteString(escapeText(ref.content))

		if !strings.HasSuffix(ref.content, "\n") {
			b.WriteString("\n")
		}

		b.WriteString("</")
		b.WriteString(tag)
		b.WriteString(">\n")
	}

	b.WriteString("</context_references>")

	return b.String()
}

func escapeAttr(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, "\r", "&#13;")
	value = strings.ReplaceAll(value, "\n", "&#10;")
	value = strings.ReplaceAll(value, "\t", "&#9;")

	return value
}

func references(refs []expandedReference) []Reference {
	out := make([]Reference, 0, len(refs))
	for i := range refs {
		out = append(out, refs[i].Reference())
	}

	return out
}

func imageReferences(refs []expandedReference) []ImageReference {
	var out []ImageReference

	for i := range refs {
		image, ok := refs[i].ImageReference()
		if ok {
			out = append(out, image)
		}
	}

	return out
}

func inlineReferenceEvent(ref Reference) ReferenceEvent {
	decision := ReferenceDecisionLoaded
	reason := "inline reference resolved inside root"

	if ref.Truncated {
		decision = ReferenceDecisionTruncated
		reason = "byte limit reached"
	}

	return ReferenceEvent{
		Source:           ref.Path,
		Kind:             ref.Kind,
		MediaType:        ref.MediaType,
		Scope:            ReferenceScopeInline,
		Location:         referenceLocationLocal,
		TokenEstimator:   ref.TokenEstimator,
		DigestSHA256:     ref.DigestSHA256,
		Bytes:            ref.Bytes,
		Truncated:        ref.Truncated,
		PolicyDecision:   decision,
		PolicyReason:     reason,
		PolicyReasonCode: ReferenceReasonCode(decision, reason),
		TokenEstimate:    ref.TokenEstimate,
	}
}

func rejectedInlineReferenceEvent(candidate string, opts Options, err error) ReferenceEvent {
	opts.ReferenceScope = ReferenceScopeInline

	kind := kindFile
	if _, ok := imageMediaTypeFromExtension(candidate); ok {
		kind = kindImage
	}

	return newReferenceEvent(candidate, kind, referenceLocationLocal, opts, ReferenceDecisionRejected, err.Error())
}
