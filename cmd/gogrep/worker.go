package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/quasilyte/gogrep"
	"github.com/quasilyte/gogrep/filters"
	"github.com/quasilyte/perf-heatmap/heatmap"
)

type worker struct {
	id int

	countMode bool

	needCapture   bool
	needMatchLine bool

	workDir            string
	heatmapFilenameSet map[string]struct{}
	heatmap            *heatmap.Index

	filterHints filterHints
	filterInfo  *filters.Info
	filterExpr  *filters.Expr

	m           *gogrep.Pattern
	gogrepState gogrep.MatcherState
	fset        *token.FileSet

	matches []match

	errors []string

	data      []byte
	filename  string
	pkgName   string
	typeName  string
	funcName  string
	closureID int

	n int
}

func (w *worker) grepFile(filename string) (int, error) {
	// When doing a heatmap-based filtering, we can skip files
	// that are 100% outside of the heatmap.
	// heatmapFilenameSet is non nil if we should do this optimization.
	if w.heatmapFilenameSet != nil {
		if _, ok := w.heatmapFilenameSet[filepath.Base(filename)]; !ok {
			return 0, nil
		}
	}

	if w.filterHints.testCond != bool3unset {
		isTest := strings.HasSuffix(filename, "_test.go")
		if !w.filterHints.testCond.Eq(isTest) {
			return 0, nil
		}
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		return 0, fmt.Errorf("read file: %v", err)
	}

	w.fset = token.NewFileSet()
	root, err := w.parseFile(w.fset, filename, data)
	if err != nil {
		return 0, err
	}

	if w.filterHints.autogenCond != bool3unset {
		if !w.filterHints.autogenCond.Eq(isAutogenFile(root)) {
			return 0, nil
		}
	}

	w.data = data
	w.filename = filename
	w.pkgName = root.Name.Name

	w.n = 0

	walker := astWalker{
		worker: w,
		visit:  w.Visit,
	}
	walker.walk(root)

	return w.n, nil
}

func (w *worker) parseFile(fset *token.FileSet, filename string, data []byte) (*ast.File, error) {
	needComments := false
	if w.filterHints.autogenCond != bool3unset {
		needComments = true
	}
	parserFlags := parser.Mode(0)
	if needComments {
		parserFlags |= parser.ParseComments
	}
	f, err := parser.ParseFile(fset, filename, data, parserFlags)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (w *worker) Visit(n ast.Node) {
	w.m.MatchNode(&w.gogrepState, n, func(data gogrep.MatchData) {
		accept := w.filterExpr.Op == filters.OpNop ||
			applyFilter(filterContext{w: w, m: data}, w.filterExpr, data.Node)
		if !accept {
			return
		}

		w.n++

		if w.countMode {
			return
		}

		start := w.fset.Position(data.Node.Pos())
		end := w.fset.Position(data.Node.End())
		m := match{
			filename:    w.filename,
			line:        start.Line,
			startOffset: start.Offset,
			endOffset:   end.Offset,
		}
		if w.needCapture {
			w.initMatchCapture(&m, data.Capture)
		}
		w.initMatchText(&m, start.Offset, end.Offset)
		w.matches = append(w.matches, m)
	})
}

func (w *worker) initMatchCapture(m *match, capture []gogrep.CapturedNode) {
	m.capture = make([]capturedNode, len(capture))
	for i, c := range capture {
		startOffset := w.fset.Position(c.Node.Pos()).Offset
		endOffset := w.fset.Position(c.Node.End()).Offset
		m.capture[i] = capturedNode{
			startOffset: startOffset,
			endOffset:   endOffset,
			data:        c,
		}
	}
}

func (w *worker) initMatchText(m *match, startPos, endPos int) {
	if !w.needMatchLine {
		m.text = string(w.data[startPos:endPos])
		m.matchStartOffset = 0
		m.matchLength = len(m.text)
		return
	}

	isNewline := func(b byte) bool {
		return b == '\n' || b == '\r'
	}

	// Try to expand the match pos range in a way that it includes the
	// For example, if we have `if foo {` source line and `foo` matches,
	// we would want to record the `if foo {` string as a matching line.
	start := startPos
	for start > 0 {
		if isNewline(w.data[start]) {
			if start != startPos {
				start++
			}
			break
		}
		start--
	}
	end := endPos
	for end < len(w.data) {
		if isNewline(w.data[end]) {
			break
		}
		end++
	}
	m.text = string(w.data[start:end])
	m.matchStartOffset = startPos - start
	m.matchLength = endPos - startPos
}

func (w *worker) nodeText(n ast.Node) []byte {
	if gogrep.IsEmptyNodeSlice(n) {
		return nil
	}

	from := w.fset.Position(n.Pos()).Offset
	to := w.fset.Position(n.End()).Offset
	src := w.data
	if (from >= 0 && from < len(src)) && (to >= 0 && to < len(src)) {
		return src[from:to]
	}

	// Go printer would panic on comments.
	if n, ok := n.(*ast.Comment); ok {
		return []byte(n.Text)
	}

	// Fallback to the printer.
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, w.fset, n); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func isAutogenFile(f *ast.File) bool {
	for _, comment := range f.Comments {
		if isAutogenComment(comment) {
			return true
		}
	}
	return false
}

func isAutogenComment(comment *ast.CommentGroup) bool {
	generated := false
	doNotEdit := false
	for _, c := range comment.List {
		s := strings.ToLower(c.Text)
		if !generated {
			generated = strings.Contains(s, " code generated ") ||
				strings.Contains(s, " generated by ")
		}
		if !doNotEdit {
			doNotEdit = strings.Contains(s, "do not edit") ||
				strings.Contains(s, "don't edit")
		}
		if generated && doNotEdit {
			return true
		}
	}
	return false
}
