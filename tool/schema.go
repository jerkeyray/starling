package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// generateSchema returns a minified JSON Schema document for t. The output
// is deterministic — fields are emitted in alphabetical order and required
// lists are sorted — so identical Go types produce byte-identical schemas.
//
// The generator supports a narrow subset suited to agent tool inputs:
// primitives (string/bool/int*/uint*/float*), structs, slices/arrays,
// pointers to any of the above, and nested structs. Unsupported kinds
// (maps, interfaces, channels, functions, time types, recursive structs)
// cause a panic — this is programmer error, not runtime error.
func generateSchema(t reflect.Type) json.RawMessage {
	node := buildNode(t, map[reflect.Type]bool{})
	b, err := marshalNode(node)
	if err != nil {
		panic(fmt.Sprintf("tool: marshal schema: %v", err))
	}
	return json.RawMessage(b)
}

// schemaNode is an ordered, minimal representation of a JSON Schema node.
// We build it by hand (rather than using map[string]any) so that we can
// emit keys in a fixed, deterministic order.
type schemaNode struct {
	typeName    string
	description string
	enum        []string
	items       *schemaNode
	properties  []propEntry // alphabetical by name
	required    []string    // sorted
	additional  *bool       // additionalProperties=false for objects
}

type propEntry struct {
	name string
	node *schemaNode
}

func buildNode(t reflect.Type, seen map[reflect.Type]bool) *schemaNode {
	// Unwrap pointers: a pointer field has the same schema as its element.
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.String:
		return &schemaNode{typeName: "string"}
	case reflect.Bool:
		return &schemaNode{typeName: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &schemaNode{typeName: "integer"}
	case reflect.Float32, reflect.Float64:
		return &schemaNode{typeName: "number"}

	case reflect.Slice, reflect.Array:
		// []byte is common as base64 strings in JSON, but to keep behavior
		// predictable we always treat slices as arrays. Callers who need
		// base64 should use `string`.
		return &schemaNode{
			typeName: "array",
			items:    buildNode(t.Elem(), seen),
		}

	case reflect.Struct:
		if seen[t] {
			panic(fmt.Sprintf("tool: recursive struct not supported: %s", t))
		}
		seen[t] = true
		defer delete(seen, t)
		return buildStructNode(t, seen)

	case reflect.Map:
		panic(fmt.Sprintf("tool: map types not supported: %s (use a struct)", t))
	case reflect.Interface:
		panic(fmt.Sprintf("tool: interface / any types not supported: %s", t))
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		panic(fmt.Sprintf("tool: unsupported kind %s for type %s", t.Kind(), t))
	}

	panic(fmt.Sprintf("tool: unsupported type %s", t))
}

func buildStructNode(t reflect.Type, seen map[reflect.Type]bool) *schemaNode {
	falseVal := false
	n := &schemaNode{
		typeName:   "object",
		additional: &falseVal,
	}

	type builtField struct {
		name     string
		node     *schemaNode
		required bool
	}
	var fields []builtField

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		jsonTag := f.Tag.Get("json")

		// Anonymous struct embedding with no json tag: encoding/json
		// promotes the embedded struct's exported fields to the parent
		// (even when the embedded type itself is unexported). Mirror
		// that by merging its properties in.
		if f.Anonymous && jsonTag == "" {
			embedType := f.Type
			for embedType.Kind() == reflect.Pointer {
				embedType = embedType.Elem()
			}
			if embedType.Kind() == reflect.Struct {
				child := buildNode(embedType, seen)
				for _, p := range child.properties {
					childReq := false
					for _, r := range child.required {
						if r == p.name {
							childReq = true
							break
						}
					}
					fields = append(fields, builtField{name: p.name, node: p.node, required: childReq})
				}
				continue
			}
		}

		if !f.IsExported() {
			continue
		}

		jsonName, omit, skip := parseJSONTag(f)
		if skip {
			continue
		}

		child := buildNode(f.Type, seen)
		applyJSONSchemaTag(child, f.Tag.Get("jsonschema"))

		// Pointer fields are optional regardless of omitempty.
		isPointer := f.Type.Kind() == reflect.Pointer
		req := !omit && !isPointer

		fields = append(fields, builtField{name: jsonName, node: child, required: req})
	}

	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	for _, f := range fields {
		n.properties = append(n.properties, propEntry{name: f.name, node: f.node})
		if f.required {
			n.required = append(n.required, f.name)
		}
	}
	sort.Strings(n.required)

	return n
}

// parseJSONTag returns the effective field name, whether the field is
// omitempty, and whether the field should be skipped entirely (json:"-").
func parseJSONTag(f reflect.StructField) (name string, omit bool, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag == "" {
		return f.Name, false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omit = true
		}
	}
	return name, omit, false
}

// applyJSONSchemaTag parses a `jsonschema:"..."` tag and mutates the node
// in place. Supported keys:
//
//	description=<text>
//	enum=<value>              (repeatable; string enums only)
func applyJSONSchemaTag(n *schemaNode, tag string) {
	if tag == "" {
		return
	}
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key, val := part[:eq], part[eq+1:]
		switch key {
		case "description":
			n.description = val
		case "enum":
			n.enum = append(n.enum, val)
		}
	}
}

// marshalNode serializes a schemaNode to minified JSON with a stable key
// order: type, description, enum, items, properties, required,
// additionalProperties.
func marshalNode(n *schemaNode) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeNode(&buf, n); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeNode(buf *bytes.Buffer, n *schemaNode) error {
	buf.WriteByte('{')
	first := true

	emit := func(key string, write func() error) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		return write()
	}

	if n.typeName != "" {
		_ = emit("type", func() error {
			v, _ := json.Marshal(n.typeName)
			buf.Write(v)
			return nil
		})
	}
	if n.description != "" {
		_ = emit("description", func() error {
			v, _ := json.Marshal(n.description)
			buf.Write(v)
			return nil
		})
	}
	if len(n.enum) > 0 {
		_ = emit("enum", func() error {
			v, _ := json.Marshal(n.enum)
			buf.Write(v)
			return nil
		})
	}
	if n.items != nil {
		if err := emit("items", func() error { return writeNode(buf, n.items) }); err != nil {
			return err
		}
	}
	if n.typeName == "object" {
		if err := emit("properties", func() error {
			buf.WriteByte('{')
			for i, p := range n.properties {
				if i > 0 {
					buf.WriteByte(',')
				}
				k, _ := json.Marshal(p.name)
				buf.Write(k)
				buf.WriteByte(':')
				if err := writeNode(buf, p.node); err != nil {
					return err
				}
			}
			buf.WriteByte('}')
			return nil
		}); err != nil {
			return err
		}
		_ = emit("required", func() error {
			if n.required == nil {
				buf.WriteString("[]")
				return nil
			}
			v, _ := json.Marshal(n.required)
			buf.Write(v)
			return nil
		})
		if n.additional != nil {
			_ = emit("additionalProperties", func() error {
				if *n.additional {
					buf.WriteString("true")
				} else {
					buf.WriteString("false")
				}
				return nil
			})
		}
	}

	buf.WriteByte('}')
	return nil
}
