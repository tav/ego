package ego

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Template represents an entire Ego template.
// A template consists of a declaration block followed by zero or more blocks.
// Blocks can be either a TextBlock, a PrintBlock, or a CodeBlock.
type Template struct {
	Path   string
	Blocks []Block
}

// Write writes the template to a writer.
func (t *Template) Write(w io.Writer) error {
	buf := &bytes.Buffer{}

	decl := t.declarationBlock()
	if decl == nil {
		return ErrDeclarationRequired
	}

	// Write function declaration.
	decl.write(buf)

	// Write non-header blocks.
	for _, b := range t.nonHeaderBlocks() {
		if err := b.write(buf); err != nil {
			return err
		}
	}

	// Write function closing brace.
	fmt.Fprint(buf, "}\n\n")

	// Write code to external writer.
	_, err := buf.WriteTo(w)
	return err
}

func (t *Template) declarationBlock() *DeclarationBlock {
	for _, b := range t.Blocks {
		if b, ok := b.(*DeclarationBlock); ok {
			return b
		}
	}
	return nil
}

func (t *Template) headerBlocks() []*HeaderBlock {
	var blocks []*HeaderBlock
	for _, b := range t.Blocks {
		if b, ok := b.(*HeaderBlock); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func (t *Template) nonHeaderBlocks() []Block {
	var blocks []Block
	for _, b := range t.Blocks {
		switch b.(type) {
		case *DeclarationBlock, *HeaderBlock:
		default:
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func (t *Template) textBlocks() []*TextBlock {
	var blocks []*TextBlock
	for _, b := range t.Blocks {
		if b, ok := b.(*TextBlock); ok {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// normalize joins together adjacent text blocks.
func (t *Template) normalize() {
	var a []Block
	for _, b := range t.Blocks {
		if isTextBlock(b) && len(a) > 0 && isTextBlock(a[len(a)-1]) {
			a[len(a)-1].(*TextBlock).Content += b.(*TextBlock).Content
		} else {
			a = append(a, b)
		}
	}
	t.Blocks = a
}

// Block represents an element of the template.
type Block interface {
	block()
	write(*bytes.Buffer) error
}

func (b *DeclarationBlock) block() {}
func (b *TextBlock) block()        {}
func (b *CodeBlock) block()        {}
func (b *HeaderBlock) block()      {}
func (b *PrintBlock) block()       {}
func (b *WriteBlock) block()       {}

// DeclarationBlock represents a block that declaration the function signature.
type DeclarationBlock struct {
	Pos     Pos
	Content string
}

func (b *DeclarationBlock) write(buf *bytes.Buffer) error {
	fmt.Fprintf(buf, "%s {\n", b.Content)
	return nil
}

// TextBlock represents a UTF-8 encoded block of text that is written to the writer as-is.
type TextBlock struct {
	Pos     Pos
	Content string
	ID      int
}

func (b *TextBlock) write(buf *bytes.Buffer) error {
	if b.Content == "" {
		return nil
	}
	text := strconv.QuoteToASCII(b.Content)
	text = strings.Replace(text[1:len(text)-1], `\n`, "\n\t// ", -1)
	fmt.Fprintf(buf, "// %s\n", text)
	fmt.Fprintf(buf, "c.Write(__%d)\n", b.ID)
	return nil
}

// isTextBlock returns true if the block is a text block.
func isTextBlock(b Block) bool {
	_, ok := b.(*TextBlock)
	return ok
}

// CodeBlock represents a Go code block that is printed as-is to the template.
type CodeBlock struct {
	Pos     Pos
	Content string
}

func (b *CodeBlock) write(buf *bytes.Buffer) error {
	fmt.Fprintln(buf, b.Content)
	return nil
}

// HeaderBlock represents a Go code block that is printed at the top of the template.
type HeaderBlock struct {
	Pos     Pos
	Content string
}

func (b *HeaderBlock) write(buf *bytes.Buffer) error {
	fmt.Fprintln(buf, b.Content)
	return nil
}

// PrintBlock represents a block of the template that is printed out to the writer.
type PrintBlock struct {
	Pos     Pos
	Content string
}

func (b *PrintBlock) write(buf *bytes.Buffer) error {
	fmt.Fprintf(buf, "c.Write(Escape(%s))\n", b.Content)
	return nil
}

type WriteBlock struct {
	Pos     Pos
	Content string
}

func (b *WriteBlock) write(buf *bytes.Buffer) error {
	fmt.Fprintf(buf, "c.Write(%s)\n", b.Content)
	return nil
}

// Pos represents a position in a given file.
type Pos struct {
	Path   string
	LineNo int
}

func (p *Pos) write(buf *bytes.Buffer) {
	if p != nil && p.Path != "" && p.LineNo > 0 {
		fmt.Fprintf(buf, "//line %s:%d\n", p.Path, p.LineNo)
	}
}

// Package represents a collection of ego templates in a single package.
type Package struct {
	Name      string
	Templates []*Template
}

// Write writes out the package header and templates to a writer.
func (p *Package) Write(w io.Writer) error {
	if err := p.writeHeader(w); err != nil {
		return err
	}
	id := 0
	texts := map[string]int{}
	for _, t := range p.Templates {
		for _, b := range t.textBlocks() {
			b.Content = strings.TrimRight(strings.TrimLeft(b.Content, "\n "), "\n")
			if b.Content == "" {
				continue
			}
			if curID, exists := texts[b.Content]; exists {
				b.ID = curID
			} else {
				if id == 0 {
					fmt.Fprintf(w, "var (\n")
				}
				id++
				b.ID = id
				texts[b.Content] = id
				fmt.Fprintf(w, "\t__%d = []byte(%q)\n", id, b.Content)
			}
		}
	}
	if id != 0 {
		fmt.Fprint(w, ")\n\n")
	}
	for _, t := range p.Templates {
		if err := t.Write(w); err != nil {
			return fmt.Errorf("template: %s: err", t.Path)
		}
	}
	return nil
}

// Writes the package name and consolidated header blocks.
func (p *Package) writeHeader(w io.Writer) error {
	if p.Name == "" {
		return errors.New("package name required")
	}

	// Write naive header first.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n", p.Name)
	for _, t := range p.Templates {
		for _, b := range t.headerBlocks() {
			b.write(&buf)
		}
	}

	// Parse header into Go AST.
	f, err := parser.ParseFile(token.NewFileSet(), "ego.go", buf.String(), parser.ImportsOnly)
	if err != nil {
		fmt.Println(buf.String())
		return fmt.Errorf("writeHeader: %s", err)
	}

	// Reset buffer.
	buf.Reset()

	// Add note that the file is auto-generated
	fmt.Fprintf(&buf, "// Generated by ego on %s.\n// DO NOT EDIT\n\n", time.Now().Format(time.ANSIC))

	fmt.Fprintf(&buf, "package %s\n", p.Name)

	// Write deduped imports.
	var decls = map[string]bool{}
	fmt.Fprint(&buf, "import (\n")
	for _, d := range f.Decls {
		d, ok := d.(*ast.GenDecl)
		if !ok || d.Tok != token.IMPORT {
			continue
		}

		for _, s := range d.Specs {
			s := s.(*ast.ImportSpec)
			var id string
			if s.Name != nil {
				id = s.Name.Name
			}
			id += ":" + s.Path.Value

			// Ignore any imports which have already been imported.
			if decls[id] {
				continue
			}
			decls[id] = true

			// Otherwise write it.
			if s.Name == nil {
				fmt.Fprintf(&buf, "%s\n", s.Path.Value)
			} else {
				fmt.Fprintf(&buf, "%s %s\n", s.Name.Name, s.Path.Value)
			}
		}
	}
	fmt.Fprint(&buf, ")\n")

	// Write out to writer.
	buf.WriteTo(w)

	return nil
}

func warn(v ...interface{})              { fmt.Fprintln(os.Stderr, v...) }
func warnf(msg string, v ...interface{}) { fmt.Fprintf(os.Stderr, msg+"\n", v...) }
