package target

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"
)

type semanticCallee struct {
	name      *ast.Ident
	qualified bool
	function  *types.Func
}

func (e *sourceEdits) semanticCall(call *ast.CallExpr) {
	if e.rawSource {
		return
	}
	expr := ast.Unparen(call.Fun)
	switch index := expr.(type) {
	case *ast.IndexExpr:
		expr = ast.Unparen(index.X)
	case *ast.IndexListExpr:
		expr = ast.Unparen(index.X)
	}
	info := e.pkg.TypesInfo
	var callee semanticCallee
	switch fn := expr.(type) {
	case *ast.Ident:
		if (fn.Name == "min" || fn.Name == "max") && e.builtin(fn, fn.Name) {
			// A different constant result may overflow an enclosing conversion.
			if info.Types[call].Value == nil {
				e.swapSemanticName(semanticCallee{name: fn}, otherName(fn.Name, "min", "max"), "min-max")
			}
			return
		}
		callee.name = fn
	case *ast.SelectorExpr:
		qualifier, ok := fn.X.(*ast.Ident)
		if !ok {
			return
		}
		if _, ok := info.Uses[qualifier].(*types.PkgName); !ok {
			return
		}
		callee = semanticCallee{name: fn.Sel, qualified: true}
	default:
		return
	}
	callee.function, _ = info.Uses[callee.name].(*types.Func)
	if callee.function == nil || callee.function.Pkg() == nil {
		return
	}
	name := callee.name.Name
	switch callee.function.Pkg().Path() {
	case "math":
		switch name {
		case "Min", "Max":
			e.swapSemanticName(callee, otherName(name, "Min", "Max"), "min-max")
		case "Floor", "Ceil", "Round", "RoundToEven", "Trunc":
			for _, alternative := range []string{"Floor", "Ceil", "Round", "RoundToEven", "Trunc"} {
				if alternative != name {
					e.swapSemanticName(callee, alternative, "rounding-mode")
				}
			}
		}
	case "strings", "bytes":
		e.semanticTextCall(call, callee)
	case "slices":
		switch name {
		case "Min", "Max":
			e.swapSemanticName(callee, otherName(name, "Min", "Max"), "min-max")
		case "MinFunc", "MaxFunc":
			e.swapSemanticName(callee, otherName(name, "MinFunc", "MaxFunc"), "min-max")
		case "Contains", "Index":
			if len(call.Args) == 2 {
				e.pruneLiteralSlice(call.Args[0])
			}
		}
	}
}

func (e *sourceEdits) semanticTextCall(call *ast.CallExpr, callee semanticCallee) {
	name := callee.name.Name
	switch name {
	case "HasPrefix", "HasSuffix":
		e.swapSemanticName(callee, "Contains", "affix-contains")
	case "TrimPrefix", "TrimSuffix":
		e.swapSemanticName(callee, otherName(name, "TrimPrefix", "TrimSuffix"), "affix-end")
	case "CutPrefix", "CutSuffix":
		e.swapSemanticName(callee, otherName(name, "CutPrefix", "CutSuffix"), "affix-end")
	case "Index", "LastIndex":
		e.swapSemanticName(callee, otherName(name, "Index", "LastIndex"), "search-direction")
	case "IndexByte", "LastIndexByte":
		e.swapSemanticName(callee, otherName(name, "IndexByte", "LastIndexByte"), "search-direction")
	case "IndexAny", "LastIndexAny":
		e.swapSemanticName(callee, otherName(name, "IndexAny", "LastIndexAny"), "search-direction")
	case "Replace", "ReplaceAll":
		e.changeReplacementCount(call, callee)
	}
}

func otherName(name, first, second string) string {
	if name == first {
		return second
	}
	return first
}

func (e *sourceEdits) canNameAlternative(callee semanticCallee, name string) bool {
	if callee.function == nil {
		return e.nameAvailable(callee.name.Pos(), name)
	}
	alternative, ok := callee.function.Pkg().Scope().Lookup(name).(*types.Func)
	if !ok {
		return false
	}
	if callee.qualified {
		return true
	}
	// A dot import can be shadowed independently at either name.
	scope := e.pkg.Types.Scope().Innermost(callee.name.Pos())
	if scope == nil {
		return false
	}
	_, visible := scope.LookupParent(name, callee.name.Pos())
	return visible == alternative
}

func (e *sourceEdits) swapSemanticName(callee semanticCallee, name, rule string) {
	if e.canNameAlternative(callee, name) {
		e.replace(callee.name, rule, name)
	}
}

func (e *sourceEdits) changeReplacementCount(call *ast.CallExpr, callee semanticCallee) {
	const rule = "replacement-count"
	name := otherName(callee.name.Name, "Replace", "ReplaceAll")
	if name == "ReplaceAll" {
		if len(call.Args) != 4 {
			return
		}
		count := e.pkg.TypesInfo.Types[call.Args[3]].Value
		if count == nil || !constant.Compare(count, token.EQL, constant.MakeInt64(1)) {
			return
		}
	} else if len(call.Args) != 3 {
		return
	}
	if !e.canNameAlternative(callee, name) {
		return
	}
	offset := e.positions.Offset
	start, end := offset(call.Pos()), offset(call.End())
	nameStart, nameEnd := offset(callee.name.Pos()), offset(callee.name.End())
	argsEnd := offset(call.Args[2].End())
	data := e.file.Data
	prefix := string(data[start:nameStart]) + name + string(data[nameEnd:argsEnd])
	if name == "ReplaceAll" {
		e.replace(call, rule, prefix+")")
	} else {
		e.replace(call, rule, prefix+", 1"+string(data[argsEnd:end]))
	}
}

func (e *sourceEdits) pruneLiteralSlice(argument ast.Expr) {
	literal, ok := ast.Unparen(argument).(*ast.CompositeLit)
	if !ok || literal.Type == nil || len(literal.Elts) < 2 || len(literal.Elts) > 8 {
		return
	}
	info := e.pkg.TypesInfo
	typ := info.TypeOf(literal)
	if typ == nil {
		return
	}
	slice, ok := typ.Underlying().(*types.Slice)
	if !ok {
		return
	}
	if _, ok := slice.Elem().Underlying().(*types.Basic); !ok {
		return
	}
	var values []string
	unique := make(map[string]bool)
	for _, element := range literal.Elts {
		if !e.scalarLiteral(element) {
			return
		}
		value := info.Types[element].Value
		if value == nil {
			return
		}
		key := value.ExactString()
		if unique[key] {
			return
		}
		unique[key] = true
		values = append(values, e.text(element))
	}
	for i := range values {
		remaining := append(append(make([]string, 0, len(values)-1), values[:i]...), values[i+1:]...)
		e.replace(literal, "slice-element-remove", e.text(literal.Type)+"{"+strings.Join(remaining, ", ")+"}")
	}
}

func (e *sourceEdits) scalarLiteral(expr ast.Expr) bool {
	switch element := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return true
	case *ast.UnaryExpr:
		_, ok := ast.Unparen(element.X).(*ast.BasicLit)
		return ok && (element.Op == token.ADD || element.Op == token.SUB)
	case *ast.Ident:
		return (element.Name == "true" || element.Name == "false") && e.builtin(element, element.Name)
	}
	return false
}

func (e *sourceEdits) integerRounding(expr *ast.BinaryExpr) {
	if e.rawSource || expr.Op != token.QUO {
		return
	}
	product, ok := ast.Unparen(expr.X).(*ast.BinaryExpr)
	if !ok || product.Op != token.MUL {
		return
	}
	value := e.pkg.TypesInfo.Types[expr]
	if value.Type == nil || value.Value != nil {
		return
	}
	basic, ok := value.Type.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 {
		return
	}
	for _, arg := range []ast.Expr{product.X, product.Y, expr.Y} {
		switch ast.Unparen(arg).(type) {
		case *ast.Ident, *ast.BasicLit:
		default:
			if !e.scalarLiteral(arg) {
				return
			}
		}
	}
	divide := func(left, right ast.Expr) *ast.BinaryExpr {
		return &ast.BinaryExpr{X: left, Y: right, Op: token.QUO, OpPos: expr.OpPos}
	}
	multiply := func(left, right ast.Expr) *ast.BinaryExpr {
		return &ast.BinaryExpr{X: left, Y: right, Op: token.MUL, OpPos: product.OpPos}
	}
	left, right, divisor := e.text(product.X), e.text(product.Y), e.text(expr.Y)
	alternatives := []struct {
		expression *ast.BinaryExpr
		source     string
	}{
		{multiply(divide(product.X, expr.Y), product.Y), "((" + left + " / " + divisor + ") * " + right + ")"},
		{multiply(product.X, divide(product.Y, expr.Y)), "(" + left + " * (" + right + " / " + divisor + "))"},
	}
	for _, candidate := range alternatives {
		info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
		if err := types.CheckExpr(e.pkg.Fset, e.pkg.Types, expr.Pos(), candidate.expression, info); err == nil &&
			types.Identical(info.TypeOf(candidate.expression), value.Type) {
			e.replace(expr, "integer-rounding-order", candidate.source)
		}
	}
}
