package cborenc_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/jerkeyray/starling/internal/cborenc"
)

type inner struct {
	Name  string `cbor:"name"`
	Count int    `cbor:"count"`
}

type wide struct {
	I64   int64            `cbor:"i64"`
	U64   uint64           `cbor:"u64"`
	Str   string           `cbor:"str"`
	Bytes []byte           `cbor:"bytes"`
	List  []int            `cbor:"list"`
	Map   map[string]int   `cbor:"map"`
	Sub   inner            `cbor:"sub"`
	Subs  []inner          `cbor:"subs"`
	Nest  map[string]inner `cbor:"nest"`
}

func sample() wide {
	return wide{
		I64:   -42,
		U64:   1 << 40,
		Str:   "starling",
		Bytes: []byte{0x01, 0x02, 0x03},
		List:  []int{3, 1, 4, 1, 5, 9, 2, 6},
		Map:   map[string]int{"b": 2, "a": 1, "aa": 11, "": 0},
		Sub:   inner{Name: "x", Count: 7},
		Subs:  []inner{{Name: "y", Count: 8}, {Name: "z", Count: 9}},
		Nest: map[string]inner{
			"second": {Name: "s", Count: 2},
			"first":  {Name: "f", Count: 1},
		},
	}
}

func TestMarshal_Deterministic(t *testing.T) {
	v := sample()
	first, err := cborenc.Marshal(v)
	if err != nil {
		t.Fatalf("first marshal: %v", err)
	}
	for i := 0; i < 1000; i++ {
		got, err := cborenc.Marshal(v)
		if err != nil {
			t.Fatalf("marshal iter %d: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("non-deterministic CBOR on iter %d:\nfirst=% x\ngot  =% x", i, first, got)
		}
	}
}

func TestMarshal_MapKeysSortedBytewise(t *testing.T) {
	// In Core Deterministic mode, map keys are sorted by their CBOR-encoded
	// bytewise lexical order. For short text strings the encoded form is
	// (0x60+len) || utf8_bytes, so "" < "a" < "aa" < "b" (shorter prefixes
	// come before longer ones because their length byte is smaller, and for
	// equal-length keys the utf8 bytes decide).
	//
	// Marshal two maps whose Go iteration order differs but whose canonical
	// encoding must be identical.
	a := map[string]int{"": 0, "a": 1, "aa": 11, "b": 2}
	b := map[string]int{"b": 2, "aa": 11, "": 0, "a": 1}

	ab, err := cborenc.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	bb, err := cborenc.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatalf("maps with identical contents encoded differently:\na=% x\nb=% x", ab, bb)
	}

	// Sanity check: "a"'s single-byte key 0x61 must appear before "b"'s 0x62
	// in the encoded output.
	idxA := bytes.IndexByte(ab, 0x61)
	idxB := bytes.IndexByte(ab, 0x62)
	if idxA < 0 || idxB < 0 || idxA >= idxB {
		t.Fatalf("expected 'a' before 'b' in canonical encoding: idxA=%d idxB=%d bytes=% x", idxA, idxB, ab)
	}
}

func TestRoundTrip(t *testing.T) {
	want := sample()
	data, err := cborenc.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wide
	if err := cborenc.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round-trip mismatch:\nwant=%+v\ngot =%+v", want, got)
	}
}

func TestUnmarshal_RejectsDuplicateKeys(t *testing.T) {
	// A CBOR map of length 2 with the same text key "a" twice:
	//   0xa2                map(2)
	//   0x61 0x61 0x01      "a" => 1
	//   0x61 0x61 0x02      "a" => 2
	data := []byte{0xa2, 0x61, 0x61, 0x01, 0x61, 0x61, 0x02}
	var m map[string]int
	if err := cborenc.Unmarshal(data, &m); err == nil {
		t.Fatalf("expected error on duplicate map keys, got decoded value %v", m)
	}
}

func TestUnmarshal_RejectsIndefLength(t *testing.T) {
	// Indefinite-length array containing a single 1, terminated by 0xff:
	//   0x9f 0x01 0xff
	data := []byte{0x9f, 0x01, 0xff}
	var s []int
	if err := cborenc.Unmarshal(data, &s); err == nil {
		t.Fatalf("expected error on indefinite-length array, got decoded value %v", s)
	}
}
