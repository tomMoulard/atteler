package config

import (
	"fmt"
	"strings"
)

// DiagnosticSeverity identifies how urgently a configuration diagnostic
// should be treated.
type DiagnosticSeverity string

const (
	// DiagnosticWarning reports import data that was ignored or bounded
	// without making the merged atteler config invalid.
	DiagnosticWarning DiagnosticSeverity = "warning"
)

// Diagnostic reports non-fatal configuration import behavior, such as a
// harness field that atteler does not import or a malformed best-effort input.
type Diagnostic struct {
	Severity DiagnosticSeverity `json:"severity" yaml:"severity"`
	Importer string             `json:"importer,omitempty" yaml:"importer,omitempty"`
	Source   string             `json:"source,omitempty" yaml:"source,omitempty"`
	Path     string             `json:"path,omitempty" yaml:"path,omitempty"`
	Message  string             `json:"message" yaml:"message"`
}

// String renders a compact human-readable diagnostic for CLI output.
func (d Diagnostic) String() string {
	parts := make([]string, 0, 3)
	if d.Importer != "" {
		parts = append(parts, d.Importer)
	}

	if d.Source != "" {
		source := d.Source
		if d.Path != "" {
			source += " " + d.Path
		}

		parts = append(parts, source)
	} else if d.Path != "" {
		parts = append(parts, d.Path)
	}

	prefix := strings.Join(parts, ": ")
	if prefix == "" {
		return d.Message
	}

	return fmt.Sprintf("%s: %s", prefix, d.Message)
}

type diagnosticCollector struct {
	importer    string
	source      string
	diagnostics []Diagnostic
}

func newDiagnosticCollector(importer, source string) *diagnosticCollector {
	return &diagnosticCollector{
		importer: strings.TrimSpace(importer),
		source:   strings.TrimSpace(source),
	}
}

func (c *diagnosticCollector) warnf(path, format string, args ...any) {
	if c == nil {
		return
	}

	c.diagnostics = append(c.diagnostics, Diagnostic{
		Severity: DiagnosticWarning,
		Importer: c.importer,
		Source:   c.source,
		Path:     strings.TrimSpace(path),
		Message:  fmt.Sprintf(format, args...),
	})
}

func (c *diagnosticCollector) all() []Diagnostic {
	if c == nil || len(c.diagnostics) == 0 {
		return nil
	}

	return append([]Diagnostic(nil), c.diagnostics...)
}
