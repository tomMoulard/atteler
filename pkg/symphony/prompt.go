//nolint:cyclop,gocognit,gocritic,wsl_v5,modernize,intrange // The prompt renderer is a small strict parser kept dependency-free.
package symphony

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// RenderPrompt renders the workflow prompt body with strict Liquid-like
// variable lookup. It supports interpolation, if/else, and for/endfor tags.
func RenderPrompt(template string, issue Issue, attempt *int) (string, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		template = defaultPromptWhenEmpty
	}

	data := map[string]any{
		"issue":   issueTemplateMap(issue),
		"attempt": attemptValue(attempt),
	}

	rendered, err := renderLiquidSection(template, data)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(rendered), nil
}

func attemptValue(attempt *int) any {
	if attempt == nil {
		return nil
	}

	return *attempt
}

func issueTemplateMap(issue Issue) map[string]any {
	return map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": pointerValue(issue.Description),
		"priority":    pointerValue(issue.Priority),
		"state":       issue.State,
		"branch_name": pointerValue(issue.BranchName),
		"url":         pointerValue(issue.URL),
		"labels":      issue.Labels,
		"blocked_by":  blockerTemplateMaps(issue.BlockedBy),
		"comments":    issueCommentTemplateMaps(issue.Comments),
		"created_at":  timePointerValue(issue.CreatedAt),
		"updated_at":  timePointerValue(issue.UpdatedAt),
	}
}

func issueCommentTemplateMaps(comments []IssueComment) []map[string]any {
	out := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		out = append(out, map[string]any{
			"author":             comment.Author,
			"author_association": comment.AuthorAssociation,
			"body":               comment.Body,
			"url":                pointerValue(comment.URL),
			"created_at":         timePointerValue(comment.CreatedAt),
			"updated_at":         timePointerValue(comment.UpdatedAt),
		})
	}

	return out
}

func blockerTemplateMaps(blockers []BlockerRef) []map[string]any {
	out := make([]map[string]any, 0, len(blockers))
	for _, blocker := range blockers {
		out = append(out, map[string]any{
			"id":         pointerValue(blocker.ID),
			"identifier": pointerValue(blocker.Identifier),
			"state":      pointerValue(blocker.State),
		})
	}

	return out
}

func pointerValue[T any](value *T) any {
	if value == nil {
		return nil
	}

	return *value
}

func timePointerValue(value *time.Time) any {
	if value == nil {
		return nil
	}

	return value.UTC().Format(time.RFC3339)
}

func renderLiquidSection(template string, scope map[string]any) (string, error) {
	var out strings.Builder
	for len(template) > 0 {
		exprIndex := strings.Index(template, "{{")
		tagIndex := strings.Index(template, "{%")
		nextIndex := minPositiveIndex(exprIndex, tagIndex)
		if nextIndex < 0 {
			out.WriteString(template)
			break
		}

		out.WriteString(template[:nextIndex])
		template = template[nextIndex:]

		switch {
		case strings.HasPrefix(template, "{{"):
			closeIndex := strings.Index(template, "}}")
			if closeIndex < 0 {
				return "", &ClassedError{Class: ErrTemplateParse, Err: errors.New("unclosed interpolation")}
			}

			expr := strings.TrimSpace(template[2:closeIndex])
			value, err := evalExpression(expr, scope)
			if err != nil {
				return "", err
			}

			out.WriteString(renderValue(value))
			template = template[closeIndex+2:]

		case strings.HasPrefix(template, "{%"):
			tag, rest, err := readTag(template)
			if err != nil {
				return "", err
			}

			switch {
			case strings.HasPrefix(tag, "for "):
				rendered, remaining, err := renderFor(tag, rest, scope)
				if err != nil {
					return "", err
				}

				out.WriteString(rendered)
				template = remaining
			case strings.HasPrefix(tag, "if "):
				rendered, remaining, err := renderIf(tag, rest, scope)
				if err != nil {
					return "", err
				}

				out.WriteString(rendered)
				template = remaining
			case tag == "else" || tag == "endif" || tag == "endfor":
				return "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("unexpected tag %q", tag)}
			default:
				return "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("unknown tag %q", tag)}
			}
		}
	}

	return out.String(), nil
}

func minPositiveIndex(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func readTag(template string) (tag string, rest string, err error) {
	closeIndex := strings.Index(template, "%}")
	if closeIndex < 0 {
		return "", "", &ClassedError{Class: ErrTemplateParse, Err: errors.New("unclosed tag")}
	}

	return strings.TrimSpace(template[2:closeIndex]), template[closeIndex+2:], nil
}

func renderFor(tag, rest string, scope map[string]any) (string, string, error) {
	parts := strings.Fields(tag)
	if len(parts) != 4 || parts[0] != "for" || parts[2] != "in" {
		return "", "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("invalid for tag %q", tag)}
	}

	varName := parts[1]
	if !validIdentifier(varName) {
		return "", "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("invalid loop variable %q", varName)}
	}

	value, err := evalExpression(parts[3], scope)
	if err != nil {
		return "", "", err
	}

	body, remaining, err := splitBlock(rest, "for")
	if err != nil {
		return "", "", err
	}

	values, err := iterableValues(value)
	if err != nil {
		return "", "", err
	}

	var out strings.Builder
	for _, item := range values {
		child := cloneScope(scope)
		child[varName] = item

		rendered, err := renderLiquidSection(body, child)
		if err != nil {
			return "", "", err
		}

		out.WriteString(rendered)
	}

	return out.String(), remaining, nil
}

func renderIf(tag, rest string, scope map[string]any) (string, string, error) {
	expr := strings.TrimSpace(strings.TrimPrefix(tag, "if "))
	if expr == "" {
		return "", "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("invalid if tag %q", tag)}
	}

	value, err := evalExpression(expr, scope)
	if err != nil {
		return "", "", err
	}

	body, elseBody, remaining, err := splitIfBlock(rest)
	if err != nil {
		return "", "", err
	}

	selected := body
	if !truthy(value) {
		selected = elseBody
	}

	rendered, err := renderLiquidSection(selected, scope)
	if err != nil {
		return "", "", err
	}

	return rendered, remaining, nil
}

func splitBlock(template, blockName string) (body string, remaining string, err error) {
	endTag := "end" + blockName
	depth := 1
	cursor := 0

	for {
		next := strings.Index(template[cursor:], "{%")
		if next < 0 {
			return "", "", &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("missing %s tag", endTag)}
		}

		start := cursor + next
		tag, rest, err := readTag(template[start:])
		if err != nil {
			return "", "", err
		}

		switch {
		case strings.HasPrefix(tag, blockName+" "):
			depth++
		case tag == endTag:
			depth--
			if depth == 0 {
				end := start + len(template[start:]) - len(rest)
				return template[:start], template[end:], nil
			}
		}

		cursor = start + len(template[start:]) - len(rest)
	}
}

func splitIfBlock(template string) (body string, elseBody string, remaining string, err error) {
	depth := 1
	cursor := 0
	elseStart := -1
	elseEnd := -1

	for {
		next := strings.Index(template[cursor:], "{%")
		if next < 0 {
			return "", "", "", &ClassedError{Class: ErrTemplateParse, Err: errors.New("missing endif tag")}
		}

		start := cursor + next
		tag, rest, err := readTag(template[start:])
		if err != nil {
			return "", "", "", err
		}

		tagEnd := start + len(template[start:]) - len(rest)
		switch {
		case strings.HasPrefix(tag, "if "):
			depth++
		case tag == "endif":
			depth--
			if depth == 0 {
				if elseStart >= 0 {
					return template[:elseStart], template[elseEnd:start], template[tagEnd:], nil
				}

				return template[:start], "", template[tagEnd:], nil
			}
		case tag == "else" && depth == 1:
			elseStart = start
			elseEnd = tagEnd
		}

		cursor = tagEnd
	}
}

func evalExpression(expr string, scope map[string]any) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, &ClassedError{Class: ErrTemplateParse, Err: errors.New("empty expression")}
	}

	if strings.Contains(expr, "|") {
		return nil, &ClassedError{Class: ErrTemplateRender, Err: fmt.Errorf("unknown filter in %q", expr)}
	}

	if quoted, ok := quotedString(expr); ok {
		return quoted, nil
	}

	if parsed, err := strconv.Atoi(expr); err == nil {
		return parsed, nil
	}

	parts := strings.Split(expr, ".")
	root := parts[0]
	if !validIdentifier(root) {
		return nil, &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("invalid expression %q", expr)}
	}

	value, ok := scope[root]
	if !ok {
		return nil, &ClassedError{Class: ErrTemplateRender, Err: fmt.Errorf("unknown variable %q", root)}
	}

	for _, part := range parts[1:] {
		if !validIdentifier(part) {
			return nil, &ClassedError{Class: ErrTemplateParse, Err: fmt.Errorf("invalid path %q", expr)}
		}

		next, ok := lookupField(value, part)
		if !ok {
			return nil, &ClassedError{Class: ErrTemplateRender, Err: fmt.Errorf("unknown variable %q", expr)}
		}

		value = next
	}

	return value, nil
}

func quotedString(expr string) (string, bool) {
	if len(expr) < 2 {
		return "", false
	}

	if (expr[0] == '"' && expr[len(expr)-1] == '"') || (expr[0] == '\'' && expr[len(expr)-1] == '\'') {
		unquoted, err := strconv.Unquote(expr)
		if err != nil && expr[0] == '\'' {
			return expr[1 : len(expr)-1], true
		}

		return unquoted, err == nil
	}

	return "", false
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}

	for i, r := range value {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}

		return false
	}

	return true
}

func lookupField(value any, field string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		next, ok := typed[field]
		return next, ok
	case map[string]string:
		next, ok := typed[field]
		return next, ok
	default:
		rv := reflect.ValueOf(value)
		if !rv.IsValid() {
			return nil, false
		}

		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return nil, false
			}

			rv = rv.Elem()
		}

		if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
			next := rv.MapIndex(reflect.ValueOf(field))
			if !next.IsValid() {
				return nil, false
			}

			return next.Interface(), true
		}
	}

	return nil, false
}

func iterableValues(value any) ([]any, error) {
	if value == nil {
		return nil, &ClassedError{Class: ErrTemplateRender, Err: errors.New("cannot iterate null")}
	}

	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, &ClassedError{Class: ErrTemplateRender, Err: errors.New("cannot iterate null")}
		}

		rv = rv.Elem()
	}

	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, &ClassedError{Class: ErrTemplateRender, Err: fmt.Errorf("cannot iterate %T", value)}
	}

	out := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out = append(out, rv.Index(i).Interface())
	}

	return out, nil
}

func cloneScope(scope map[string]any) map[string]any {
	out := make(map[string]any, len(scope)+1)
	for key, value := range scope {
		out[key] = value
	}

	return out
}

func renderValue(value any) string {
	if value == nil {
		return ""
	}

	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, bool, float32, float64:
		return fmt.Sprint(typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}

		return string(data)
	}
}

func truthy(value any) bool {
	if value == nil {
		return false
	}

	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed != ""
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.String:
		return rv.Len() > 0
	case reflect.Pointer, reflect.Interface:
		return !rv.IsNil()
	default:
		return true
	}
}
