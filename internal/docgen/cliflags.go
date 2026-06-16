package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// flagDoc is one extracted CLI option.
type flagDoc struct {
	Name    string
	Type    string
	Default string
	Usage   string
}

// cliFlagsFile is the single file where every CLI flag is registered.
var cliFlagsFile = filepath.Join("cmd", "atteler", "cli_flags.go")

// renderCLIOptions parses the flag-registration file and renders a section per
// CLI option (description, type, default, example), sorted by flag name. It is
// sourced from the actual flag.FlagSet registrations so it cannot drift.
func renderCLIOptions(root string) (string, error) {
	path := filepath.Join(root, cliFlagsFile)

	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", cliFlagsFile, err)
	}

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", cliFlagsFile, err)
	}

	flags := collectFlagDocs(fset, src, file)
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })

	return formatCLIOptions(flags), nil
}

// flagKinds maps the FlagSet registration method to a documented type and
// whether a default-value argument precedes the usage string.
var flagKinds = map[string]struct {
	typ        string
	hasDefault bool
}{
	"BoolVar":     {"bool", true},
	"StringVar":   {"string", true},
	"IntVar":      {"int", true},
	"Int64Var":    {"int", true},
	"Float64Var":  {"float", true},
	"DurationVar": {"duration", true},
	"Var":         {"value", false},
}

func collectFlagDocs(fset *token.FileSet, src []byte, file *ast.File) []flagDoc {
	var flags []flagDoc

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "fs" {
			return true
		}

		kind, known := flagKinds[sel.Sel.Name]
		if !known {
			return true
		}

		if doc, ok := flagDocFromCall(fset, src, call, kind.typ, kind.hasDefault); ok {
			flags = append(flags, doc)
		}

		return true
	})

	return flags
}

func flagDocFromCall(fset *token.FileSet, src []byte, call *ast.CallExpr, typ string, hasDefault bool) (flagDoc, bool) {
	// Layout: (ptr, name, [default,] usage).
	want := 3
	if hasDefault {
		want = 4
	}

	if len(call.Args) < want {
		return flagDoc{}, false
	}

	name, ok := stringLit(call.Args[1])
	if !ok {
		return flagDoc{}, false
	}

	usageArg := call.Args[2]
	def := ""

	if hasDefault {
		def = defaultText(fset, src, call.Args[2])
		usageArg = call.Args[3]
	}

	usage, ok := stringLit(usageArg)
	if !ok {
		return flagDoc{}, false
	}

	return flagDoc{Name: name, Type: typ, Default: def, Usage: usage}, true
}

func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}

	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}

	return value, true
}

// defaultText renders the default-value argument: empty/whitespace strings show
// as `""`, other string literals show their content, and everything else (bools,
// numbers, named constants) shows its verbatim source text.
func defaultText(fset *token.FileSet, src []byte, expr ast.Expr) string {
	if value, ok := stringLit(expr); ok {
		if strings.TrimSpace(value) == "" {
			return `""`
		}

		return value
	}

	start := fset.Position(expr.Pos()).Offset
	end := fset.Position(expr.End()).Offset

	if start < 0 || end > len(src) || start >= end {
		return ""
	}

	return strings.TrimSpace(string(src[start:end]))
}

func formatCLIOptions(flags []flagDoc) string {
	var out bytes.Buffer

	fmt.Fprintf(&out, "%s\n\n", generatedHeader)
	fmt.Fprintf(&out, "Every CLI option, one section each. There are %d options. Flags combine with\n", len(flags))
	fmt.Fprintln(&out, "the grouped domain commands (before or after the subcommand). Boolean flags")
	fmt.Fprintln(&out, "default to off unless noted; omitted values are not sent to providers.")
	fmt.Fprintln(&out)

	for i := range flags {
		f := &flags[i]

		fmt.Fprintf(&out, "### `--%s`\n\n", f.Name)
		fmt.Fprintf(&out, "%s\n\n", capitalizeFirst(f.Usage))
		fmt.Fprintf(&out, "- **Type:** %s\n", f.Type)

		if f.Default != "" {
			fmt.Fprintf(&out, "- **Default:** `%s`\n", f.Default)
		}

		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "```sh")
		fmt.Fprintf(&out, "atteler %s\n", flagExample(f))
		fmt.Fprintln(&out, "```")
		fmt.Fprintln(&out)
	}

	return out.String()
}

func flagExample(f *flagDoc) string {
	switch f.Type {
	case "bool":
		return "--" + f.Name
	case "int", "float":
		return "--" + f.Name + " <n>"
	case "duration":
		return "--" + f.Name + " 30m"
	default:
		return "--" + f.Name + " <value>"
	}
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}

	// Flag usage strings are ASCII, so byte-indexing the first rune is safe.
	return strings.ToUpper(s[:1]) + s[1:]
}
