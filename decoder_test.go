package rubymarshal

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

var (
	rubyDec    *exec.Cmd
	rubyDecOut *bufio.Scanner
	rubyDecIn  io.Writer
)

var streamDelim = []byte("$$END$$")

func scanStream(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if len(data) >= 7 {
		for i := 0; i <= len(data)-7; i++ {
			if bytes.Compare(data[i:i+7], streamDelim) == 0 {
				return i + 7, data[0:i], nil
			}
		}
	}
	return 0, nil, nil
}

func testRubyEncode(t *testing.T, payload string, expected interface{}) {
	if rubyDec == nil {
		rubyDec = exec.Command("ruby", "decoder_test.rb")
		// Send stderr to top level so it's obvious if the Ruby script blew up somehow.
		rubyDec.Stderr = os.Stdout

		stdout, err := rubyDec.StdoutPipe()
		if err != nil {
			panic(err)
		}
		stdin, err := rubyDec.StdinPipe()
		if err != nil {
			panic(err)
		}
		if err := rubyDec.Start(); err != nil {
			panic(err)
		}

		rubyDecOut = bufio.NewScanner(stdout)
		rubyDecOut.Split(scanStream)
		rubyDecIn = stdin
	}

	_, err := io.WriteString(rubyDecIn, fmt.Sprintf("%s\n", payload))
	if err != nil {
		panic(err)
	}

	rubyDecOut.Scan()
	raw := rubyDecOut.Bytes()
	dec := NewDecoder(bytes.NewReader(raw))
	v, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode() failed: %s\nRaw ruby encoded:\n%s", err, hex.Dump(raw))
	}

	if !reflect.DeepEqual(v, expected) {
		t.Errorf("Decode() gave %#v (%T), expected %#v\nRaw ruby encoded:\n%s", v, v, expected, hex.Dump(raw))
	}
}

func TestDecodeNil(t *testing.T) {
	testRubyEncode(t, "nil", nil)
}

func TestDecodeTrue(t *testing.T) {
	testRubyEncode(t, "true", true)
}

func TestDecodeFalse(t *testing.T) {
	testRubyEncode(t, "false", false)
}

func TestDecodeFixnums(t *testing.T) {
	testRubyEncode(t, "0", int64(0))
	testRubyEncode(t, "1", int64(1))
	testRubyEncode(t, "122", int64(122))
	testRubyEncode(t, "0xDEAD", int64(0xDEAD))
	testRubyEncode(t, "0xDEADBE", int64(0xDEADBE))

	testRubyEncode(t, "-1", int64(-1))
	testRubyEncode(t, "-123", int64(-123))
	testRubyEncode(t, "-0xDEAD", int64(-0xDEAD))
}

func TestDecodeBignums(t *testing.T) {
	var exp big.Int
	exp.SetString("DEADCAFEBEEF", 16)
	testRubyEncode(t, "0xDEADCAFEBEEF", &exp)

	exp.SetString("-DEADCAFEBEEF", 16)
	testRubyEncode(t, "-0xDEADCAFEBEEF", &exp)
}

func TestDecodeArray(t *testing.T) {
	testRubyEncode(t, "[]", []interface{}{})
	testRubyEncode(t, "[nil, true, false]", []interface{}{nil, true, false})
	testRubyEncode(t, "[[]]", []interface{}{[]interface{}{}})
}

func TestDecodeSymbol(t *testing.T) {
	testRubyEncode(t, ":test", NewSymbol("test"))
}

func TestDecodeSymlink(t *testing.T) {
	testRubyEncode(t, "[:test,:test]", []interface{}{NewSymbol("test"), NewSymbol("test")})
}

func TestDecodeModule(t *testing.T) {
	testRubyEncode(t, "Process", NewModule("Process"))
}

func TestDecodeClass(t *testing.T) {
	testRubyEncode(t, "Gem::Version", NewClass("Gem::Version"))
}

func TestDecodeString(t *testing.T) {
	testRubyEncode(t, `"test".force_encoding("SHIFT_JIS")`, "test")
}