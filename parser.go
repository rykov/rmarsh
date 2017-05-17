package rmarsh

import (
	// "fmt"
	"io"
	"math/big"
	"reflect"
	"strconv"
	"unsafe"

	"github.com/pkg/errors"
)

const (
	bufInitSz    = 256 // Initial size of our read buffer. We double it each time we overflow available space.
	rngTblInitSz = 8   // Initial size of range table entries
	stackGrowSz  = 8   // Amount to grow stack by when needed
)

// A Token represents a single distinct value type read from a Parser instance.
type Token uint8

// The valid token types.
const (
	tokenStart = iota
	TokenNil
	TokenTrue
	TokenFalse
	TokenFixnum
	TokenFloat
	TokenBignum
	TokenSymbol
	TokenString
	TokenStartArray
	TokenEndArray
	TokenStartHash
	TokenEndHash
	TokenStartIVar
	TokenEndIVar
	TokenLink
	TokenEOF
)

var tokenNames = map[Token]string{
	TokenNil:        "TokenNil",
	TokenTrue:       "TokenTrue",
	TokenFalse:      "TokenFalse",
	TokenFixnum:     "TokenFixnum",
	TokenFloat:      "TokenFloat",
	TokenBignum:     "TokenBignum",
	TokenSymbol:     "TokenSymbol",
	TokenString:     "TokenString",
	TokenStartArray: "TokenStartArray",
	TokenEndArray:   "TokenEndArray",
	TokenStartHash:  "TokenStartHash",
	TokenEndHash:    "TokenEndHash",
	TokenStartIVar:  "TokenStartIVar",
	TokenEndIVar:    "TokenEndIVar",
	TokenLink:       "TokenLink",
	TokenEOF:        "EOF",
}

func (t Token) String() string {
	if n, ok := tokenNames[t]; ok {
		return n
	}
	return "UNKNOWN"
}

const (
	ctxArray = iota
	ctxHash
	ctxIVar
)

type parserContext struct {
	typ uint8
	sz  int
	pos int

	ivSym *string // If current context is an IVar, then this will contain the instance variable name
}

// Parser is a low-level streaming implementation of the Ruby Marshal 4.8 format.
type Parser struct {
	r io.Reader // Where we are pulling the Marshal stream bytes from

	cur Token // The token we have most recently parsed

	st   []parserContext
	stSz int
	cst  *parserContext

	buf []byte // The read buffer contains every bit of data that we've read for the stream.
	pos int    // Our write position in the read buffer
	ctx rng    // Range of the raw data for the current token

	num      int
	bnumbits []big.Word
	bnumsign byte

	symTbl rngTbl // Store ranges marking the symbols we've parsed in the read buffer.
	lnkTbl rngTbl // Store ranges marking the linkable objects we've parsed in the read buffer.
}

// A range encodes a pair of start/end positions, used to mark interesting locations in the read buffer.
type rng struct{ beg, end int }

// Range table
type rngTbl []rng

func (t *rngTbl) add(r rng) (id int) {
	// We track the current parse sym table by slicing the underlying array.
	// That is, if we've seen one symbol in the stream so far, len(p.symTbl) == 1 && cap(p.symTable) == rngTblInitSz
	// Once we exceed cap, we double size of the tbl.
	id = len(*t)
	if c := cap(*t); id == c {
		if c == 0 {
			c = rngTblInitSz
		} else {
			c = c * 2
		}
		newT := make([]rng, c)
		copy(newT, *t)
		*t = newT[0:id]
	}
	*t = append(*t, r)
	return
}

// NewParser constructs a new parser that streams data from the given io.Reader
// Due to the nature of the Marshal format, data is read in very small increments. Please ensure that the provided
// Reader is buffered, or wrap it in a bufio.Reader.
func NewParser(r io.Reader) *Parser {
	p := &Parser{
		r:   r,
		buf: make([]byte, bufInitSz),
	}
	return p
}

// Reset reverts the Parser into the identity state, ready to read a new Marshal 4.8 stream from the existing Reader.
// If the provided io.Reader is nil, the existing Reader will continue to be used.
func (p *Parser) Reset(r io.Reader) {
	if r != nil {
		p.r = r
	}
	p.pos = 0
	p.cur = tokenStart
	p.stSz = 0
	p.symTbl = p.symTbl[0:0]
	p.lnkTbl = p.lnkTbl[0:0]
}

// Next advances the parser to the next token in the stream.
func (p *Parser) Next() (Token, error) {
	// If we're currently parsing an IVar, then we handle the next symbol+value pair.
	if p.cst != nil && p.cst.typ == ctxIVar {
		if p.cst.sz > 0 {
			return p.advIVar()
		} else if p.cst.sz < 0 {
			// Crappy state handling being encoded in magic numbers.
			// This situation means we only just parsed the beginning of the IVar
			// in the previous Next() call. So we need to let the actual value read
			// start. We mark the sz as 0 so that once we're back to this context
			// (after current value is parsed) we'll then read the instance variable
			// length and read all the instance vars.
			p.cst.sz = 0
		} else {
			// If we get here, it's because we finished parsing the actual value for an IVar
			// and now it's time to parse the instance variables.
			n, err := p.long()
			if err != nil {
				return tokenStart, errors.Wrap(err, "ivar")
			}
			p.cst.pos = 0
			p.cst.sz = int(n)
			return p.advIVar()
		}
	} else if p.cst != nil && p.cst.pos == p.cst.sz {
		// If we're in the middle of an array/map, check if we've finished it.
		switch p.cst.typ {
		case ctxArray:
			p.cur = TokenEndArray
		case ctxHash:
			p.cur = TokenEndHash
		}

		p.popStack()
		return p.cur, nil
	}

	if err := p.adv(); err != nil {
		return 0, errors.Wrap(err, "rmarsh.Parser.Next()")
	}

	if p.cst != nil {
		p.cst.pos++
	}

	return p.cur, nil
}

// ExpectNext is a convenience method that calls Next() and ensures the next token is the one provided.
func (p *Parser) ExpectNext(exp Token) error {
	tok, err := p.Next()
	if err != nil {
		return err
	}
	if tok != exp {
		return errors.Errorf("rmarsh.Parser.ExpectNext(): read token %s, expected %s", tok, exp)
	}
	return nil
}

// Len returns the number of elements to be read in the current structure.
// Returns 0 if the current token is not TokenStartArray, TokenStartHash, etc.
func (p *Parser) Len() int {
	if p.cur != TokenStartArray && p.cur != TokenStartHash {
		return 0
	}

	return p.cst.sz
}

// LinkId returns the id number for the current link value, or the expected link id for a linkable value.
// Only valid for the first token of linkable values such as TokenFloat, TokenString, TokenStartHash, TokenStartArray,
// etc. Returns -1 for anything else.
func (p *Parser) LinkId() int {
	switch p.cur {
	case TokenLink:
		return p.num
	case TokenFloat, TokenStartArray:
		return len(p.lnkTbl) - 1
	}
	return -1
}

// Int returns the value contained in the current Fixnum token.
// A fixnum will not exceed an int32, so this method returns int.
// Returns an error if called for any other type of token.
func (p *Parser) Int() (int, error) {
	if p.cur != TokenFixnum {
		return 0, errors.Errorf("rmarsh.Parser.Int() called for wrong token: %s", p.cur)
	}
	return p.num, nil
}

// Float returns the value contained in the current Float token.
// Converting the current context into a float is expensive, be  sure to only call this once for each distinct value.
// Returns an error if called for any other type of token.
func (p *Parser) Float() (float64, error) {
	if p.cur != TokenFloat {
		return 0, errors.Errorf("rmarsh.Parser.Float() called for wrong token: %s", p.cur)
	}

	// Avoid some unnecessary allocations by constructing a raw string view over the bytes. This is safe because the
	// fake string is not leaked outside of this method call - the bytes only need to stay constant for the call to
	// strconv.ParseFloat.
	buf := p.buf[p.ctx.beg:p.ctx.end]
	bytesHeader := (*reflect.SliceHeader)(unsafe.Pointer(&buf))
	strHeader := reflect.StringHeader{Data: bytesHeader.Data, Len: bytesHeader.Len}
	str := *(*string)(unsafe.Pointer(&strHeader))

	flt, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, errors.Wrap(err, "rmarsh.Parser.Float()")
	}
	return flt, nil
}

// Bignum returns the value contained in the current Bignum token.
// Converting the current context into a big.Int is expensive, be  sure to only call this once for each distinct value.
// Returns an error if called for any other type of token.
func (p *Parser) Bignum() (big.Int, error) {
	if p.cur != TokenBignum {
		return big.Int{}, errors.Errorf("rmarsh.Parser.Bignum() called for wrong token: %s", p.cur)
	}

	wordsz := (p.ctx.end - p.ctx.beg + _S - 1) / _S
	if len(p.bnumbits) < wordsz {
		p.bnumbits = make([]big.Word, wordsz)
	}

	k := 0
	s := uint(0)
	var d big.Word

	var i int
	for pos := p.ctx.beg; pos <= p.ctx.end; pos++ {
		d |= big.Word(p.buf[pos]) << s
		if s += 8; s == _S*8 {
			p.bnumbits[k] = d
			k++
			s = 0
			d = 0
		}
		i++
	}
	if k < wordsz {
		p.bnumbits[k] = d
	}

	var bnum big.Int
	bnum.SetBits(p.bnumbits[:wordsz])

	if p.bnumsign == '-' {
		bnum = *bnum.Neg(&bnum)
	}
	return bnum, nil
}

// Bytes returns the raw bytes for the current token.
// NOTE: The return byte slice is the one that is used internally, it will be modified on the next call to Next().
// If any data needs to be kept, be sure to copy it out of the returned buffer.
func (p *Parser) Bytes() []byte {
	return p.buf[p.ctx.beg:p.ctx.end]
}

// IVarName returns the name of the instance variable that is currently being parsed.
// Errors if not presently parsing the variables of an IVar.
func (p *Parser) IVarName() (string, error) {
	if p.cst == nil || p.cst.typ != ctxIVar {
		return "", errors.New("rmarsh.Parser.IVarName() called outside of an IVar")
	}

	return *p.cst.ivSym, nil
}

// Text returns the value contained in the current token interpreted as a string.
// Errors if the token is not one of Float, Bignum, Symbol or String
func (p *Parser) Text() (string, error) {
	switch p.cur {
	case TokenBignum:
		return string(p.bnumsign) + string(p.buf[p.ctx.beg:p.ctx.end]), nil
	case TokenFloat, TokenSymbol, TokenString:
		return string(p.buf[p.ctx.beg:p.ctx.end]), nil
	}
	return "", errors.Errorf("rmarsh.Parser.Text() called for wrong token: %s", p.cur)
}

func (p *Parser) adv() (err error) {
	var r rng

	if p.cur == tokenStart {
		if _, err := p.fill(3); err != nil {
			return errors.Wrap(err, "reading magic")
		} else if p.buf[0] != 0x04 || p.buf[1] != 0x08 {
			return errors.Errorf("Expected magic header 0x0408, got 0x%.4X", int16(p.buf[0])<<8|int16(p.buf[1]))
		} else {
			// Minor optimisation: we fetched 3 bytes on the first read since there is always at least one token to read.
			r.beg = 2
		}
	} else {
		r.beg = p.pos
		_, err := p.fill(1)
		if err == io.ErrUnexpectedEOF {
			p.cur = TokenEOF
			return nil
		} else if err != nil {
			return errors.Wrap(err, "read type id")
		}
	}

	switch p.buf[r.beg] {
	case typeNil:
		p.cur = TokenNil
	case typeTrue:
		p.cur = TokenTrue
	case typeFalse:
		p.cur = TokenFalse
	case typeFixnum:
		p.cur = TokenFixnum
		p.num, err = p.long()
		if err != nil {
			return errors.Wrap(err, "fixnum")
		}
	case typeFloat:
		p.cur = TokenFloat
		if p.ctx, err = p.sizedBlob(false); err != nil {
			return errors.Wrap(err, "float")
		}
		r.end = p.pos
		p.lnkTbl.add(r)
	case typeBignum:
		p.cur = TokenBignum
		r, err := p.fill(1)
		if err != nil {
			return errors.Wrap(err, "bignum")
		}
		p.bnumsign = p.buf[r.beg]

		if p.ctx, err = p.sizedBlob(true); err != nil {
			return errors.Wrap(err, "bignum")
		}
	case typeSymbol:
		p.cur = TokenSymbol
		p.ctx, err = p.sizedBlob(false)
		if err != nil {
			return errors.Wrap(err, "symbol")
		}
		p.symTbl.add(p.ctx)
	case typeString:
		p.cur = TokenString
		if p.ctx, err = p.sizedBlob(false); err != nil {
			return errors.Wrap(err, "string")
		}
	case typeSymlink:
		p.cur = TokenSymbol
		n, err := p.long()
		if err != nil {
			return errors.Wrap(err, "symlink id")
		}
		id := int(n)
		if id >= len(p.symTbl) {
			return errors.Errorf("Symlink id %d is larger than max known %d", id, len(p.symTbl)-1)
		}
		p.ctx = p.symTbl[id]
	case typeArray:
		p.cur = TokenStartArray
		n, err := p.long()
		if err != nil {
			return errors.Wrap(err, "array")
		}
		p.pushStack(ctxArray, int(n))
		p.lnkTbl.add(r)
	case typeHash:
		p.cur = TokenStartHash
		n, err := p.long()
		if err != nil {
			return errors.Wrap(err, "hash")
		}
		p.pushStack(ctxHash, int(n*2))
	case typeIvar:
		p.cur = TokenStartIVar
		p.pushStack(ctxIVar, -1)
	case typeLink:
		p.cur = TokenLink
		p.num, err = p.long()
		if err != nil {
			return errors.Wrap(err, "link")
		}
	}

	return nil
}

func (p *Parser) advIVar() (Token, error) {
	if p.cst.pos == p.cst.sz {
		p.cur = TokenEndIVar
		p.popStack()
		return p.cur, nil
	}
	p.cst.pos++

	// Next thing needs to be a symbol, or things are really FUBAR.
	if err := p.adv(); err != nil {
		return p.cur, err
	} else if p.cur != TokenSymbol {
		return tokenStart, errors.Errorf("Unexpected token type %s while parsing IVar, expected Symbol", p.cur)
	}
	sym := string(p.buf[p.ctx.beg:p.ctx.end])
	p.cst.ivSym = &sym

	err := p.adv()
	return p.cur, err
}

func (p *Parser) pushStack(typ uint8, sz int) {
	// Grow stack if needed
	if l := len(p.st); p.stSz == l {
		newStack := make([]parserContext, l+stackGrowSz)
		copy(newStack, p.st)
		p.st = newStack
	}

	p.cst = &p.st[p.stSz]
	p.cst.typ = typ
	p.cst.sz = sz
	p.cst.pos = -1

	p.stSz++
}

func (p *Parser) popStack() {
	p.stSz--
	if p.stSz > 0 {
		p.cst = &p.st[p.stSz-1]
		p.cst.pos++
	} else {
		p.cst = nil
	}
}

// Strings, Symbols, Floats, Bignums and the like all begin with an encoded long
// for the size and then the raw bytes. In most cases, interpreting those bytes
// is relatively expensive - and the caller may not even care (just skips to the
// next token). So, we save off the raw bytes and interpret them only when needed.
func (p *Parser) sizedBlob(bnum bool) (r rng, err error) {
	var sz int
	sz, err = p.long()
	if err != nil {
		return
	}

	// For some stupid reason bignums store the length in shorts, not bytes.
	if bnum {
		sz = sz * 2
	}

	return p.fill(sz)
}

func (p *Parser) long() (n int, err error) {
	_, err = p.fill(1)
	if err != nil {
		return
	}

	n = int(p.buf[p.pos-1])
	if n == 0 {
		return
	} else if 4 < n && n < 128 {
		n = n - 5
		return
	} else if -129 < n && n < -4 {
		n = n + 5
		return
	}

	var r rng

	if n > 0 {
		r, err = p.fill(n)
		if err != nil {
			return
		}
		n = 0
		var i int
		for r.beg <= r.end {
			n |= int(p.buf[r.beg]) << uint(8*i)
			i++
			r.beg++
		}
		return
	}

	r, err = p.fill(-n)
	if err != nil {
		return
	}
	n = -1
	var i int
	for r.beg <= r.end {
		n &= ^(0xff << uint(8*i))
		n |= int(p.buf[r.beg]) << uint(8*i)
		i++
		r.beg++
	}

	return
}

// Pull bytes from the io.Reader into our read buffer.
func (p *Parser) fill(num int) (r rng, err error) {
	r.beg = p.pos
	r.end = p.pos + num

	if r.end > len(p.buf) {
		// Overflowed our read buffer, allocate a new one double the size,
		buf := make([]byte, len(p.buf)*2)
		copy(buf, p.buf)
		p.buf = buf
	}

	var rd, n int
	for rd < num && err == nil {
		n, err = p.r.Read(p.buf[p.pos:r.end])
		rd += n
		p.pos += n
	}
	// fmt.Printf("cunt %d %d\n", num, rd)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return
}
