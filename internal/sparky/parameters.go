package sparky

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// recipeOrdered is a JSON object that keeps key insertion order so pinned
// recipe files round-trip byte-identically apart from the edited fields.
type recipeOrdered struct {
	keys []string
	vals map[string]any
}

func newRecipeOrdered() *recipeOrdered {
	return &recipeOrdered{vals: map[string]any{}}
}

func (o *recipeOrdered) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	return recipeDecodeObject(dec, o)
}

func recipeDecodeObject(dec *json.Decoder, o *recipeOrdered) error {
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("expected an object key")
		}
		var val any
		if val, err = recipeDecodeValue(dec); err != nil {
			return err
		}
		if _, seen := o.vals[key]; !seen {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = val
	}
	_, err := dec.Token()
	return err
}

func recipeDecodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			child := newRecipeOrdered()
			if err := recipeDecodeObject(dec, child); err != nil {
				return nil, err
			}
			return child, nil
		case '[':
			var items []any
			for dec.More() {
				item, err := recipeDecodeValue(dec)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return items, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	default:
		return tok, nil
	}
}

func recipeEncodeValue(buf *bytes.Buffer, val any, indent int) {
	pad := strings.Repeat("  ", indent)
	inner := strings.Repeat("  ", indent+1)
	switch t := val.(type) {
	case *recipeOrdered:
		buf.WriteString("{\n")
		for i, key := range t.keys {
			buf.WriteString(inner)
			kb, _ := json.Marshal(key)
			buf.Write(kb)
			buf.WriteString(": ")
			recipeEncodeValue(buf, t.vals[key], indent+1)
			if i < len(t.keys)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(pad)
		buf.WriteString("}")
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, item := range t {
			buf.WriteString(inner)
			recipeEncodeValue(buf, item, indent+1)
			if i < len(t)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(pad)
		buf.WriteString("]")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			b = []byte("null")
		}
		buf.Write(b)
	}
}

func (o *recipeOrdered) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	recipeEncodeValue(&buf, o, 0)
	return buf.Bytes(), nil
}

func recipeGet(o Object, key string) any { return value(o, key, nil) }

const recipeNumericPattern = `[0-9]+(?:\.[0-9]+)?`

// recipeAtomicWrite is a seam so tests can force write failures.
var recipeAtomicWrite = atomicWrite

// recipePlainNumber reports whether v is a JSON number (bools and strings are
// rejected, matching the reference behavior for env-style recipes).
func recipePlainNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64, json.Number:
		return true
	}
	return false
}

func recipeNumeric(v any) (string, error) {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case json.Number:
		if _, err := x.Float64(); err != nil {
			return "", fail("Recipe parameters must be numeric.")
		}
		return x.String(), nil
	case string:
		return x, nil
	}
	return "", fail("Recipe parameters must be numeric.")
}

// recipeReplaceFlag mirrors replace_flag: it rewrites a numeric value next to
// a flag inside a command line or an argv/keyword/override snippet.
func recipeReplaceFlag(text, flag string, value any, style string) (string, int, error) {
	rendered, err := recipeNumeric(value)
	if err != nil {
		return "", 0, err
	}
	quoted := regexp.QuoteMeta(flag)
	if !regexp.MustCompile(`^` + recipeNumericPattern + `$`).MatchString(rendered) {
		return "", 0, fail("Recipe parameters must be numeric.")
	}
	if style == "command" {
		// RE2 has no lookahead, so consume the trailing separator and put it back.
		pattern := regexp.MustCompile(`(` + quoted + `\s+)(` + recipeNumericPattern + `)(\s|\z)`)
		count := len(pattern.FindAllStringIndex(text, -1))
		out := pattern.ReplaceAllString(text, "${1}"+rendered+"${3}")
		return out, count, nil
	}
	pattern := regexp.MustCompile(`(["']` + quoted + `["']\s*[, :]\s*["'])(` + recipeNumericPattern + `)(["'])`)
	count := len(pattern.FindAllStringIndex(text, -1))
	out := pattern.ReplaceAllString(text, "${1}"+rendered+"${3}")
	override := regexp.MustCompile(`(args\[args\.index\(["']` + quoted + `["']\)\s*\+\s*1\]\s*(?:=|==)\s*["'])(` + recipeNumericPattern + `)(["'])`)
	overrides := len(override.FindAllStringIndex(out, -1))
	out = override.ReplaceAllString(out, "${1}"+rendered+"${3}")
	return out, count + overrides, nil
}

func recipeSortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func recipeBindingFields(binding Object) (map[string]string, error) {
	raw := recipeGet(binding, "fields")
	if fields, ok := raw.(Object); ok && fields != nil {
		out := map[string]string{}
		for field, flag := range fields {
			out[field] = str(flag)
		}
		return out, nil
	}
	return nil, fail("Recipe binding requires a fields mapping.")
}

func RenderParameters(spec Object) (Object, error) {
	parameters := obj(recipeGet(spec, "parameters"))
	bindings, _ := recipeGet(spec, "bindings").([]any)
	type output struct {
		path     string
		original []byte
		data     []byte
	}
	var outputs []output
	matched := map[string]bool{}
	paths := map[string]bool{}

	for _, raw := range bindings {
		binding, ok := raw.(map[string]any)
		if !ok {
			return nil, fail("Recipe binding requires a fields mapping.")
		}
		path := expand(str(recipeGet(binding, "path")))
		if !filepath.IsAbs(path) {
			return nil, fail("Recipe binding path must be absolute.")
		}
		if paths[path] {
			return nil, fail("A recipe file must have one binding with all its fields.")
		}
		paths[path] = true
		original, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text := string(original)
		style := str(recipeGet(binding, "format"))
		fields, err := recipeBindingFields(binding)
		if err != nil {
			return nil, err
		}
		bindingMatched := map[string]bool{}

		switch style {
		case "nvidia_json":
			document := newRecipeOrdered()
			if err := json.Unmarshal(original, document); err != nil {
				return nil, fail("Recipe file is not valid JSON: %s", path)
			}
			serve, _ := document.vals["serve"].(*recipeOrdered)
			if serve == nil {
				return nil, fail("Recipe file is missing the serve command: %s", path)
			}
			var commands []*recipeOrdered
			commands = append(commands, serve)
			if extra, ok := serve.vals["additional_commands"].([]any); ok {
				for _, item := range extra {
					if command, ok := item.(*recipeOrdered); ok {
						commands = append(commands, command)
					}
				}
			}
			for _, field := range recipeSortedKeys(fields) {
				flag := fields[field]
				for _, command := range commands {
					line, _ := command.vals["command"].(string)
					updated, count, err := recipeReplaceFlag(line, flag, parameters[field], "command")
					if err != nil {
						return nil, err
					}
					if count > 0 {
						command.vals["command"] = updated
						bindingMatched[field] = true
					}
				}
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				return nil, err
			}
			text = string(encoded) + "\n"
		case "env", "docker_env":
			for _, field := range recipeSortedKeys(fields) {
				key := fields[field]
				v := parameters[field]
				rendered, err := recipeNumeric(v)
				if err != nil || !recipePlainNumber(v) || !regexp.MustCompile(`^`+recipeNumericPattern+`$`).MatchString(rendered) {
					return nil, fail("Recipe values must be numeric.")
				}
				var pattern *regexp.Regexp
				if style == "env" {
					pattern = regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(key) + `=)(` + recipeNumericPattern + `)[ \t]*$`)
				} else {
					pattern = regexp.MustCompile(`(\b` + regexp.QuoteMeta(key) + `=)(` + recipeNumericPattern + `)`)
				}
				count := len(pattern.FindAllStringIndex(text, -1))
				if count > 0 {
					text = pattern.ReplaceAllString(text, "${1}"+rendered)
					bindingMatched[field] = true
				}
			}
		case "python", "command":
			for _, field := range recipeSortedKeys(fields) {
				updated, count, err := recipeReplaceFlag(text, fields[field], parameters[field], style)
				if err != nil {
					return nil, err
				}
				if count > 0 {
					text = updated
					bindingMatched[field] = true
				}
			}
		default:
			return nil, fail("Unknown recipe binding format.")
		}

		var missing []string
		for field := range fields {
			if !bindingMatched[field] {
				missing = append(missing, field)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			return nil, fail("Recipe fields did not match %s: %s", path, strings.Join(missing, ", "))
		}
		for field := range bindingMatched {
			matched[field] = true
		}
		outputs = append(outputs, output{path: path, original: original, data: []byte(text)})
	}

	if len(parameters) > 0 {
		var unmatched []string
		for field := range parameters {
			if !matched[field] {
				unmatched = append(unmatched, field)
			}
		}
		sort.Strings(unmatched)
		if len(unmatched) > 0 {
			return nil, fail("Some settings do not match the installed recipe: %s", strings.Join(unmatched, ", "))
		}
	}

	var changed []string
	type written struct {
		path     string
		original []byte
		mode     os.FileMode
	}
	var journal []written
	for _, out := range outputs {
		if bytes.Equal(out.original, out.data) {
			continue
		}
		info, err := os.Stat(out.path)
		if err != nil {
			return nil, err
		}
		mode := info.Mode().Perm()
		backup := out.path + ".before-sparky"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := recipeAtomicWrite(backup, out.original, 0600); err != nil {
				return nil, err
			}
		}
		journal = append(journal, written{out.path, out.original, mode})
		if err := recipeAtomicWrite(out.path, out.data, mode); err != nil {
			var failed []string
			for i := len(journal) - 1; i >= 0; i-- {
				if err := recipeAtomicWrite(journal[i].path, journal[i].original, journal[i].mode); err != nil {
					failed = append(failed, journal[i].path)
				}
			}
			if len(failed) > 0 {
				return nil, fail("Recipe rollback failed for: %s", strings.Join(failed, ", "))
			}
			return nil, err
		}
		changed = append(changed, out.path)
	}

	var fields []string
	for field := range matched {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return Object{"changed_files": anyStrings(changed), "parameter_fields": anyStrings(fields)}, nil
}
