// Package pycsv reads CSV exactly as Python's csv module does with the excel dialect, as Signal
// Atlas reads its surveys: '"' quoting with doubled quotes, newlines inside quotes kept, CR, LF and
// CRLF all ending a record, a blank line as an empty record [], a quote that does not open a field
// kept as an ordinary character, and the 131,072-character field limit. Line numbers and
// invalid-row counts then agree with Graphite's Python.
package pycsv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// FieldLimit is csv.field_size_limit()'s default.
const FieldLimit = 131072

// ErrNotUTF8 is reported for text that is not UTF-8 (Python's utf-8-sig decoding fails there).
var ErrNotUTF8 = errors.New("'utf-8' codec can't decode the file: it is not UTF-8 text")

const (
	startRecord = iota
	startField
	inField
	inQuoted
	quoteInQuoted
)

// Parser is a streaming csv.reader. Feed it bytes, then End.
type Parser struct {
	state   int
	afterCR bool
	field   []byte
	fields  []string
	emit    func([]string) error
	bom     bool
}

// NewParser calls emit with every record (a fresh slice each time).
func NewParser(emit func([]string) error) *Parser { return &Parser{emit: emit} }

func (p *Parser) save() error {
	if !utf8.Valid(p.field) {
		return ErrNotUTF8
	}
	p.fields = append(p.fields, string(p.field))
	p.field = p.field[:0]
	return nil
}

func (p *Parser) end() error {
	rec := p.fields
	p.fields = nil
	p.state = startRecord
	return p.emit(rec)
}

func (p *Parser) add(c byte) error {
	if len(p.field) >= FieldLimit && utf8.RuneCount(p.field) >= FieldLimit {
		return fmt.Errorf("field larger than field limit (%d)", FieldLimit)
	}
	p.field = append(p.field, c)
	return nil
}

// Feed parses more bytes.
func (p *Parser) Feed(b []byte) error {
	if !p.bom {
		p.bom = true
		b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if p.afterCR {
			p.afterCR = false
			if c == '\n' {
				continue
			}
		}
		switch p.state {
		case startRecord:
			if c == '\r' || c == '\n' {
				if err := p.emit([]string{}); err != nil {
					return err
				}
				p.afterCR = c == '\r'
				continue
			}
			p.state = startField
			fallthrough
		case startField:
			switch c {
			case '"':
				p.state = inQuoted
			case ',':
				if err := p.save(); err != nil {
					return err
				}
			case '\r', '\n':
				if err := p.save(); err != nil {
					return err
				}
				p.afterCR = c == '\r'
				if err := p.end(); err != nil {
					return err
				}
			default:
				if err := p.add(c); err != nil {
					return err
				}
				p.state = inField
			}
		case inField:
			// fast path: copy the run of ordinary bytes at once
			j := i
			for j < len(b) && b[j] != ',' && b[j] != '\r' && b[j] != '\n' {
				j++
			}
			if j > i {
				if len(p.field)+(j-i) > FieldLimit && utf8.RuneCount(p.field)+utf8.RuneCount(b[i:j]) > FieldLimit {
					return fmt.Errorf("field larger than field limit (%d)", FieldLimit)
				}
				p.field = append(p.field, b[i:j]...)
				i = j - 1
				continue
			}
			if c == ',' {
				if err := p.save(); err != nil {
					return err
				}
				p.state = startField
			} else {
				if err := p.save(); err != nil {
					return err
				}
				p.afterCR = c == '\r'
				if err := p.end(); err != nil {
					return err
				}
			}
		case inQuoted:
			j := bytes.IndexByte(b[i:], '"')
			end := len(b)
			if j >= 0 {
				end = i + j
			}
			if end > i {
				if len(p.field)+(end-i) > FieldLimit && utf8.RuneCount(p.field)+utf8.RuneCount(b[i:end]) > FieldLimit {
					return fmt.Errorf("field larger than field limit (%d)", FieldLimit)
				}
				p.field = append(p.field, b[i:end]...)
			}
			if j < 0 {
				i = len(b)
				continue
			}
			i = end
			p.state = quoteInQuoted
		case quoteInQuoted:
			switch c {
			case '"':
				if err := p.add('"'); err != nil {
					return err
				}
				p.state = inQuoted
			case ',':
				if err := p.save(); err != nil {
					return err
				}
				p.state = startField
			case '\r', '\n':
				if err := p.save(); err != nil {
					return err
				}
				p.afterCR = c == '\r'
				if err := p.end(); err != nil {
					return err
				}
			default:
				if err := p.add(c); err != nil {
					return err
				}
				p.state = inField
			}
		}
	}
	return nil
}

// End finishes the input as Python does at end of file.
func (p *Parser) End() error {
	switch p.state {
	case startRecord:
		return nil
	default:
		if err := p.save(); err != nil {
			return err
		}
		return p.end()
	}
}

// ParseString parses a whole string (tests).
func ParseString(s string) ([][]string, error) {
	var out [][]string
	p := NewParser(func(r []string) error { out = append(out, r); return nil })
	if err := p.Feed([]byte(s)); err != nil {
		return out, err
	}
	return out, p.End()
}

// ReadFile streams a file through the parser; onBytes(read, total) is called per block.
func ReadFile(path string, emit func([]string) error, onBytes func(read, total int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	p := NewParser(emit)
	buf := make([]byte, 4<<20)
	var read int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			read += int64(n)
			if e := p.Feed(buf[:n]); e != nil {
				return e
			}
			if onBytes != nil {
				onBytes(read, st.Size())
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return p.End()
}
