package target

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"math"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

var binaryFamilies = [][]token.Token{
	{token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ},
	{token.ADD, token.SUB, token.MUL, token.QUO, token.REM},
	{token.AND, token.OR, token.XOR, token.AND_NOT},
	{token.SHL, token.SHR},
	{token.LAND, token.LOR},
}

var compoundFamilies = [][]token.Token{
	{token.ADD_ASSIGN, token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN, token.REM_ASSIGN},
	{token.AND_ASSIGN, token.OR_ASSIGN, token.XOR_ASSIGN, token.AND_NOT_ASSIGN},
	{token.SHL_ASSIGN, token.SHR_ASSIGN},
}

type editKey struct {
	start, end  int
	replacement string
}

type sourceEdits struct {
	file      *File
	positions *token.File
	pkg       *packages.Package
	scope     Scope
	probed    map[int]bool
	added     map[editKey]bool
}

func discoverSourceEdits(file *File, tree *ast.File, positions *token.File, pkg *packages.Package, scope Scope) {
	edits := sourceEdits{file: file, positions: positions, pkg: pkg, scope: scope, probed: make(map[int]bool), added: make(map[editKey]bool)}
	for _, site := range file.Sites {
		edits.probed[site.Start] = true
	}
	ast.PreorderStack(tree, nil, edits.visit)
}

func (e *sourceEdits) visit(node ast.Node, ancestors []ast.Node) bool {
	if !e.inScope(node.Pos(), node.End()) {
		return false
	}
	var parent ast.Node
	if len(ancestors) != 0 {
		parent = ancestors[len(ancestors)-1]
	}
	switch n := node.(type) {
	case *ast.ImportSpec:
		return false
	case *ast.FuncDecl:
		return !hasCompilerDirective(n)
	case *ast.BinaryExpr:
		e.binary(n, parent)
	case *ast.UnaryExpr:
		e.unary(n)
	case *ast.BasicLit:
		e.literal(n)
	case *ast.Ident:
		switch {
		case e.builtin(n, "true"):
			e.boolean(n, false, "boolean-literal")
		case e.builtin(n, "false"):
			e.boolean(n, true, "boolean-literal")
		}
	case *ast.AssignStmt:
		e.assign(n, parent)
	case *ast.IncDecStmt:
		e.swap(n.TokPos, n.Tok, token.DEC, token.INC)
		e.replace(n, "statement-remove", "_ = "+e.text(n.X))
		e.remove(n, parent)
	case *ast.ExprStmt, *ast.DeferStmt, *ast.GoStmt, *ast.SendStmt:
		e.remove(n, parent)
	case *ast.BranchStmt:
		e.swap(n.TokPos, n.Tok, token.CONTINUE, token.BREAK)
	case *ast.IfStmt:
		e.condition(n.Cond)
	case *ast.ForStmt:
		e.condition(n.Cond)
	case *ast.RangeStmt:
		firstBreak := false
		if len(n.Body.List) > 0 {
			if branch, ok := n.Body.List[0].(*ast.BranchStmt); ok {
				firstBreak = branch.Tok == token.BREAK && branch.Label == nil
			}
		}
		if !firstBreak {
			e.add(n.Body.Lbrace+1, n.Body.Lbrace+1, n.Body.Lbrace, "range-break", " break;")
		}
	case *ast.BlockStmt:
		if len(n.List) > 0 {
			e.add(n.Lbrace+1, n.Rbrace, n.Lbrace, "empty-block", "")
		}
	case *ast.CaseClause:
		e.caseBody(n.Colon, n.Body)
	case *ast.CommClause:
		e.caseBody(n.Colon, n.Body)
	case *ast.ReturnStmt:
		e.returns(n, ancestors)
	case *ast.CallExpr:
		e.errorf(n)
	}
	return true
}

func (e *sourceEdits) inScope(start, end token.Pos) bool {
	if end > start {
		end--
	}
	return e.scope.Contains(e.file.Path, e.positions.PositionFor(start, false).Line, e.positions.PositionFor(end, false).Line)
}

func (e *sourceEdits) text(node ast.Node) string {
	return string(e.file.Data[e.positions.Offset(node.Pos()):e.positions.Offset(node.End())])
}

func (e *sourceEdits) add(start, end, anchor token.Pos, kind, replacement string) {
	if !e.inScope(start, end) {
		return
	}
	from, to := e.positions.Offset(start), e.positions.Offset(end)
	original := string(e.file.Data[from:to])
	key := editKey{from, to, replacement}
	if original == replacement || e.added[key] {
		return
	}
	e.added[key] = true
	pos := e.positions.PositionFor(anchor, false)
	e.file.Edits = append(e.file.Edits, Edit{File: e.file.Path, Line: pos.Line, Column: pos.Column,
		Start: from, End: to, Before: original, After: replacement, Kind: kind})
}

func (e *sourceEdits) replace(node ast.Node, kind, replacement string) {
	e.add(node.Pos(), node.End(), node.Pos(), kind, replacement)
}

func (e *sourceEdits) operator(pos token.Pos, op, replacement token.Token) {
	e.add(pos, pos+token.Pos(len(op.String())), pos, "source-operator", replacement.String())
}

func (e *sourceEdits) swap(pos token.Pos, original, first, second token.Token) {
	switch original {
	case first:
		e.operator(pos, original, second)
	case second:
		e.operator(pos, original, first)
	}
}

func (e *sourceEdits) binary(n *ast.BinaryExpr, parent ast.Node) {
	original := operator(n.Op)
	for _, family := range binaryFamilies {
		if !slices.Contains(family, n.Op) {
			continue
		}
		for _, replacement := range family {
			if n.Op == replacement || (n.Op == token.ADD || n.Op == token.SUB) && replacement == token.REM {
				continue
			}
			other := operator(replacement)
			if other != 0 && e.probed[e.positions.Offset(n.Pos())] && (other == original.Boundary() || other == original.Negate()) {
				continue
			}
			edited := *n
			edited.Op = replacement
			// A standalone type check lacks the enclosing constant declaration's iota.
			valid := e.pkg.TypesInfo.Types[n].Value != nil || e.check(n.Pos(), &edited)
			grouped := changesGrouping(n, parent, replacement) || e.joinsNextToken(n, replacement)
			switch {
			case valid && !grouped:
				e.operator(n.OpPos, n.Op, replacement)
			case grouped:
				// Some peers edit source tokens and others print an edited AST.
				e.operator(n.OpPos, n.Op, replacement)
				if valid {
					e.replace(n, "operator-grouping", "(("+e.text(n.X)+") "+replacement.String()+" ("+e.text(n.Y)+"))")
				}
			}
		}
		break
	}
	if n.Op == token.LAND || n.Op == token.LOR {
		e.boolean(n.X, n.Op == token.LAND, "boolean-operand")
		e.boolean(n.Y, n.Op == token.LAND, "boolean-operand")
	}
	if n.Op == token.NEQ && (e.errorAndNil(n.X, n.Y) || e.errorAndNil(n.Y, n.X)) {
		e.boolean(n, false, "error-check")
	}
}

func (e *sourceEdits) joinsNextToken(n *ast.BinaryExpr, replacement token.Token) bool {
	text := replacement.String()
	next := e.positions.Offset(n.OpPos) + len(n.Op.String())
	if next >= len(e.file.Data) {
		return false
	}
	switch [2]byte{text[len(text)-1], e.file.Data[next]} {
	case [2]byte{'+', '+'}, [2]byte{'-', '-'}, [2]byte{'&', '&'}, [2]byte{'&', '^'},
		[2]byte{'|', '|'}, [2]byte{'<', '<'}, [2]byte{'>', '>'}, [2]byte{'<', '-'},
		[2]byte{'/', '*'}, [2]byte{'/', '/'}:
		return true
	}
	return false
}

func changesGrouping(n *ast.BinaryExpr, parent ast.Node, replacement token.Token) bool {
	if n.Op.Precedence() == replacement.Precedence() {
		return false
	}
	priority := replacement.Precedence()
	if left, ok := n.X.(*ast.BinaryExpr); ok && left.Op.Precedence() < priority {
		return true
	}
	if right, ok := n.Y.(*ast.BinaryExpr); ok && right.Op.Precedence() <= priority {
		return true
	}
	if p, ok := parent.(*ast.BinaryExpr); ok {
		return priority < p.Op.Precedence() || p.Y == n && priority == p.Op.Precedence()
	}
	return false
}

func (e *sourceEdits) unary(n *ast.UnaryExpr) {
	switch n.Op {
	case token.NOT:
		// Keep grouping when the removed negation enclosed a comparison.
		e.replace(n, "remove-not", e.text(n.X))
	case token.ADD, token.SUB:
		e.swap(n.OpPos, n.Op, token.ADD, token.SUB)
	case token.XOR:
		edited := *n
		edited.Op = token.AND
		if e.check(n.Pos(), &edited) {
			e.operator(n.OpPos, n.Op, token.AND)
		}
	}
}

func (e *sourceEdits) literal(n *ast.BasicLit) {
	switch n.Kind {
	case token.STRING:
		value, err := strconv.Unquote(n.Value)
		if err == nil {
			if value == "" {
				e.replace(n, "string-literal", `"mutated"`)
			} else {
				e.replace(n, "string-literal", `""`)
			}
		}
	case token.INT:
		value := constant.MakeFromLiteral(n.Value, token.INT, 0)
		for _, delta := range []int64{-1, 1} {
			result := constant.BinaryOp(value, token.ADD, constant.MakeInt64(delta)).ExactString()
			if strings.HasPrefix(result, "-") {
				// A preceding unary minus must not form a decrement token.
				result = "(" + result + ")"
			}
			e.replace(n, "integer-step", result)
		}
	case token.FLOAT:
		value, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			return
		}
		for _, delta := range []float64{-1, 1} {
			result := value + delta
			if math.IsInf(result, 0) || result == value {
				continue
			}
			text := strconv.FormatFloat(result, 'g', -1, 64)
			if !strings.ContainsAny(text, ".eE") {
				text += ".0"
			}
			if strings.HasPrefix(text, "-") {
				text = "(" + text + ")"
			}
			e.replace(n, "float-step", text)
		}
	}
}

func (e *sourceEdits) assign(n *ast.AssignStmt, parent ast.Node) {
	if n.Tok == token.DEFINE {
		return
	}
	e.remove(n, parent)
	if n.Tok == token.ASSIGN {
		replacements := make([]string, len(n.Lhs))
		allBlank := true
		for i, expr := range n.Lhs {
			replacements[i] = "_"
			if name, ok := expr.(*ast.Ident); !ok || name.Name != "_" {
				allBlank = false
			}
		}
		if !allBlank {
			e.add(n.Lhs[0].Pos(), n.Lhs[len(n.Lhs)-1].End(), n.TokPos, "discard-assignment", strings.Join(replacements, ", "))
		}
		return
	}
	for _, family := range compoundFamilies {
		if slices.Contains(family, n.Tok) {
			for _, replacement := range family {
				if replacement != n.Tok && !((n.Tok == token.ADD_ASSIGN || n.Tok == token.SUB_ASSIGN) && replacement == token.REM_ASSIGN) {
					e.operator(n.TokPos, n.Tok, replacement)
				}
			}
			e.operator(n.TokPos, n.Tok, token.ASSIGN)
			break
		}
	}
}

func (e *sourceEdits) remove(node, parent ast.Node) {
	replacement := ""
	if _, labelled := parent.(*ast.LabeledStmt); labelled {
		replacement = ";"
	}
	e.replace(node, "statement-remove", replacement)
}

func (e *sourceEdits) caseBody(colon token.Pos, body []ast.Stmt) {
	if len(body) != 0 {
		e.add(colon+1, body[len(body)-1].End(), colon, "empty-case", " ")
	}
}

func (e *sourceEdits) condition(expr ast.Expr) {
	if expr == nil {
		return
	}
	e.boolean(expr, false, "condition-constant")
	e.boolean(expr, true, "condition-constant")
	e.replace(expr, "condition-negation", "!("+e.text(expr)+")")
}

func (e *sourceEdits) boolean(node ast.Node, value bool, kind string) {
	name := strconv.FormatBool(value)
	text := name
	if !e.nameAvailable(node.Pos(), name) {
		text = "(0 != 0)"
		if value {
			text = "(0 == 0)"
		}
	}
	if expr, ok := node.(ast.Expr); ok {
		if current := e.pkg.TypesInfo.Types[expr].Value; current != nil &&
			current.Kind() == constant.Bool && constant.BoolVal(current) == value {
			return
		}
	}
	e.replace(node, kind, text)
}

func (e *sourceEdits) builtin(id *ast.Ident, name string) bool {
	return id.Name == name && e.pkg.TypesInfo.Uses[id] == types.Universe.Lookup(name)
}

func (e *sourceEdits) nameAvailable(pos token.Pos, name string) bool {
	if scope := e.pkg.Types.Scope().Innermost(pos); scope != nil {
		_, obj := scope.LookupParent(name, pos)
		return obj == types.Universe.Lookup(name)
	}
	return false
}

func (e *sourceEdits) errorAndNil(err, nilExpr ast.Expr) bool {
	id, ok := ast.Unparen(nilExpr).(*ast.Ident)
	typ := e.pkg.TypesInfo.TypeOf(err)
	errorInterface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	return ok && e.builtin(id, "nil") && typ != nil && types.Implements(typ, errorInterface)
}

func (e *sourceEdits) returns(n *ast.ReturnStmt, ancestors []ast.Node) {
	var results *ast.FieldList
	var found bool
	for i := len(ancestors) - 1; i >= 0 && !found; i-- {
		switch fn := ancestors[i].(type) {
		case *ast.FuncDecl:
			results, found = fn.Type.Results, true
		case *ast.FuncLit:
			results, found = fn.Type.Results, true
		}
	}
	if results == nil || results.NumFields() != len(n.Results) {
		return
	}
	offset := 0
	for _, field := range results.List {
		count := max(1, len(field.Names))
		typ := e.pkg.TypesInfo.TypeOf(field.Type)
		for _, result := range n.Results[offset : offset+count] {
			e.returnValue(result, field.Type, typ)
		}
		offset += count
	}
}

func (e *sourceEdits) returnValue(expr, typeExpr ast.Expr, typ types.Type) {
	if typ == nil {
		return
	}
	if basic, ok := typ.Underlying().(*types.Basic); ok {
		switch {
		case basic.Info()&types.IsBoolean != 0:
			e.boolean(expr, true, "return-boolean")
			e.boolean(expr, false, "return-boolean")
			return
		case basic.Info()&types.IsNumeric != 0:
			value := e.pkg.TypesInfo.Types[expr].Value
			if value == nil || !constant.Compare(value, token.EQL, constant.MakeInt64(0)) {
				e.replace(expr, "return-zero", "0")
			}
			return
		case basic.Info()&types.IsString != 0:
			value := e.pkg.TypesInfo.Types[expr].Value
			if value == nil || constant.StringVal(value) != "" {
				e.replace(expr, "return-zero", `""`)
			}
			return
		}
	}
	if _, boxed := typ.Underlying().(*types.Interface); boxed {
		if literal, ok := ast.Unparen(expr).(*ast.BasicLit); ok {
			switch literal.Kind {
			case token.INT, token.FLOAT:
				e.replace(expr, "return-zero", "0")
			case token.STRING:
				e.replace(expr, "return-zero", `""`)
			}
		}
	}
	if types.AssignableTo(types.Typ[types.UntypedNil], typ) && e.nameAvailable(expr.Pos(), "nil") {
		if id, ok := ast.Unparen(expr).(*ast.Ident); !ok || !e.builtin(id, "nil") {
			e.replace(expr, "return-nil", "nil")
		}
		return
	}
	if composite, ok := ast.Unparen(expr).(*ast.CompositeLit); ok && len(composite.Elts) == 0 &&
		!types.AssignableTo(types.Typ[types.UntypedNil], typ) {
		return
	}
	zero := "*new(" + e.text(typeExpr) + ")"
	if e.nameAvailable(expr.Pos(), "new") {
		e.replace(expr, "return-zero", zero)
		return
	}
	e.replace(expr, "return-zero", "(func() (_ "+e.text(typeExpr)+") { return })()")
}

func (e *sourceEdits) errorf(n *ast.CallExpr) {
	var name string
	switch fn := n.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	if name != "Errorf" {
		return
	}
	for _, arg := range n.Args {
		literal, ok := arg.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			continue
		}
		for i := 0; i+1 < len(literal.Value); i++ {
			if literal.Value[i] != '%' {
				continue
			}
			if literal.Value[i+1] == 'w' {
				pos := literal.Pos() + token.Pos(i)
				e.add(pos, pos+2, pos, "errorf-unwrap", "%v")
			}
			i++
		}
		break
	}
}

func (e *sourceEdits) check(pos token.Pos, expression ast.Expr) bool {
	return types.CheckExpr(e.pkg.Fset, e.pkg.Types, pos, expression, nil) == nil
}
