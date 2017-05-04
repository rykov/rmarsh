package rmarsh_test

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/samcday/rmarsh"
)

func testRubyDecode(t *testing.T, val interface{}, expected string) {
	b, err := rmarsh.Encode(val)
	if err != nil {
		t.Fatalf("Encode() failed: %s", err)
	}

	result := rbDecode(t, b)

	if result != expected {
		t.Errorf("Encoded %v (%T), Ruby saw %s, expected %q\nRaw encoded:\n%s\n", val, val, result, expected, hex.Dump(b))
	}
}

func TestEncodeNil(t *testing.T) {
	testRubyDecode(t, nil, "nil")
}

func TestEncodeBools(t *testing.T) {
	testRubyDecode(t, true, "true")
	testRubyDecode(t, false, "false")

	// Ptrs
	v := true
	testRubyDecode(t, &v, "true")
}

func TestEncodeSymbols(t *testing.T) {
	// Basic symbol test
	testRubyDecode(t, rmarsh.Symbol("test"), ":test")

	// Basic symlink test
	testRubyDecode(t, []rmarsh.Symbol{rmarsh.Symbol("test"), rmarsh.Symbol("test")}, "[:test, :test]")

	// Slightly more contrived symlink test
	testRubyDecode(t, []rmarsh.Symbol{
		rmarsh.Symbol("foo"),
		rmarsh.Symbol("bar"),
		rmarsh.Symbol("bar"),
		rmarsh.Symbol("foo"),
	}, "[:foo, :bar, :bar, :foo]")

	// Ptr test
	sym := rmarsh.Symbol("foo")
	testRubyDecode(t, &sym, ":foo")
}

func TestEncodeInts(t *testing.T) {
	testRubyDecode(t, 0, "0")
	testRubyDecode(t, 1, "1")
	testRubyDecode(t, 122, "122")
	testRubyDecode(t, 0xDE, "222")
	testRubyDecode(t, 0xDEAD, "57005")
	testRubyDecode(t, 0xDEADBE, "14593470")
	testRubyDecode(t, 0x3DEADBEE, "1038801902")

	testRubyDecode(t, -1, "-1")
	testRubyDecode(t, -123, "-123")
	testRubyDecode(t, -0xDE, "-222")
	testRubyDecode(t, -0xDEAD, "-57005")
	testRubyDecode(t, -0xDEADBE, "-14593470")
	testRubyDecode(t, -0x3DEADBEE, "-1038801902")

	// Ptrs
	v := 123
	testRubyDecode(t, &v, "123")
}

func TestEncodeBigNums(t *testing.T) {
	// Check that regular int64s larger than the fixnum encodable range become bignums.
	testRubyDecode(t, int64(0xDEADCAFEBEEF), "244838016401135")

	// Check that actual math.Big numbers encode properly too.
	var huge1, huge2 big.Int
	huge1.SetString("DEADCAFEBABEBEEFDEADCAFEBABEBEEF", 16)
	huge2.SetString("-DEADCAFEBABEBEEFDEADCAFEBABEBEEF", 16)

	testRubyDecode(t, huge1, "295990999649265874631894574770086133487")
	testRubyDecode(t, huge2, "-295990999649265874631894574770086133487")

	// And ptrs.
	v := int64(0xDEADCAFEBEEF)
	testRubyDecode(t, &v, "244838016401135")
	testRubyDecode(t, &huge1, "295990999649265874631894574770086133487")
}

func TestEncodeFloats(t *testing.T) {
	testRubyDecode(t, 123.33, "123.33")

	// Ptrs
	v := 123.33
	testRubyDecode(t, &v, "123.33")
}

func TestEncodeStrings(t *testing.T) {
	testRubyDecode(t, "hi", `"hi"`)

	// Ptrs
	v := "test"
	testRubyDecode(t, &v, `"test"`)
}

func TestEncodeClass(t *testing.T) {
	testRubyDecode(t, rmarsh.Class("Gem::Version"), "Class<Gem::Version>")

	// Ptrs
	v := rmarsh.Class("Gem::Version")
	testRubyDecode(t, &v, "Class<Gem::Version>")
}

func TestEncodeModule(t *testing.T) {
	testRubyDecode(t, rmarsh.Module("Gem"), "Module<Gem>")

	// Ptrs
	v := rmarsh.Module("Gem")
	testRubyDecode(t, &v, "Module<Gem>")
}

func TestEncodeSlices(t *testing.T) {
	testRubyDecode(t, []int{}, "[]")
	testRubyDecode(t, []int{123}, "[123]")
	testRubyDecode(t, []interface{}{123, true, nil, rmarsh.Symbol("test"), "test"}, `[123, true, nil, :test, "test"]`)

	// Ptrs
	v := []int{123}
	testRubyDecode(t, &v, "[123]")
}

func TestEncodeMap(t *testing.T) {
	testRubyDecode(t, map[string]int{"foo": 123, "bar": 321}, `{"bar"=>321, "foo"=>123}`)

	// Ptrs
	v := map[int]int{123: 321}
	testRubyDecode(t, &v, `{123=>321}`)
}

func TestEncodeInstance(t *testing.T) {
	inst := rmarsh.Instance{
		Name: "Object",
		InstanceVars: map[string]interface{}{
			"@test": 123,
		},
	}
	testRubyDecode(t, inst, "#Object<:@test=123>")

	// Checking object links
	testRubyDecode(t, []interface{}{&inst, &inst}, "[#Object<:@test=123>, #Object<:@test=123>]")

	testRubyDecode(t, rmarsh.Instance{
		Name:           "Gem::Version",
		UserMarshalled: true,
		Data:           []string{"1.2.3"},
	}, `#<Gem::Version "1.2.3">`)
}

func TestEncodeRegexp(t *testing.T) {
	testRubyDecode(t, rmarsh.Regexp{
		Expr:  "test",
		Flags: rmarsh.REGEXP_MULTILINE | rmarsh.REGEXP_IGNORECASE,
	}, `/test/mi`)

	// Ptrs
	v := rmarsh.Regexp{Expr: "test"}
	testRubyDecode(t, &v, "/test/")
}

type encodeStruct struct {
	Foo string `rmarsh:"quux"`
}

func TestEncodeStruct(t *testing.T) {
	testRubyDecode(t, &encodeStruct{Foo: "test"}, `{:quux=>test}`)
}
