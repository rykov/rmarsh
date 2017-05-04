package rmarsh

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"

	"github.com/pkg/errors"
)

var ErrGeneratorFinished = fmt.Errorf("Write on finished Marshal stream")
var ErrGeneratorOverflow = fmt.Errorf("Write past end of bounded array/hash/ivar")

const (
	genStateGrowSize = 8 // Initial size + amount to grow state stack by
)

type Generator struct {
	w  io.Writer
	c  int
	st genState

	buf  []byte
	bufn int

	symCount int
	symTbl   []string
}

func NewGenerator(w io.Writer) *Generator {
	gen := &Generator{
		w:   w,
		buf: make([]byte, 128),
	}
	gen.Reset()
	return gen
}

func (gen *Generator) Reset() {
	gen.st.reset()

	gen.c = 0
	gen.symCount = 0

	gen.buf[0] = 0x04
	gen.buf[1] = 0x08
	gen.bufn = 2
}

// Nil writes the nil value to the stream
func (gen *Generator) Nil() error {
	if err := gen.checkState(1); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_NIL
	gen.bufn++
	return gen.writeAdv()
}

// Bool writes a true/false value to the stream
func (gen *Generator) Bool(b bool) error {
	if err := gen.checkState(1); err != nil {
		return err
	}

	if b {
		gen.buf[gen.bufn] = TYPE_TRUE
	} else {
		gen.buf[gen.bufn] = TYPE_FALSE
	}
	gen.bufn++

	return gen.writeAdv()
}

// Fixnum writes a signed/unsigned number to the stream.
// Ruby has bounds on what can be encoded as a fixnum, those bounds are
// less than the range an int64 can cover. If the provided number overflows
// it will be encoded as a Bignum instead.
func (gen *Generator) Fixnum(n int64) error {
	if n < fixnumMin || n > fixnumMax {
		var bign big.Int
		bign.SetInt64(n)
		return gen.Bignum(&bign)
	}

	if err := gen.checkState(fixnumMaxBytes + 1); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_FIXNUM
	gen.bufn++
	gen.encodeLong(n)
	return gen.writeAdv()
}

func (gen *Generator) Bignum(b *big.Int) error {
	// We don't use big.Int.Bytes() for two reasons:
	// 1) it's an unnecessary buffer allocation which can't be avoided
	//    (can't provide an existing buffer for big.Int to write into)
	// 2) the returned buffer is big-endian but Ruby expects le.
	bits := b.Bits()
	l := len(bits)

	// Calculate the number of bytes we'll be writing.
	sz := 0
	for i, d := range bits {
		for j := 0; j < _S; j++ {
			sz++
			d >>= 8
			if d == 0 && i == l-1 {
				break
			}
		}
	}

	// bignum is encoded as a series of shorts. If we have an uneven number of
	// bytes we gotta pad it out.
	if sz&1 == 1 {
		sz++
	}

	if err := gen.checkState(2 + fixnumMaxBytes + sz); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_BIGNUM
	gen.bufn++
	if b.Sign() < 0 {
		gen.buf[gen.bufn] = '-'
	} else {
		gen.buf[gen.bufn] = '+'
	}
	gen.bufn++

	gen.encodeLong(int64(math.Ceil(float64(sz) / 2)))

	w := 0
	for i, d := range bits {
		for j := 0; j < _S; j++ {
			gen.buf[gen.bufn] = byte(d)
			gen.bufn++
			w++
			d >>= 8
			if d == 0 && i == l-1 {
				break
			}
		}
	}

	for w < sz {
		gen.buf[gen.bufn] = 0
		gen.bufn++
		w++
	}

	return gen.writeAdv()
}

func (gen *Generator) Symbol(sym string) error {
	if l := len(gen.symTbl); l == 0 || l == gen.symCount {
		newTbl := make([]string, l+symTblGrowSize)
		copy(newTbl, gen.symTbl)
		gen.symTbl = newTbl
	}

	for i := 0; i < gen.symCount; i++ {
		if gen.symTbl[i] == sym {
			if err := gen.checkState(1 + fixnumMaxBytes); err != nil {
				return err
			}
			gen.buf[gen.bufn] = TYPE_SYMLINK
			gen.bufn++
			gen.encodeLong(int64(i))
			return gen.writeAdv()
		}
	}

	l := len(sym)

	if err := gen.checkState(1 + fixnumMaxBytes + l); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_SYMBOL
	gen.bufn++

	gen.encodeLong(int64(l))
	copy(gen.buf[gen.bufn:], sym)
	gen.bufn += l

	gen.symTbl[gen.symCount] = sym
	gen.symCount++

	return gen.writeAdv()
}

func (gen *Generator) String(str string) error {
	l := len(str)
	if err := gen.checkState(1 + fixnumMaxBytes + l); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_STRING
	gen.bufn++
	gen.encodeLong(int64(l))
	copy(gen.buf[gen.bufn:], str)
	gen.bufn += l

	return gen.writeAdv()
}

func (gen *Generator) Float(f float64) error {
	// String repr of a float64 will never exceed 30 chars.
	// That also means the len encoded long will never exceed 1 byte.
	if err := gen.checkState(1 + 1 + 30); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_FLOAT
	gen.bufn++

	// We pass a 0 len slice of our scratch buffer to append float.
	// This ensures it makes no allocation since the append() calls it makes
	// will just consume existing capacity.
	b := strconv.AppendFloat(gen.buf[gen.bufn+1:gen.bufn+1:len(gen.buf)], f, 'g', -1, 64)
	l := len(b)

	gen.encodeLong(int64(l))
	gen.bufn += l

	return gen.writeAdv()
}

func (gen *Generator) StartArray(l int) error {
	if err := gen.checkState(1 + fixnumMaxBytes); err != nil {
		return err
	}
	gen.buf[gen.bufn] = TYPE_ARRAY
	gen.bufn++
	gen.encodeLong(int64(l))

	gen.st.push(genStArr, l)
	return nil
}

func (gen *Generator) EndArray() error {
	if gen.st.sz == 0 || gen.st.cur.typ != genStArr {
		return errors.New("EndArray() called outside of context of array")
	}
	if gen.st.cur.pos != gen.st.cur.cnt {
		return errors.Errorf("EndArray() called prematurely, %d of %d elems written", gen.st.cur.pos, gen.st.cur.cnt)
	}
	gen.st.pop()

	return gen.writeAdv()
}

func (gen *Generator) StartHash(l int) error {
	if err := gen.checkState(1 + fixnumMaxBytes); err != nil {
		return err
	}
	gen.buf[gen.bufn] = TYPE_HASH
	gen.bufn++
	gen.encodeLong(int64(l))

	gen.st.push(genStHash, l*2)
	return nil
}

func (gen *Generator) EndHash() error {
	if gen.st.sz == 0 || gen.st.cur.typ != genStHash {
		return errors.New("EndHash() called outside of context of hash")
	}
	if gen.st.cur.pos != gen.st.cur.cnt {
		return errors.Errorf("EndHash() called prematurely, %d of %d elems written", gen.st.cur.pos, gen.st.cur.cnt)
	}
	gen.st.pop()

	return gen.writeAdv()
}

func (gen *Generator) Class(name string) error {
	l := len(name)
	if err := gen.checkState(1 + fixnumMaxBytes + l); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_CLASS
	gen.bufn++
	gen.encodeLong(int64(l))
	copy(gen.buf[gen.bufn:], name)
	gen.bufn += l

	return gen.writeAdv()
}

func (gen *Generator) Module(name string) error {
	l := len(name)
	if err := gen.checkState(1 + fixnumMaxBytes + l); err != nil {
		return err
	}

	gen.buf[gen.bufn] = TYPE_MODULE
	gen.bufn++
	gen.encodeLong(int64(l))
	copy(gen.buf[gen.bufn:], name)
	gen.bufn += l

	return gen.writeAdv()
}

func (gen *Generator) checkState(sz int) error {
	// Make sure we're not writing past bounds.
	if gen.st.cur.pos == gen.st.cur.cnt {
		if gen.st.sz == 1 {
			return ErrGeneratorFinished
		} else {
			return ErrGeneratorOverflow
		}
	}

	if len(gen.buf) < gen.bufn+sz {
		newBuf := make([]byte, gen.bufn+sz)
		if gen.bufn > 0 {
			copy(newBuf, gen.buf)
		}
		gen.buf = newBuf
	}

	return nil
}

// Writes the given bytes if provided, then advances current state of the generator.
func (gen *Generator) writeAdv() error {
	gen.st.cur.pos++

	// If we've just finished writing out the last value, then we make sure to flush anything remaining.
	// Otherwise, we let things accumulate in our small buffer between calls to reduce the number of writes.
	if gen.bufn > 0 && gen.st.cur.pos == gen.st.cur.cnt && gen.st.sz == 1 {
		if _, err := gen.w.Write(gen.buf[:gen.bufn]); err != nil {
			return err
		}
		gen.c += gen.bufn
		gen.bufn = 0
	}

	return nil
}

func (gen *Generator) encodeLong(n int64) {
	if n == 0 {
		gen.buf[gen.bufn] = 0
		gen.bufn++
		return
	} else if 0 < n && n < 0x7B {
		gen.buf[gen.bufn] = byte(n + 5)
		gen.bufn++
		return
	} else if -0x7C < n && n < 0 {
		gen.buf[gen.bufn] = byte((n - 5) & 0xFF)
		gen.bufn++
		return
	}

	for i := 1; i < 5; i++ {
		gen.buf[gen.bufn+i] = byte(n & 0xFF)
		n = n >> 8
		if n == 0 {
			gen.buf[gen.bufn] = byte(i)
			gen.bufn += i + 1
			return
		}
		if n == -1 {
			gen.buf[gen.bufn] = byte(-i)
			gen.bufn += i + 1
			return
		}
	}
	panic("Shouldn't *ever* reach here")
}

const (
	genStTop = iota
	genStArr
	genStHash
)

type genStateItem struct {
	cnt int
	pos int
	typ uint8
}

func (st *genStateItem) reset(sz int, typ uint8) {
	st.cnt = sz
	st.pos = 0
	st.typ = typ
}

type genState struct {
	stack []genStateItem
	sz    int
	cur   *genStateItem
}

// Resets generator state back to initial state (which is ready for a new
// top level value to be written)
func (st *genState) reset() {
	st.sz = 0
	st.push(genStTop, 1)
}

func (st *genState) push(typ uint8, cnt int) {
	if st.sz == len(st.stack) {
		newSt := make([]genStateItem, st.sz+genStateGrowSize)
		copy(newSt, st.stack)
		st.stack = newSt
	}

	st.stack[st.sz].typ = typ
	st.stack[st.sz].cnt = cnt
	st.stack[st.sz].pos = 0
	st.cur = &st.stack[st.sz]
	st.sz++
}

func (st *genState) pop() {
	st.sz--
	if st.sz > 0 {
		st.cur = &st.stack[st.sz-1]
	} else {
		st.cur = nil
	}
}
