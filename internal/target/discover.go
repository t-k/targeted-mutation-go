package target

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/t-k/targeted-mutation-go/internal/analysis"
	"golang.org/x/tools/go/packages"
)

type Span struct{ Start, End int }

type Scope map[string][]Span

func (scope Scope) Contains(file string, start, end int) bool {
	if scope == nil {
		return true
	}
	for _, span := range scope[file] {
		if span.Start <= end && start <= span.End {
			return true
		}
	}
	return false
}

type Package struct {
	Path, Dir, Name, Prefix string
	Files                   []*File
	Paths                   []string
}

type File struct {
	Path  string
	Data  []byte
	Sites []*Site
	Edits []Edit
}

type Site struct {
	ID                      int
	File                    string
	Line, Column            int
	Start, End              int
	Left, Right, Expression string
	Op                      analysis.Operator
	Domain                  *analysis.Domain
	Callable                string
}

type Edit struct {
	File                string
	Line, Column        int
	Start, End          int
	Before, After, Kind string
}

func Discover(ctx context.Context, dir string, patterns []string, tags string, scope Scope, full bool) ([]Package, error) {
	var contents sync.Map
	cfg := &packages.Config{
		ParseFile: func(fset *token.FileSet, path string, data []byte) (*ast.File, error) {
			contents.Store(path, data)
			return parser.ParseFile(fset, path, data, parser.ParseComments|parser.SkipObjectResolution)
		},
		Context: ctx,
		Dir:     dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo |
			packages.NeedTypesSizes | packages.NeedModule,
	}
	if tags != "" {
		cfg.BuildFlags = []string{"-tags=" + tags}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load Go packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages match %s", strings.Join(patterns, " "))
	}
	slices.SortFunc(pkgs, func(a, b *packages.Package) int { return strings.Compare(a.PkgPath, b.PkgPath) })
	var results []Package
	for _, pkg := range pkgs {
		if len(pkg.Errors) != 0 {
			return nil, fmt.Errorf("load %s: %s", pkg.PkgPath, pkg.Errors[0])
		}
		if len(pkg.GoFiles) == 0 {
			if pkg.Dir != "" {
				results = append(results, Package{Path: pkg.PkgPath, Dir: pkg.Dir, Name: pkg.Name})
			}
			continue
		}
		p := Package{Path: pkg.PkgPath, Name: pkg.Name, Dir: pkg.Dir, Paths: pkg.GoFiles}
		hash := sha256.Sum256([]byte(pkg.PkgPath))
		p.Prefix = fmt.Sprintf("tmtGo%x", hash[:6])
		if pkg.Module != nil && !pkg.Module.Main {
			return nil, fmt.Errorf("package %s is outside the main module or workspace", pkg.PkgPath)
		}
		portable := (pkg.Module == nil || pkg.Module.GoVersion == "" || versionSupportsProbes(pkg.Module.GoVersion)) && runtimeNamesAvailable(pkg.Types.Scope())
		slices.SortFunc(pkg.Syntax, func(a, b *ast.File) int {
			return strings.Compare(pkg.Fset.PositionFor(a.Pos(), false).Filename, pkg.Fset.PositionFor(b.Pos(), false).Filename)
		})
		for _, syntax := range pkg.Syntax {
			astFile := pkg.Fset.File(syntax.Pos())
			path := astFile.Name()
			if !slices.Contains(pkg.GoFiles, path) || ast.IsGenerated(syntax) {
				continue
			}
			snapshot, ok := contents.Load(path)
			if !ok {
				return nil, fmt.Errorf("missing parsed source for %s", path)
			}
			data := snapshot.([]byte)
			file := &File{Path: path, Data: data}
			findSites(file, syntax, astFile, pkg, scope, full, portable)
			if len(file.Sites)+len(file.Edits) > 0 {
				p.Files = append(p.Files, file)
			}
		}
		for _, path := range pkg.GoFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			for bytes.Contains(data, []byte(p.Prefix)) {
				p.Prefix += "X"
			}
		}
		results = append(results, p)
	}
	return results, nil
}

func versionSupportsProbes(v string) bool {
	var major, minor int
	_, _ = fmt.Sscanf(v, "%d.%d", &major, &minor)
	return major > 1 || major == 1 && minor >= 18
}

func findSites(file *File, tree *ast.File, positions *token.File, pkg *packages.Package, scope Scope, full, probes bool) {
	text := func(node ast.Node) string {
		return string(file.Data[positions.Offset(node.Pos()):positions.Offset(node.End())])
	}
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || hasCompilerDirective(fn) {
			continue
		}
		findFunctionSites(file, fn, positions, pkg, scope, full, probes, text)
	}
}

func findFunctionSites(file *File, fn *ast.FuncDecl, positions *token.File, pkg *packages.Package, scope Scope, full, probes bool, text func(ast.Node) string) {
	var stack []ast.Node
	safety := make(map[ast.Stmt]bool)
	ast.Inspect(fn.Body, func(node ast.Node) (visit bool) {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		defer func() {
			if visit {
				stack = append(stack, node)
			}
		}()
		if expr, ok := node.(ast.Expr); ok && pkg.TypesInfo.Types[expr].Value != nil {
			return false
		}
		switch expr := node.(type) {
		case *ast.BinaryExpr:
			pos := positions.PositionFor(expr.OpPos, false)
			start := positions.PositionFor(expr.Pos(), false)
			end := positions.PositionFor(expr.End()-1, false)
			if !scope.Contains(file.Path, start.Line, end.Line) {
				return true
			}
			op := operator(expr.Op)
			if probes && op != 0 && safeStatement(stack, pkg.TypesInfo, safety) && validOperand(expr.X) && validOperand(expr.Y) && pkg.TypesInfo.Types[expr].Value == nil {
				site := &Site{
					File: file.Path, Line: pos.Line, Column: pos.Column,
					Start: start.Offset, End: positions.Offset(expr.End()), Op: op,
					Left: text(expr.X), Right: text(expr.Y), Expression: text(expr),
				}
				if makeIntegerSite(site, expr, fn, pkg) {
					file.Sites = append(file.Sites, site)
					return false
				}
			}
			if full {
				appendEdits(file, positions, expr.OpPos, expr.Op)
			}
		}
		return true
	})
}

func safeStatement(ancestors []ast.Node, info *types.Info, cache map[ast.Stmt]bool) bool {
	for i := len(ancestors) - 1; i >= 0; i-- {
		stmt, ok := ancestors[i].(ast.Stmt)
		if !ok {
			continue
		}
		if value, checked := cache[stmt]; checked {
			return value
		}
		value := safeEvaluation(stmt, info)
		cache[stmt] = value
		return value
	}
	return false
}

func safeEvaluation(stmt ast.Stmt, info *types.Info) bool {
	var nodes []ast.Node
	switch s := stmt.(type) {
	case *ast.IfStmt:
		nodes = []ast.Node{s.Cond}
	case *ast.ForStmt:
		if s.Cond != nil {
			nodes = []ast.Node{s.Cond}
		}
	case *ast.SwitchStmt:
		if s.Tag != nil {
			nodes = []ast.Node{s.Tag}
		}
	case *ast.CaseClause:
		for _, expr := range s.List {
			nodes = append(nodes, expr)
		}
	case *ast.RangeStmt:
		blank, isIdent := s.Value.(*ast.Ident)
		if s.Value == nil || isIdent && blank.Name == "_" {
			typ := info.TypeOf(s.X)
			if typ == nil {
				return false
			}
			if _, parameter := typ.(*types.TypeParam); parameter {
				return false
			}
			kind := typ.Underlying()
			if pointer, ok := kind.(*types.Pointer); ok {
				kind = pointer.Elem().Underlying()
			}
			if _, array := kind.(*types.Array); array {
				return false
			}
		}
		nodes = []ast.Node{s.X}
	case *ast.ReturnStmt, *ast.AssignStmt, *ast.DeclStmt:
		nodes = []ast.Node{stmt}
	default:
		return false
	}
	safe := true
	for _, node := range nodes {
		ast.Inspect(node, func(node ast.Node) bool {
			if expression, ok := node.(ast.Expr); ok && info.Types[expression].Value != nil {
				return false
			}
			switch child := node.(type) {
			case *ast.FuncLit:
				return false
			case *ast.CallExpr:
				// A conversion is not a function evaluation.
				safe = safe && info.Types[child.Fun].IsType()
			case *ast.UnaryExpr:
				safe = safe && child.Op != token.ARROW
			}
			return safe
		})
	}
	return safe
}

func runtimeNamesAvailable(scope *types.Scope) bool {
	for _, name := range strings.Fields("int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr bool nil len panic") {
		if scope.Lookup(name) != nil {
			return false
		}
	}
	return true
}

func hasCompilerDirective(fn *ast.FuncDecl) bool {
	if fn.Doc != nil {
		for _, comment := range fn.Doc.List {
			if strings.HasPrefix(comment.Text, "//go:") {
				return true
			}
		}
	}
	return false
}

func validOperand(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident, *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return validOperand(e.X)
	case *ast.SelectorExpr:
		return validOperand(e.X)
	case *ast.IndexExpr:
		return validOperand(e.X) && validOperand(e.Index)
	case *ast.StarExpr:
		return validOperand(e.X)
	case *ast.UnaryExpr:
		return (e.Op == token.ADD || e.Op == token.SUB || e.Op == token.XOR) && validOperand(e.X)
	case *ast.BinaryExpr:
		return e.Op >= token.ADD && e.Op <= token.AND_NOT && validOperand(e.X) && validOperand(e.Y)
	}
	return false
}

func makeIntegerSite(site *Site, expr *ast.BinaryExpr, fn *ast.FuncDecl, pkg *packages.Package) bool {
	left, right := pkg.TypesInfo.Types[expr.X], pkg.TypesInfo.Types[expr.Y]
	if left.Type == nil || right.Type == nil || left.Value != nil && right.Value != nil {
		return false
	}
	variable := expr.X
	if left.Value != nil {
		left, right = right, left
		site.Left, site.Right = site.Right, site.Left
		site.Op = site.Op.Reverse()
		variable = expr.Y
	}
	basic, ok := left.Type.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 || basic.Info()&types.IsUntyped != 0 {
		return false
	}
	if right.Value == nil {
		return types.Identical(left.Type, right.Type)
	}
	if right.Value.Kind() != constant.Int {
		return false
	}
	if rightBasic, ok := right.Type.Underlying().(*types.Basic); !ok ||
		(rightBasic.Info()&types.IsUntyped == 0 && !types.Identical(left.Type, right.Type)) {
		return false
	}
	bits := pkg.TypesSizes.Sizeof(left.Type) * 8
	if bits <= 0 || bits > 64 {
		return false
	}
	if basic.Info()&types.IsUnsigned != 0 {
		value, ok := constant.Uint64Val(right.Value)
		if !ok {
			return false
		}
		site.Domain = &analysis.Domain{High: math.MaxUint64 >> (64 - bits), Pivot: value}
	} else {
		value, ok := constant.Int64Val(right.Value)
		if !ok {
			return false
		}
		site.Domain = &analysis.Domain{
			Low: analysis.RankSigned(math.MinInt64 >> (64 - bits)), High: analysis.RankSigned(math.MaxInt64 >> (64 - bits)),
			Pivot: analysis.RankSigned(value), Signed: true,
		}
	}
	if id, ok := ast.Unparen(variable).(*ast.Ident); ok && directReturn(fn, expr) {
		param := fn.Type.Params.List[0].Names[0]
		if pkg.TypesInfo.Uses[id] == pkg.TypesInfo.Defs[param] {
			site.Callable = fn.Name.Name
		}
	}
	return true
}

func directReturn(fn *ast.FuncDecl, expr *ast.BinaryExpr) bool {
	if fn.Recv != nil || fn.Type.TypeParams != nil || len(fn.Type.Params.List) != 1 ||
		len(fn.Type.Params.List[0].Names) != 1 || len(fn.Body.List) != 1 {
		return false
	}
	statement, ok := fn.Body.List[0].(*ast.ReturnStmt)
	return ok && len(statement.Results) == 1 && ast.Unparen(statement.Results[0]) == expr
}

func operator(op token.Token) analysis.Operator {
	switch op {
	case token.LSS:
		return analysis.LT
	case token.LEQ:
		return analysis.LE
	case token.GTR:
		return analysis.GT
	case token.GEQ:
		return analysis.GE
	case token.EQL:
		return analysis.EQ
	case token.NEQ:
		return analysis.NE
	}
	return 0
}

func appendEdits(file *File, positions *token.File, at token.Pos, op token.Token) {
	var replacements []token.Token
	if cmp := operator(op); cmp != 0 {
		for _, opt := range []analysis.Operator{cmp.Boundary(), cmp.Negate()} {
			if opt != 0 {
				replacements = append(replacements, [...]token.Token{0, token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ}[opt])
			}
		}
	} else {
		switch op {
		case token.LAND:
			replacements = []token.Token{token.LOR}
		case token.LOR:
			replacements = []token.Token{token.LAND}
		case token.ADD:
			replacements = []token.Token{token.SUB}
		case token.SUB:
			replacements = []token.Token{token.ADD}
		case token.MUL:
			replacements = []token.Token{token.QUO}
		case token.QUO:
			replacements = []token.Token{token.MUL}
		}
	}
	pos := positions.PositionFor(at, false)
	for _, replacement := range replacements {
		file.Edits = append(file.Edits, Edit{
			File: file.Path, Line: pos.Line, Column: pos.Column, Start: pos.Offset, End: pos.Offset + len(op.String()),
			Before: op.String(), After: replacement.String(), Kind: "source-operator",
		})
	}
}
