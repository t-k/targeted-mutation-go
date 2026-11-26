package campaign

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"reflect"
	"strconv"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type prSource struct {
	path      string
	data      []byte
	file      *ast.File
	positions *token.File
}

func parsePRSource(path string, data []byte) *prSource {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	return &prSource{path: path, data: data, file: file, positions: fset.File(file.Pos())}
}

func (s *prSource) offset(pos token.Pos) int { return s.positions.Offset(pos) }

func (s *prSource) text(node ast.Node) []byte {
	return s.data[s.offset(node.Pos()):s.offset(node.End())]
}

func (s *prSource) signature(node ast.Node) string {
	if node == nil {
		return ""
	}
	return tokenSignature(s.text(node))
}

type expressionAnchor struct {
	expr   ast.Expr
	parent reflect.Type
}

func (s *prSource) expressions(visit func(ast.Expr, ast.Node)) {
	ast.PreorderStack(s.file, nil, func(node ast.Node, parents []ast.Node) bool {
		if expr, ok := node.(ast.Expr); ok && len(parents) != 0 {
			visit(expr, parents[len(parents)-1])
		}
		return true
	})
}

// A matching expression must retain a byte-exact surrounding anchor in the
// unchanged prefix or suffix of the hunk. The old expression's role and
// ordered terms or keys then establish which component was added.
func subHunkReverts(current *prSource, wholeRevert []byte, from, to int) []target.Replacement {
	if current == nil || from == to {
		return nil
	}
	old := parsePRSource(current.path, wholeRevert)
	if old == nil {
		return nil
	}
	prefix, suffix := matchingEdges(current.data, old.data)
	delta := len(current.data) - len(old.data)
	oldByStart, oldByEnd := map[int][]expressionAnchor{}, map[int][]expressionAnchor{}
	old.expressions(func(expr ast.Expr, parent ast.Node) {
		anchor := expressionAnchor{expr: expr, parent: reflect.TypeOf(parent)}
		start, end := old.offset(expr.Pos()), old.offset(expr.End())
		if start <= prefix {
			oldByStart[start] = append(oldByStart[start], anchor)
		}
		if end >= len(old.data)-suffix {
			oldByEnd[end] = append(oldByEnd[end], anchor)
		}
	})
	seen := map[string]bool{tokenSignature(old.data): true, tokenSignature(current.data): true}
	var result []target.Replacement
	current.expressions(func(expr ast.Expr, parent ast.Node) {
		start, end := current.offset(expr.Pos()), current.offset(expr.End())
		if start >= to || end <= from {
			return
		}
		if binary, ok := expr.(*ast.BinaryExpr); ok {
			if binary.Op != token.LAND && binary.Op != token.LOR {
				return
			}
			if enclosing, ok := parent.(*ast.BinaryExpr); ok && enclosing.Op == binary.Op {
				return
			}
		} else if _, ok := expr.(*ast.CompositeLit); !ok {
			return
		}
		matches := oldByStart[start]
		ends := oldByEnd[end-delta]
		if end < len(current.data)-suffix {
			ends = nil
		}
		var edits []prRemoval
		matched := make(map[ast.Expr]bool)
		for _, group := range [][]expressionAnchor{matches, ends} {
			for _, anchor := range group {
				if anchor.parent != reflect.TypeOf(parent) || matched[anchor.expr] {
					continue
				}
				matched[anchor.expr] = true
				var candidate []prRemoval
				switch cur := expr.(type) {
				case *ast.BinaryExpr:
					candidate = logicalRemovals(current, old, cur, anchor.expr)
				case *ast.CompositeLit:
					if prior, ok := anchor.expr.(*ast.CompositeLit); ok {
						candidate = literalRemovals(current, old, cur, prior)
					}
				}
				if len(candidate) == 0 {
					continue
				}
				if len(edits) != 0 {
					return
				}
				edits = candidate
			}
		}
		for _, edit := range edits {
			termStart, termEnd := current.offset(edit.term.Pos()), current.offset(edit.term.End())
			if termStart >= to || termEnd <= from || current.cutsComment(edit.from, edit.to) {
				continue
			}
			data := make([]byte, 0, len(current.data)-(edit.to-edit.from))
			data = append(data, current.data[:edit.from]...)
			data = append(data, current.data[edit.to:]...)
			if _, err := parser.ParseFile(token.NewFileSet(), current.path, data, parser.SkipObjectResolution); err != nil {
				continue
			}
			signature := tokenSignature(data)
			if seen[signature] {
				continue
			}
			seen[signature] = true
			position := current.positions.PositionFor(edit.term.Pos(), false)
			last := current.positions.PositionFor(edit.term.End()-1, false)
			result = append(result, target.Replacement{Path: current.path, Line: position.Line, EndLine: last.Line,
				Column: position.Column, Description: edit.description, Data: data})
		}
	})
	return result
}

func matchingEdges(a, b []byte) (prefix, suffix int) {
	limit := min(len(a), len(b))
	for prefix < limit && a[prefix] == b[prefix] {
		prefix++
	}
	for suffix < limit-prefix && a[len(a)-suffix-1] == b[len(b)-suffix-1] {
		suffix++
	}
	return prefix, suffix
}

type prRemoval struct {
	term        ast.Expr
	from, to    int
	description string
}

func logicalTerms(expr ast.Expr, op token.Token, terms *[]ast.Expr, operators *[]token.Pos) {
	if binary, ok := expr.(*ast.BinaryExpr); ok && binary.Op == op {
		logicalTerms(binary.X, op, terms, operators)
		*operators = append(*operators, binary.OpPos)
		logicalTerms(binary.Y, op, terms, operators)
		return
	}
	*terms = append(*terms, expr)
}

func logicalRemovals(current, old *prSource, root *ast.BinaryExpr, prior ast.Expr) []prRemoval {
	var terms, previous []ast.Expr
	var operators, previousOperators []token.Pos
	logicalTerms(root, root.Op, &terms, &operators)
	logicalTerms(prior, root.Op, &previous, &previousOperators)
	names := func(src *prSource, atoms []ast.Expr) []string {
		result := make([]string, len(atoms))
		for i, atom := range atoms {
			result[i] = src.signature(atom)
		}
		return result
	}
	var edits []prRemoval
	for _, i := range addedTerms(names(old, previous), names(current, terms)) {
		term := terms[i]
		from, to := current.offset(term.Pos()), current.offset(term.End())
		if i == 0 {
			to = current.offset(operators[0]) + len(root.Op.String())
		} else {
			from = current.offset(operators[i-1])
		}
		edits = append(edits, prRemoval{term: term, from: from, to: to,
			description: "remove added " + root.Op.String() + " term " + string(current.text(term))})
	}
	return edits
}

func literalRemovals(current, old *prSource, literal, prior *ast.CompositeLit) []prRemoval {
	if current.signature(literal.Type) != old.signature(prior.Type) {
		return nil
	}
	currentKeys, oldKeys := literalKeys(current, literal), literalKeys(old, prior)
	if currentKeys == nil || oldKeys == nil {
		return nil
	}
	var edits []prRemoval
	for _, i := range addedTerms(oldKeys, currentKeys) {
		term := literal.Elts[i].(*ast.KeyValueExpr)
		from, to := current.offset(term.Pos()), current.offset(term.End())
		if i+1 < len(literal.Elts) {
			to = current.offset(literal.Elts[i+1].Pos())
		} else if comma := current.comma(to, current.offset(literal.Rbrace)); comma >= 0 {
			to = comma + 1
		} else if i > 0 {
			from = current.comma(current.offset(literal.Elts[i-1].End()), from)
			if from < 0 {
				continue
			}
		}
		edits = append(edits, prRemoval{term: term, from: from, to: to,
			description: "remove added keyed field " + string(current.text(term.Key))})
	}
	return edits
}

func literalKeys(source *prSource, literal *ast.CompositeLit) []string {
	keys := make([]string, 0, len(literal.Elts))
	seen := make(map[string]bool, len(literal.Elts))
	for _, element := range literal.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil
		}
		key := source.signature(entry.Key)
		if seen[key] {
			return nil
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// Matching both from the front and back rejects ambiguous insertions of
// repeated terms without enumerating every subsequence.
func addedTerms(old, current []string) []int {
	if len(old) >= len(current) {
		return nil
	}
	oldIndexes := make([]int, 0, len(old))
	index := 0
	for _, value := range old {
		for index < len(current) && current[index] != value {
			index++
		}
		if index == len(current) {
			return nil
		}
		oldIndexes = append(oldIndexes, index)
		index++
	}
	index = len(current) - 1
	for i := len(old) - 1; i >= 0; i-- {
		for current[index] != old[i] {
			index--
		}
		if index != oldIndexes[i] {
			return nil
		}
		index--
	}
	var additions []int
	next := 0
	for i := range current {
		if next < len(oldIndexes) && i == oldIndexes[next] {
			next++
		} else {
			additions = append(additions, i)
		}
	}
	return additions
}

func (s *prSource) cutsComment(from, to int) bool {
	for _, group := range s.file.Comments {
		for _, comment := range group.List {
			if s.offset(comment.Pos()) < to && s.offset(comment.End()) > from {
				return true
			}
		}
	}
	return false
}

func (s *prSource) comma(from, to int) int {
	var lex scanner.Scanner
	file := token.NewFileSet().AddFile("", -1, to-from)
	lex.Init(file, s.data[from:to], nil, 0)
	pos, kind, _ := lex.Scan()
	if kind != token.COMMA {
		return -1
	}
	return from + file.Offset(pos)
}

func tokenSignature(data []byte) string {
	var lex scanner.Scanner
	file := token.NewFileSet().AddFile("", -1, len(data))
	lex.Init(file, data, nil, 0)
	var signature strings.Builder
	for {
		_, kind, value := lex.Scan()
		if kind == token.EOF {
			return signature.String()
		}
		signature.WriteString(kind.String())
		signature.WriteByte(':')
		if kind != token.SEMICOLON {
			signature.WriteString(strconv.Itoa(len(value)))
			signature.WriteByte(':')
			signature.WriteString(value)
		}
		signature.WriteByte(';')
	}
}
