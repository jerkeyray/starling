package tool

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

type weatherInput struct {
	City  string `json:"city" jsonschema:"description=City name."`
	Units string `json:"units,omitempty" jsonschema:"enum=metric,enum=imperial"`
	Days  int    `json:"days"`
}

func TestSchema_Basic(t *testing.T) {
	raw := generateSchema(reflect.TypeOf(weatherInput{}))

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":  map[string]any{"type": "string", "description": "City name."},
			"days":  map[string]any{"type": "integer"},
			"units": map[string]any{"type": "string", "enum": []any{"metric", "imperial"}},
		},
		"required":             []any{"city", "days"},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestSchema_Deterministic(t *testing.T) {
	first := generateSchema(reflect.TypeOf(weatherInput{}))
	for i := 0; i < 100; i++ {
		again := generateSchema(reflect.TypeOf(weatherInput{}))
		if !bytes.Equal(first, again) {
			t.Fatalf("schema not deterministic at iter %d:\n %s\n vs\n %s", i, first, again)
		}
	}
}

type nestedInput struct {
	Outer string       `json:"outer"`
	Inner weatherInput `json:"inner"`
	Tags  []string     `json:"tags"`
}

func TestSchema_NestedStruct(t *testing.T) {
	raw := generateSchema(reflect.TypeOf(nestedInput{}))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := got["properties"].(map[string]any)
	inner := props["inner"].(map[string]any)
	if inner["type"] != "object" {
		t.Fatalf("inner.type = %v, want object", inner["type"])
	}
	tags := props["tags"].(map[string]any)
	if tags["type"] != "array" {
		t.Fatalf("tags.type = %v, want array", tags["type"])
	}
	items := tags["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("tags.items.type = %v, want string", items["type"])
	}
}

type pointerInput struct {
	Required string  `json:"required"`
	Optional *string `json:"optional"`
}

func TestSchema_PointerOptional(t *testing.T) {
	raw := generateSchema(reflect.TypeOf(pointerInput{}))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req := got["required"].([]any)
	if len(req) != 1 || req[0] != "required" {
		t.Fatalf("required = %v, want [required]", req)
	}
	// Optional pointer still appears in properties as string.
	props := got["properties"].(map[string]any)
	opt := props["optional"].(map[string]any)
	if opt["type"] != "string" {
		t.Fatalf("optional.type = %v, want string", opt["type"])
	}
}

func TestSchema_JSONDashSkipped(t *testing.T) {
	type skipInput struct {
		Keep string `json:"keep"`
		Skip string `json:"-"`
	}
	raw := generateSchema(reflect.TypeOf(skipInput{}))
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	props := got["properties"].(map[string]any)
	if _, present := props["Skip"]; present {
		t.Fatalf("json:\"-\" field appeared in schema: %v", props)
	}
	if _, present := props["keep"]; !present {
		t.Fatalf("kept field missing: %v", props)
	}
}

type embedBase struct {
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}
type embedChild struct {
	embedBase
	Extra string `json:"extra"`
}

func TestSchema_AnonymousEmbedPromotesFields(t *testing.T) {
	raw := generateSchema(reflect.TypeOf(embedChild{}))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := got["properties"].(map[string]any)
	for _, want := range []string{"name", "age", "extra"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("missing promoted field %q in %v", want, props)
		}
	}
	if _, ok := props["embedBase"]; ok {
		t.Fatalf("embedded struct type appeared as property: %v", props)
	}
	req := got["required"].([]any)
	// name (required), extra (required), age is omitempty.
	wantReq := map[string]bool{"name": true, "extra": true}
	if len(req) != len(wantReq) {
		t.Fatalf("required = %v, want exactly %v", req, wantReq)
	}
	for _, r := range req {
		if !wantReq[r.(string)] {
			t.Fatalf("unexpected required field %q", r)
		}
	}
}

func TestSchema_UnsupportedMapPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for map type")
		}
	}()
	generateSchema(reflect.TypeOf(map[string]any{}))
}

func TestSchema_UnsupportedInterfacePanics(t *testing.T) {
	type badInput struct {
		X any `json:"x"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for interface type")
		}
	}()
	generateSchema(reflect.TypeOf(badInput{}))
}
