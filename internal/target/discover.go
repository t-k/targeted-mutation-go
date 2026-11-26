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
	"path/filepath"
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
	Path    string
	Data    []byte
	Sites   []*Site
	Edits   []Edit
	Owners  []Declaration
	Ignored map[int]string
}

type Declaration struct {
	Start, End int
	Name       string
}

type Site struct {
	ID                      int
	File                    string
	Line, Column            int
	StartLine, EndLine      int
	Start, End, OperatorAt  int
	LiteralAt               int
	Left, Right, Expression string
	Op                      analysis.Operator
	Domain                  *analysis.Domain
	Callable                string
	Arguments               []string
	InputArgument           int
	Reversed, String        bool
	Float, Margin           bool
	Arith                   bool
	Bool                    bool
	ArithOp                 uint64
	CanDropOperands         bool
}

func (s *Site) IsSchemata() bool {
	return s != nil && (s.Arith || s.Bool)
}

type Edit struct {
	File                string
	Line, Column        int
	StartLine, EndLine  int
	Start, End          int
	Before, After, Kind string
	MissingReturn       bool
	Site                *Site
	Operator            analysis.Operator
	Pivot               uint64
	Shift               int
	ArithOp             uint64
}

type Discovery struct {
	Full, SourceOnly, IncludeGenerated bool
}

func Discover(ctx context.Context, dir string, patterns []string, tags string, scope Scope, options Discovery) ([]Package, error) {
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
		cgoWithoutTypes := false
		if len(pkg.Errors) != 0 {
			cgoWithoutTypes = slices.ContainsFunc(pkg.Errors, func(issue packages.Error) bool {
				return issue.Kind == packages.TypeError && strings.Contains(issue.Msg, "could not import C (no metadata for C)")
			})
			if !cgoWithoutTypes || slices.ContainsFunc(pkg.Errors, func(issue packages.Error) bool {
				return issue.Kind != packages.TypeError
			}) {
				return nil, fmt.Errorf("load %s: %s", pkg.PkgPath, pkg.Errors[0])
			}
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
		_, probeOrderOK := syntheticProbeFileName(p.Prefix, p.Paths)
		testDecls := samePackageTestDecls(pkg.Dir, pkg.Name)
		portable := !options.SourceOnly && !cgoWithoutTypes && probeOrderOK &&
			(pkg.Module == nil || pkg.Module.GoVersion == "" || versionSupportsProbes(pkg.Module.GoVersion)) &&
			runtimeNamesAvailable(pkg.Types.Scope(), testDecls)
		slices.SortFunc(pkg.Syntax, func(a, b *ast.File) int {
			return strings.Compare(pkg.Fset.PositionFor(a.Pos(), false).Filename, pkg.Fset.PositionFor(b.Pos(), false).Filename)
		})
		typed := make(map[string]bool)
		for _, syntax := range pkg.Syntax {
			astFile := pkg.Fset.File(syntax.Pos())
			path := astFile.Name()
			if !slices.Contains(pkg.GoFiles, path) {
				continue
			}
			typed[path] = true
			if !options.IncludeGenerated && ast.IsGenerated(syntax) {
				continue
			}
			snapshot, ok := contents.Load(path)
			if !ok {
				return nil, fmt.Errorf("missing parsed source for %s", path)
			}
			data := snapshot.([]byte)
			file := &File{
				Path:    path,
				Data:    data,
				Owners:  declarationOwners(syntax, astFile, data),
				Ignored: ParseIgnoreComments(pkg.Fset, syntax, data),
			}
			if portable {
				findSites(file, syntax, astFile, pkg, scope, options.Full, testDecls)
			}
			if options.Full || options.SourceOnly {
				discoverSourceEdits(file, syntax, astFile, pkg, scope, cgoWithoutTypes)
			}
			if len(file.Sites)+len(file.Edits) > 0 {
				p.Files = append(p.Files, file)
			}
		}
		for _, path := range pkg.GoFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			if !typed[path] && (options.Full || options.SourceOnly) {
				fset := token.NewFileSet()
				syntax, err := parser.ParseFile(fset, path, data, parser.ParseComments|parser.SkipObjectResolution)
				if err != nil {
					return nil, fmt.Errorf("parse original source %s: %w", path, err)
				}
				if options.IncludeGenerated || !ast.IsGenerated(syntax) {
					file := &File{
						Path:    path,
						Data:    data,
						Owners:  declarationOwners(syntax, fset.File(syntax.Pos()), data),
						Ignored: ParseIgnoreComments(fset, syntax, data),
					}
					discoverSourceEdits(file, syntax, fset.File(syntax.Pos()), pkg, scope, true)
					if len(file.Edits) > 0 {
						p.Files = append(p.Files, file)
					}
				}
			}
			for bytes.Contains(data, []byte(p.Prefix)) {
				p.Prefix += "X"
			}
		}
		results = append(results, p)
	}
	return results, nil
}

func declarationOwners(tree *ast.File, positions *token.File, data []byte) []Declaration {
	var owners []Declaration
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) != 0 {
			typ := fn.Recv.List[0].Type
			receiver := data[positions.Offset(typ.Pos()):positions.Offset(typ.End())]
			name = strings.Join(strings.Fields(string(receiver)), "") + "." + name
		}
		owners = append(owners, Declaration{
			Start: positions.PositionFor(fn.Pos(), false).Line,
			End:   positions.PositionFor(fn.End()-1, false).Line,
			Name:  name,
		})
	}
	return owners
}

func versionSupportsProbes(v string) bool {
	var major, minor int
	_, _ = fmt.Sscanf(v, "%d.%d", &major, &minor)
	return major > 1 || major == 1 && minor >= 18
}

func findSites(file *File, tree *ast.File, positions *token.File, pkg *packages.Package, scope Scope, full bool, testDecls map[string]bool) {
	text := func(node ast.Node) string {
		return string(file.Data[positions.Offset(node.Pos()):positions.Offset(node.End())])
	}
	stringsOK := full && pkg.Types.Scope().Lookup("string") == nil && !testDecls["string"]
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || hasCompilerDirective(fn) {
			continue
		}
		findFunctionSites(file, fn, positions, pkg, scope, full, stringsOK, text)
	}
}

func findFunctionSites(file *File, fn *ast.FuncDecl, positions *token.File, pkg *packages.Package, scope Scope, full, stringsOK bool, text func(ast.Node) string) {
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
			if full {
				if ident, ok := expr.(*ast.Ident); ok && (ident.Name == "true" || ident.Name == "false") &&
					pkg.TypesInfo.Uses[ident] == types.Universe.Lookup(ident.Name) &&
					types.Identical(pkg.TypesInfo.TypeOf(ident), types.Typ[types.Bool]) &&
					safeStatement(stack, pkg.TypesInfo, safety) && !inCaseOrKey(ident, stack) {
					pos := positions.PositionFor(ident.Pos(), false)
					end := positions.PositionFor(ident.End()-1, false)
					if scope.Contains(file.Path, pos.Line, end.Line) {
						file.Sites = append(file.Sites, &Site{
							File: file.Path, Line: pos.Line, Column: pos.Column,
							StartLine: pos.Line, EndLine: end.Line,
							Start: pos.Offset, End: positions.Offset(ident.End()), OperatorAt: pos.Offset,
							Expression: text(ident), Bool: true, CanDropOperands: true,
						})
					}
				}
			}
			return false
		}
		switch expr := node.(type) {
		case *ast.UnaryExpr:
			if full && expr.Op == token.NOT &&
				types.Identical(pkg.TypesInfo.TypeOf(expr), types.Typ[types.Bool]) &&
				safeStatement(stack, pkg.TypesInfo, safety) && !inCaseOrKey(expr, stack) &&
				!hasNestedComparison(expr) {
				pos := positions.PositionFor(expr.OpPos, false)
				start := positions.PositionFor(expr.Pos(), false)
				end := positions.PositionFor(expr.End()-1, false)
				if scope.Contains(file.Path, start.Line, end.Line) {
					file.Sites = append(file.Sites, &Site{
						File: file.Path, Line: pos.Line, Column: pos.Column,
						StartLine: start.Line, EndLine: end.Line,
						Start: start.Offset, End: positions.Offset(expr.End()), OperatorAt: pos.Offset,
						Expression: text(expr), Bool: true,
						CanDropOperands: parameterOrLiteralOrNil(expr.X, fn, pkg.TypesInfo),
					})
					return false
				}
			}
		case *ast.BinaryExpr:
			pos := positions.PositionFor(expr.OpPos, false)
			start := positions.PositionFor(expr.Pos(), false)
			end := positions.PositionFor(expr.End()-1, false)
			if !scope.Contains(file.Path, start.Line, end.Line) {
				return true
			}
			op := operator(expr.Op)
			if op != 0 && safeStatement(stack, pkg.TypesInfo, safety) && validOperand(expr.X, pkg.TypesInfo) && validOperand(expr.Y, pkg.TypesInfo) {
				site := &Site{
					File: file.Path, Line: pos.Line, Column: pos.Column,
					StartLine: start.Line, EndLine: end.Line,
					Start: start.Offset, End: positions.Offset(expr.End()), OperatorAt: pos.Offset, Op: op,
					Left: text(expr.X), Right: text(expr.Y), Expression: text(expr),
				}
				site.String = stringsOK && stringOperands(expr, pkg.TypesInfo)
				if site.String || makeIntegerSite(site, expr, fn, pkg) || makeFloatSite(site, expr, pkg.TypesInfo) {
					if full && site.Domain != nil {
						constantOperand := expr.Y
						if site.Reversed {
							constantOperand = expr.X
						}
						if literal, ok := ast.Unparen(constantOperand).(*ast.BasicLit); ok && literal.Kind == token.INT {
							site.LiteralAt = positions.Offset(literal.Pos())
						}
					}
					site.CanDropOperands = full && parameterOrLiteral(expr.X, fn, pkg.TypesInfo) && parameterOrLiteral(expr.Y, fn, pkg.TypesInfo)
					file.Sites = append(file.Sites, site)
					return false
				}
				if full && (expr.Op == token.EQL || expr.Op == token.NEQ) &&
					types.Identical(pkg.TypesInfo.TypeOf(expr), types.Typ[types.Bool]) &&
					!inCaseOrKey(expr, stack) && !hasNestedComparison(expr.X) && !hasNestedComparison(expr.Y) {
					site.Op = 0
					site.Bool = true
					site.CanDropOperands = parameterOrLiteralOrNil(expr.X, fn, pkg.TypesInfo) && parameterOrLiteralOrNil(expr.Y, fn, pkg.TypesInfo)
					file.Sites = append(file.Sites, site)
					return false
				}
			} else if full {
				if code := arithCode(expr.Op); code != 0 && safeStatement(stack, pkg.TypesInfo, safety) &&
					validOperand(expr.X, pkg.TypesInfo) && validOperand(expr.Y, pkg.TypesInfo) &&
					makeArithSite(expr, pkg.TypesInfo) {
					site := &Site{
						File: file.Path, Line: pos.Line, Column: pos.Column,
						StartLine: start.Line, EndLine: end.Line,
						Start: start.Offset, End: positions.Offset(expr.End()), OperatorAt: pos.Offset,
						Left: text(expr.X), Right: text(expr.Y), Expression: text(expr),
						Arith: true, ArithOp: code,
					}
					file.Sites = append(file.Sites, site)
					return false
				}
			}
		}
		return true
	})
}

func arithCode(tok token.Token) uint64 {
	switch tok {
	case token.ADD:
		return 1
	case token.SUB:
		return 2
	case token.MUL:
		return 3
	case token.QUO:
		return 4
	case token.REM:
		return 5
	case token.AND:
		return 6
	case token.OR:
		return 7
	case token.XOR:
		return 8
	case token.AND_NOT:
		return 9
	}
	return 0
}

func makeArithSite(expr *ast.BinaryExpr, info *types.Info) bool {
	resType := info.TypeOf(expr)
	if resType == nil {
		return false
	}
	if _, tp := resType.(*types.TypeParam); tp {
		return false
	}
	basic, ok := resType.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 || basic.Info()&types.IsUntyped != 0 {
		return false
	}
	xType, yType := info.TypeOf(expr.X), info.TypeOf(expr.Y)
	if xType == nil || yType == nil {
		return false
	}
	xConcrete := types.Identical(xType, resType) && info.Types[expr.X].Value == nil && !hasContextualShift(expr.X, info)
	yConcrete := types.Identical(yType, resType) && info.Types[expr.Y].Value == nil && !hasContextualShift(expr.Y, info)
	xOK := xConcrete || info.Types[expr.X].Value != nil
	yOK := yConcrete || info.Types[expr.Y].Value != nil
	if !xOK || !yOK || (!xConcrete && !yConcrete) {
		return false
	}
	if hasContextualShift(expr.X, info) || hasContextualShift(expr.Y, info) {
		return false
	}
	// Do not shadow any nested comparison expression.
	hasComparison := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && operator(b.Op) != 0 {
			hasComparison = true
			return false
		}
		return !hasComparison
	})
	return !hasComparison
}

func hasContextualShift(expr ast.Expr, info *types.Info) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		b, ok := n.(*ast.BinaryExpr)
		if ok && (b.Op == token.SHL || b.Op == token.SHR) {
			if tv, ok := info.Types[b.X]; ok && tv.Value != nil {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func parameterOrLiteralOrNil(expr ast.Expr, fn *ast.FuncDecl, info *types.Info) bool {
	if ident, ok := ast.Unparen(expr).(*ast.Ident); ok && info.Uses[ident] == types.Universe.Lookup("nil") {
		return true
	}
	return parameterOrLiteral(expr, fn, info)
}

func inCaseOrKey(current ast.Node, ancestors []ast.Node) bool {
	child := current
	for i := len(ancestors) - 1; i >= 0; i-- {
		switch a := ancestors[i].(type) {
		case *ast.CaseClause:
			for _, item := range a.List {
				if item == child {
					return true
				}
			}
			return false
		case *ast.KeyValueExpr:
			if a.Key == child {
				return true
			}
		case ast.Stmt, *ast.FuncLit:
			return false
		}
		child = ancestors[i]
	}
	return false
}

func hasNestedComparison(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && operator(b.Op) != 0 {
			found = true
			return false
		}
		return !found
	})
	return found
}

func parameterOrLiteral(expr ast.Expr, fn *ast.FuncDecl, info *types.Info) bool {
	switch operand := ast.Unparen(expr).(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		object := info.Uses[operand]
		if object != nil {
			for _, field := range fn.Type.Params.List {
				for _, parameter := range field.Names {
					if object == info.Defs[parameter] {
						return true
					}
				}
			}
		}
	}
	return false
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
				// A conversion or len/cap call is not a general function evaluation.
				safe = safe && (info.Types[child.Fun].IsType() || isLenOrCap(child, info))
			case *ast.UnaryExpr:
				safe = safe && child.Op != token.ARROW
			}
			return safe
		})
	}
	return safe
}

func isLenOrCap(call *ast.CallExpr, info *types.Info) bool {
	if len(call.Args) != 1 || info.Types[call].Value != nil {
		return false
	}
	ident, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	builtin, ok := info.Uses[ident].(*types.Builtin)
	return ok && (builtin.Name() == "len" || builtin.Name() == "cap")
}

func runtimeNamesAvailable(scope *types.Scope, testDecls map[string]bool) bool {
	for _, name := range strings.Fields("int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr float32 float64 bool nil len panic") {
		if scope.Lookup(name) != nil || testDecls[name] {
			return false
		}
	}
	return true
}

func samePackageTestDecls(dir, pkgName string) map[string]bool {
	decls := make(map[string]bool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return decls
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		tree, err := parser.ParseFile(fset, entry.Name(), data, parser.SkipObjectResolution)
		if err != nil || tree.Name.Name != pkgName {
			continue
		}
		for _, decl := range tree.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					decls[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, id := range s.Names {
							decls[id.Name] = true
						}
					case *ast.TypeSpec:
						decls[s.Name.Name] = true
					}
				}
			}
		}
	}
	return decls
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

func validOperand(expr ast.Expr, info *types.Info) bool {
	switch e := expr.(type) {
	case *ast.Ident, *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return validOperand(e.X, info)
	case *ast.SelectorExpr:
		return validOperand(e.X, info)
	case *ast.IndexExpr:
		return validOperand(e.X, info) && validOperand(e.Index, info)
	case *ast.StarExpr:
		return validOperand(e.X, info)
	case *ast.UnaryExpr:
		return (e.Op == token.ADD || e.Op == token.SUB || e.Op == token.XOR) && validOperand(e.X, info)
	case *ast.BinaryExpr:
		return e.Op >= token.ADD && e.Op <= token.AND_NOT && validOperand(e.X, info) && validOperand(e.Y, info)
	case *ast.CallExpr:
		return isLenOrCap(e, info) && validOperand(e.Args[0], info)
	}
	return false
}

func makeIntegerSite(site *Site, expr *ast.BinaryExpr, fn *ast.FuncDecl, pkg *packages.Package) bool {
	left, right := pkg.TypesInfo.Types[expr.X], pkg.TypesInfo.Types[expr.Y]
	if left.Type == nil || right.Type == nil || left.Value != nil && right.Value != nil {
		return false
	}
	variable := expr.X
	reversed := false
	if left.Value != nil {
		left, right = right, left
		reversed = true
		variable = expr.Y
	}
	basic, ok := left.Type.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsInteger == 0 || basic.Info()&types.IsUntyped != 0 {
		return false
	}
	if right.Value == nil {
		if !types.Identical(left.Type, right.Type) {
			return false
		}
		if reversed {
			site.Left, site.Right = site.Right, site.Left
			site.Op = site.Op.Reverse()
			site.Reversed = true
		}
		site.Margin = true
		return true
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
	widthMask := uint64(math.MaxUint64) >> (64 - bits)
	var domain analysis.Domain
	if basic.Info()&types.IsUnsigned != 0 {
		value, ok := constant.Uint64Val(right.Value)
		if !ok {
			return false
		}
		domain = analysis.Domain{High: widthMask, WidthMask: widthMask, Pivot: value}
	} else {
		value, ok := constant.Int64Val(right.Value)
		if !ok {
			return false
		}
		domain = analysis.Domain{
			Low: analysis.RankSigned(math.MinInt64 >> (64 - bits)), High: analysis.RankSigned(math.MaxInt64 >> (64 - bits)),
			WidthMask: widthMask, Pivot: analysis.RankSigned(value), Signed: true,
		}
	}
	if reversed {
		site.Left, site.Right = site.Right, site.Left
		site.Op = site.Op.Reverse()
		site.Reversed = true
	}
	site.Domain = &domain
	constrainOperand(site.Domain, variable, pkg.TypesInfo, bits)
	if id := directWitnessIdent(variable, pkg.TypesInfo); id != nil && directReturn(fn, expr) {
		findCallArguments(site, id, fn, pkg.TypesInfo)
	}
	return true
}

func directWitnessIdent(expr ast.Expr, info *types.Info) *ast.Ident {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return e
	case *ast.BinaryExpr:
		if e.Op == token.AND {
			if info.Types[e.Y].Value != nil {
				if id, ok := ast.Unparen(e.X).(*ast.Ident); ok {
					return id
				}
			}
			if info.Types[e.X].Value != nil {
				if id, ok := ast.Unparen(e.Y).(*ast.Ident); ok {
					return id
				}
			}
		} else if e.Op == token.AND_NOT && info.Types[e.Y].Value != nil {
			if id, ok := ast.Unparen(e.X).(*ast.Ident); ok {
				return id
			}
		}
	}
	return nil
}

func makeFloatSite(site *Site, expr *ast.BinaryExpr, info *types.Info) bool {
	left, right := info.Types[expr.X], info.Types[expr.Y]
	if left.Type == nil || right.Type == nil || left.Value != nil && right.Value != nil {
		return false
	}
	reversed := false
	if left.Value != nil {
		left, right = right, left
		reversed = true
	}
	basic, ok := left.Type.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsFloat == 0 || basic.Info()&types.IsUntyped != 0 {
		return false
	}
	if right.Value == nil {
		if !types.Identical(left.Type, right.Type) {
			return false
		}
		if reversed {
			site.Left, site.Right = site.Right, site.Left
			site.Op = site.Op.Reverse()
			site.Reversed = true
		}
		site.Float = true
		return true
	}
	if right.Value.Kind() != constant.Float && right.Value.Kind() != constant.Int {
		return false
	}
	if rightBasic, ok := right.Type.Underlying().(*types.Basic); !ok ||
		(rightBasic.Info()&types.IsUntyped == 0 && !types.Identical(left.Type, right.Type)) {
		return false
	}
	if reversed {
		site.Left, site.Right = site.Right, site.Left
		site.Op = site.Op.Reverse()
		site.Reversed = true
	}
	site.Float = true
	return true
}

func constrainOperand(domain *analysis.Domain, expr ast.Expr, info *types.Info, bits int64) {
	if attainable, _, ok := computeAttainable(domain, expr, info, bits); ok {
		domain.Attainable = &attainable
	}
}

func isUnconstrainedValueExpr(expr ast.Expr, info *types.Info) bool {
	e := ast.Unparen(expr)
	if info.Types[e].Value != nil {
		return false
	}
	switch n := e.(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return isUnconstrainedValueExpr(n.X, info)
	case *ast.StarExpr:
		return validOperand(n.X, info)
	}
	return false
}

func computeAttainable(domain *analysis.Domain, expr ast.Expr, info *types.Info, bits int64) (analysis.Attainable, bool, bool) {
	switch e := ast.Unparen(expr).(type) {
	case *ast.CallExpr:
		if isLenOrCap(e, info) {
			if domain.Signed {
				return analysis.Attainable{Low: analysis.RankSigned(0), High: domain.High}, false, true
			}
			return analysis.Attainable{Low: 0, High: domain.High}, false, true
		}
	case *ast.BinaryExpr:
		switch e.Op {
		case token.AND:
			value := info.Types[e.Y].Value
			varExpr := e.X
			if value == nil {
				value = info.Types[e.X].Value
				varExpr = e.Y
			}
			bounds, ok := andAttainable(domain, value, false)
			if !ok {
				return analysis.Attainable{}, false, false
			}
			if isUnconstrainedValueExpr(varExpr, info) {
				return bounds, true, true
			}
			if inner, innerBitwise, innerOK := computeAttainable(domain, varExpr, info, bits); innerOK {
				bounds.ZeroBits |= inner.ZeroBits
				tightenBitwiseHigh(domain, &bounds)
				if !domain.Signed || (inner.Low >= analysis.RankSigned(0) && bounds.Low >= analysis.RankSigned(0)) {
					if inner.Low > bounds.Low {
						bounds.Low = inner.Low
					}
					if inner.High < bounds.High {
						bounds.High = inner.High
					}
				}
				if !innerBitwise {
					bounds.Conservative = true
				}
				return bounds, innerBitwise, true
			}
			bounds.Conservative = true
			return bounds, false, true
		case token.AND_NOT:
			value := info.Types[e.Y].Value
			bounds, ok := andAttainable(domain, value, true)
			if !ok {
				return analysis.Attainable{}, false, false
			}
			if isUnconstrainedValueExpr(e.X, info) {
				return bounds, true, true
			}
			if inner, innerBitwise, innerOK := computeAttainable(domain, e.X, info, bits); innerOK {
				bounds.ZeroBits |= inner.ZeroBits
				tightenBitwiseHigh(domain, &bounds)
				if !domain.Signed || (inner.Low >= analysis.RankSigned(0) && bounds.Low >= analysis.RankSigned(0)) {
					if inner.Low > bounds.Low {
						bounds.Low = inner.Low
					}
					if inner.High < bounds.High {
						bounds.High = inner.High
					}
				}
				if !innerBitwise {
					bounds.Conservative = true
				}
				return bounds, innerBitwise, true
			}
			bounds.Conservative = true
			return bounds, false, true
		case token.SHL:
			value := info.Types[e.Y].Value
			if value == nil || value.Kind() != constant.Int {
				return analysis.Attainable{}, false, false
			}
			shift, ok := constant.Int64Val(value)
			if !ok || shift <= 0 || shift >= bits {
				return analysis.Attainable{}, false, false
			}
			zeroBits := (uint64(1) << shift) - 1
			var bounds analysis.Attainable
			if domain.Signed {
				maxPos := (domain.WidthMask >> 1) & ^zeroBits
				bounds = analysis.Attainable{Low: domain.Low, High: analysis.RankSigned(int64(maxPos)), ZeroBits: zeroBits}
			} else {
				bounds = analysis.Attainable{Low: 0, High: domain.WidthMask & ^zeroBits, ZeroBits: zeroBits}
			}
			if isUnconstrainedValueExpr(e.X, info) {
				return bounds, true, true
			}
			if inner, innerBitwise, innerOK := computeAttainable(domain, e.X, info, bits); innerOK {
				bounds.ZeroBits |= inner.ZeroBits << shift
				tightenBitwiseHigh(domain, &bounds)
				if !domain.Signed {
					if inner.High <= domain.WidthMask>>shift {
						lo := inner.Low << shift
						hi := inner.High << shift
						if lo > bounds.Low {
							bounds.Low = lo
						}
						if hi < bounds.High {
							bounds.High = hi
						}
					}
				} else if inner.Low >= analysis.RankSigned(0) {
					maxSigned := domain.WidthMask >> 1
					loVal := uint64(analysis.UnrankSigned(inner.Low))
					hiVal := uint64(analysis.UnrankSigned(inner.High))
					if hiVal <= maxSigned>>shift {
						lo := analysis.RankSigned(int64(loVal << shift))
						hi := analysis.RankSigned(int64(hiVal << shift))
						if lo > bounds.Low {
							bounds.Low = lo
						}
						if hi < bounds.High {
							bounds.High = hi
						}
					}
				}
				if !innerBitwise {
					bounds.Conservative = true
				}
				return bounds, innerBitwise, true
			}
			bounds.Conservative = true
			return bounds, false, true
		case token.REM:
			value := info.Types[e.Y].Value
			if value == nil || value.Kind() != constant.Int {
				return analysis.Attainable{}, false, false
			}
			if constant.Sign(value) < 0 {
				value = constant.UnaryOp(token.SUB, value, 0)
			}
			mod, ok := constant.Uint64Val(value)
			if !ok || mod == 0 {
				return analysis.Attainable{}, false, false
			}
			limit := mod - 1
			if isUnconstrainedValueExpr(e.X, info) {
				if !domain.Signed {
					if limit > domain.High {
						limit = domain.High
					}
					return analysis.Attainable{Low: 0, High: limit}, false, true
				}
				hiRank := min(analysis.RankSigned(int64(limit)), domain.High)
				loRank := max(analysis.RankSigned(-int64(limit)), domain.Low)
				return analysis.Attainable{Low: loRank, High: hiRank}, false, true
			}
			inner, innerBitwise, innerOK := computeAttainable(domain, e.X, info, bits)
			if innerOK && innerBitwise {
				if !domain.Signed && mod&(mod-1) == 0 {
					mask := mod - 1
					return analysis.Attainable{
						Low:      0,
						High:     min(inner.High, mask&^inner.ZeroBits&domain.WidthMask),
						ZeroBits: ^mask | inner.ZeroBits,
					}, true, true
				}
				if mod <= 1024 {
					ranks := exactBitwiseRemainderRanks(domain, inner, mod, bits)
					return analysis.Attainable{
						Low:        ranks[0],
						High:       ranks[len(ranks)-1],
						ExactRanks: ranks,
					}, false, true
				}
			}
			if !domain.Signed {
				if limit > domain.High {
					limit = domain.High
				}
				return analysis.Attainable{Low: 0, High: limit, Conservative: true}, false, true
			}
			hiRank := min(analysis.RankSigned(int64(limit)), domain.High)
			loRank := analysis.RankSigned(-int64(limit))
			if innerOK && inner.Low >= analysis.RankSigned(0) {
				loRank = analysis.RankSigned(0)
			}
			loRank = max(loRank, domain.Low)
			return analysis.Attainable{Low: loRank, High: hiRank, Conservative: true}, false, true
		}
	}
	return analysis.Attainable{}, false, false
}

func tightenBitwiseHigh(domain *analysis.Domain, bounds *analysis.Attainable) {
	free := ^bounds.ZeroBits & domain.WidthMask
	if !domain.Signed {
		if free < bounds.High {
			bounds.High = free
		}
		if free == 0 {
			bounds.Low = 0
		}
		return
	}
	signBit := uint64(1) << 63
	maxPos := analysis.RankSigned(int64(free & (domain.WidthMask >> 1)))
	if maxPos < bounds.High {
		bounds.High = maxPos
	}
	if free&(domain.WidthMask&^(domain.WidthMask>>1)) == 0 && bounds.Low < signBit {
		bounds.Low = signBit
	}
}

func exactBitwiseRemainderRanks(domain *analysis.Domain, inner analysis.Attainable, mod uint64, bits int64) []uint64 {
	m := int(mod)
	freeBits := ^inner.ZeroBits & domain.WidthMask
	lowMask := domain.WidthMask
	var signMask uint64
	if domain.Signed {
		signMask = uint64(1) << (bits - 1)
		lowMask = signMask - 1
	}
	posRem := make([]bool, m)
	posRem[0] = true
	for b := int64(0); b < bits; b++ {
		bit := uint64(1) << b
		if bit&lowMask == 0 || freeBits&bit == 0 {
			continue
		}
		weight := int(bit % mod)
		if weight == 0 {
			continue
		}
		next := slices.Clone(posRem)
		for r, ok := range posRem {
			if ok {
				next[(r+weight)%m] = true
			}
		}
		posRem = next
	}
	var ranks []uint64
	if !domain.Signed {
		for r, ok := range posRem {
			if ok {
				ranks = append(ranks, uint64(r))
			}
		}
		slices.Sort(ranks)
		return ranks
	}
	seen := make(map[uint64]bool, m*2)
	if inner.High >= analysis.RankSigned(0) {
		for r, ok := range posRem {
			if ok {
				seen[analysis.RankSigned(int64(r))] = true
			}
		}
	}
	if inner.Low < analysis.RankSigned(0) && freeBits&signMask != 0 {
		signRem := int(signMask % mod)
		for r, ok := range posRem {
			if ok {
				negMagRem := (signRem - r + m) % m
				seen[analysis.RankSigned(-int64(negMagRem))] = true
			}
		}
	}
	if len(seen) == 0 {
		seen[analysis.RankSigned(0)] = true
	}
	for rank := range seen {
		ranks = append(ranks, rank)
	}
	slices.Sort(ranks)
	return ranks
}

func andAttainable(domain *analysis.Domain, value constant.Value, invert bool) (analysis.Attainable, bool) {
	if value == nil || value.Kind() != constant.Int {
		return analysis.Attainable{}, false
	}
	if !domain.Signed {
		mask, ok := constant.Uint64Val(value)
		if !ok {
			return analysis.Attainable{}, false
		}
		if invert {
			mask = ^mask
		}
		return analysis.Attainable{Low: 0, High: mask & domain.WidthMask, ZeroBits: ^mask}, true
	}
	signedMask, ok := constant.Int64Val(value)
	if !ok {
		return analysis.Attainable{}, false
	}
	if invert {
		signedMask = ^signedMask
	}
	um := uint64(signedMask)
	if signedMask >= 0 {
		hi := analysis.RankSigned(signedMask)
		if hi > domain.High {
			hi = domain.High
		}
		return analysis.Attainable{Low: analysis.RankSigned(0), High: hi, ZeroBits: ^um}, true
	}
	maxPos := int64(um & (domain.WidthMask >> 1))
	return analysis.Attainable{Low: domain.Low, High: analysis.RankSigned(maxPos), ZeroBits: ^um}, true
}

func stringOperands(expr *ast.BinaryExpr, info *types.Info) bool {
	left, right := info.Types[expr.X], info.Types[expr.Y]
	if left.Type == nil || right.Type == nil {
		return false
	}
	if left.Value != nil {
		left, right = right, left
	}
	basic, ok := left.Type.Underlying().(*types.Basic)
	return ok && basic.Kind() == types.String && left.Value == nil &&
		(types.Identical(left.Type, right.Type) || right.Type == types.Typ[types.UntypedString] && right.Value != nil)
}

func directReturn(fn *ast.FuncDecl, expr *ast.BinaryExpr) bool {
	if fn.Recv != nil || fn.Type.TypeParams != nil || len(fn.Body.List) != 1 {
		return false
	}
	statement, ok := fn.Body.List[0].(*ast.ReturnStmt)
	return ok && len(statement.Results) == 1 && ast.Unparen(statement.Results[0]) == expr
}

func findCallArguments(site *Site, input *ast.Ident, fn *ast.FuncDecl, info *types.Info) {
	object := info.Uses[input]
	if fn.Name.Name == "_" || object == nil || fn.Type.Params.NumFields() > 8 {
		return
	}
	var args []string
	index := -1
	for _, field := range fn.Type.Params.List {
		typ := info.TypeOf(field.Type)
		if typ == nil || len(field.Names) == 0 {
			return
		}
		basic, ok := typ.Underlying().(*types.Basic)
		if !ok {
			return
		}
		var zero string
		switch {
		case basic.Info()&types.IsNumeric != 0:
			zero = "0"
		case basic.Info()&types.IsString != 0:
			zero = `""`
		case basic.Info()&types.IsBoolean != 0:
			zero = "0 != 0"
		default:
			return
		}
		for _, name := range field.Names {
			if info.Defs[name] == object {
				index = len(args)
			}
			args = append(args, zero)
		}
	}
	if index >= 0 {
		site.Callable, site.Arguments, site.InputArgument = fn.Name.Name, args, index
	}
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

// ParseIgnoreComments parses // and /* */ tmt:ignore directives into a 1-based line -> reason map.
func ParseIgnoreComments(fset *token.FileSet, syntax *ast.File, data []byte) map[int]string {
	if fset == nil || syntax == nil {
		return nil
	}
	positions := fset.File(syntax.Pos())
	if positions == nil {
		return nil
	}
	type commentDirective struct {
		startLine, endLine int
		startOff, endOff   int
		reason             string
	}
	var directives []commentDirective
	standalone := make(map[int]bool)
	for _, cg := range syntax.Comments {
		for _, c := range cg.List {
			startPos := positions.PositionFor(c.Pos(), false)
			endPos := positions.PositionFor(c.End()-1, false)
			startOff := startPos.Offset
			endOff := positions.Offset(c.End())
			beforeOnlyWS := isLineWhitespaceBefore(data, startOff)
			afterOnlyWS := isLineWhitespaceAfter(data, endOff)
			if startPos.Line == endPos.Line {
				if beforeOnlyWS && afterOnlyWS {
					standalone[startPos.Line] = true
				}
			} else {
				if beforeOnlyWS {
					standalone[startPos.Line] = true
				}
				for ln := startPos.Line + 1; ln < endPos.Line; ln++ {
					standalone[ln] = true
				}
				if afterOnlyWS {
					standalone[endPos.Line] = true
				}
			}
			if reason, ok := parseIgnoreCommentText(c.Text); ok {
				directives = append(directives, commentDirective{
					startLine: startPos.Line,
					endLine:   endPos.Line,
					startOff:  startOff,
					endOff:    endOff,
					reason:    reason,
				})
			}
		}
	}
	if len(directives) == 0 {
		return nil
	}
	totalLines := positions.LineCount()
	ignored := make(map[int]string, len(directives)*2)
	for _, d := range directives {
		if !isLineWhitespaceBefore(data, d.startOff) {
			setIgnoreReason(ignored, d.startLine, d.reason)
			continue
		}
		setIgnoreReason(ignored, d.startLine, d.reason)
		if d.endLine > d.startLine && !isLineWhitespaceAfter(data, d.endOff) {
			setIgnoreReason(ignored, d.endLine, d.reason)
			continue
		}
		if d.startLine == d.endLine && !isLineWhitespaceAfter(data, d.endOff) {
			continue
		}
		targetLine := d.endLine + 1
		for targetLine <= totalLines && standalone[targetLine] {
			targetLine++
		}
		if targetLine <= totalLines {
			setIgnoreReason(ignored, targetLine, d.reason)
		}
	}
	return ignored
}

func isLineWhitespaceBefore(data []byte, off int) bool {
	if off < 0 || off > len(data) {
		return false
	}
	i := off
	for i > 0 && data[i-1] != '\n' {
		i--
	}
	for _, b := range data[i:off] {
		if b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

func isLineWhitespaceAfter(data []byte, off int) bool {
	if off < 0 || off > len(data) {
		return false
	}
	for i := off; i < len(data) && data[i] != '\n'; i++ {
		if data[i] != ' ' && data[i] != '\t' && data[i] != '\r' {
			return false
		}
	}
	return true
}

func parseIgnoreCommentText(text string) (string, bool) {
	var inner string
	switch {
	case strings.HasPrefix(text, "//"):
		inner = strings.TrimSpace(text[2:])
	case strings.HasPrefix(text, "/*") && strings.HasSuffix(text, "*/"):
		inner = strings.TrimSpace(text[2 : len(text)-2])
	default:
		return "", false
	}
	rest, ok := strings.CutPrefix(inner, "tmt:ignore")
	if !ok {
		return "", false
	}
	if rest == "" {
		return "", true
	}
	if rest[0] == ':' {
		return strings.TrimSpace(rest[1:]), true
	}
	if rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r' {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

func setIgnoreReason(ignored map[int]string, line int, reason string) {
	if existing, ok := ignored[line]; !ok || existing == "" {
		ignored[line] = reason
	}
}
