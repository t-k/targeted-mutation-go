package campaign

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxSourceWitnessCalls = 80
	maxWitnessExamples    = 13
)

func sourceWitnessCalls(job sourceJob) []string {
	if job.edit == nil || len(job.source) > 256*1024 || job.edit.Start < 0 ||
		job.edit.End < job.edit.Start || job.edit.End > len(job.source) ||
		string(job.source[job.edit.Start:job.edit.End]) != job.edit.Before {
		return nil
	}
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, job.edit.File, job.source, 0)
	if err != nil || slices.ContainsFunc(tree.Imports, blockedSourceWitnessImport) {
		return nil
	}
	file := fset.File(tree.Pos())
	for _, decl := range tree.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Body == nil || funcDecl.Recv != nil || funcDecl.Type.TypeParams != nil ||
			funcDecl.Type.Results.NumFields() != 1 || funcDecl.Type.Params.NumFields() > 3 ||
			job.edit.Start <= file.Offset(funcDecl.Body.Pos()) || job.edit.End >= file.Offset(funcDecl.Body.End()) {
			continue
		}
		if hasSideEffects(funcDecl) || !witnessScalar(funcDecl.Type.Results.List[0].Type) {
			return nil
		}
		var values [][]string
		for _, field := range funcDecl.Type.Params.List {
			name, ok := field.Type.(*ast.Ident)
			if !ok || !witnessScalar(name) {
				return nil
			}
			examples := witnessInputValues(name.Name, funcDecl.Body)
			for range max(1, len(field.Names)) {
				values = append(values, examples)
			}
		}
		return sampleWitnessCalls(funcDecl.Name.Name, values)
	}
	return nil
}

func blockedSourceWitnessImport(spec *ast.ImportSpec) bool {
	path, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return true
	}
	switch path {
	case "C", "os", "os/exec", "os/signal", "syscall", "net", "net/http", "database/sql", "io/ioutil", "plugin", "unsafe":
		return true
	}
	return false
}

func hasSideEffects(fn *ast.FuncDecl) bool {
	unsafe := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if unsafe {
			return false
		}
		switch stmt := node.(type) {
		case *ast.SendStmt, *ast.GoStmt:
			unsafe = true
			return false
		case *ast.IncDecStmt:
			if !isLocalTarget(stmt.X, fn) {
				unsafe = true
				return false
			}
		case *ast.AssignStmt:
			if stmt.Tok != token.DEFINE {
				for _, lhs := range stmt.Lhs {
					if !isLocalTarget(lhs, fn) {
						unsafe = true
						return false
					}
				}
			}
		case *ast.RangeStmt:
			if stmt.Tok == token.ASSIGN {
				for _, lhs := range []ast.Expr{stmt.Key, stmt.Value} {
					if lhs != nil && !isLocalTarget(lhs, fn) {
						unsafe = true
						return false
					}
				}
			}
		}
		return true
	})
	return unsafe
}

func isLocalTarget(expr ast.Expr, fn *ast.FuncDecl) bool {
	id := rootIdent(expr)
	if id == nil {
		return false
	}
	if id.Name == "_" {
		return true
	}
	if id.Obj == nil || id.Obj.Decl == nil {
		return false
	}
	decl, ok := id.Obj.Decl.(ast.Node)
	if !ok {
		return false
	}
	return decl.Pos() >= fn.Pos() && decl.End() <= fn.End()
}

func rootIdent(expr ast.Expr) *ast.Ident {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.X
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		default:
			id, _ := expr.(*ast.Ident)
			return id
		}
	}
}

type shadowedIdents struct {
	hasTesting, hasT, hasGot, hasBool bool
}

func packageShadowedIdents(dir string, extra ...[]byte) shadowedIdents {
	var out shadowedIdents
	merge := func(src []byte) {
		cur := parseShadowedIdents(src)
		out.hasTesting = out.hasTesting || cur.hasTesting
		out.hasT = out.hasT || cur.hasT
		out.hasGot = out.hasGot || cur.hasGot
		out.hasBool = out.hasBool || cur.hasBool
	}
	for _, src := range extra {
		merge(src)
	}
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
					if data, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
						merge(data)
					}
				}
			}
		}
	}
	return out
}

func parseShadowedIdents(src []byte) shadowedIdents {
	var out shadowedIdents
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	check := func(name string) {
		switch name {
		case "testing":
			out.hasTesting = true
		case "t":
			out.hasT = true
		case "got":
			out.hasGot = true
		case "true", "false":
			out.hasBool = true
		}
	}
	for _, decl := range tree.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				check(d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, id := range s.Names {
						check(id.Name)
					}
				case *ast.TypeSpec:
					check(s.Name.Name)
				}
			}
		}
	}
	return out
}

func witnessScalar(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	switch id.Name {
	case "bool", "string", "int", "int8", "int16", "int32", "int64", "rune",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "byte", "float32", "float64":
		return true
	}
	return false
}

func witnessInputValues(typ string, body *ast.BlockStmt) []string {
	if typ == "bool" {
		return []string{"(0 != 0)", "(0 == 0)"}
	}
	var result []string
	add := func(value string) {
		if len(result) < maxWitnessExamples && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	switch typ {
	case "string":
		for _, v := range []string{"", "a", "xa", "ax", "aaa", "é"} {
			add(strconv.Quote(v))
		}
	case "float32", "float64":
		for _, v := range []string{"0.0", "0.5", "-0.5", "1.5", "-1.5", "1.0", "2.25"} {
			add(v)
		}
	default:
		for _, v := range []string{"0", "1", "2", "3", "4", "7", "10", "31"} {
			add(v)
		}
		if strings.HasPrefix(typ, "int") || typ == "rune" {
			add("-1")
		}
	}
	maxInt := maxWitnessInt(typ)
	ast.Inspect(body, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || len(result) >= maxWitnessExamples {
			return len(result) < maxWitnessExamples
		}
		switch typ {
		case "string":
			if lit.Kind == token.STRING {
				value, err := strconv.Unquote(lit.Value)
				if err == nil && value != "" && len(value) <= 64 {
					for _, v := range []string{value, "x" + value, value + "x", value + value} {
						add(strconv.Quote(v))
					}
				}
			}
		case "float32", "float64":
			if lit.Kind == token.FLOAT || lit.Kind == token.INT {
				value := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
				if v, _ := constant.Float64Val(value); math.Abs(v) <= 65536 {
					for _, neighbor := range []float64{v, v - 0.5, v + 0.5} {
						add(strconv.FormatFloat(neighbor, 'g', -1, 64))
					}
				}
			}
		default:
			if lit.Kind == token.INT {
				value := constant.MakeFromLiteral(lit.Value, lit.Kind, 0)
				if v, ok := constant.Int64Val(value); ok && v >= 0 && v <= 1_000_000_000 && v <= maxInt {
					neighbors := []int64{v, max(0, v-1), v + 1}
					if v >= 100 {
						neighbors = append(neighbors, v+v/100+1)
					}
					for _, neighbor := range neighbors {
						if neighbor <= maxInt {
							add(strconv.FormatInt(neighbor, 10))
						}
					}
				}
			}
		}
		return true
	})
	return result
}

func maxWitnessInt(typ string) int64 {
	switch typ {
	case "int8":
		return 127
	case "uint8", "byte":
		return 255
	case "int16":
		return 32767
	case "uint16":
		return 65535
	default:
		return 1_000_000_000
	}
}

func sampleWitnessCalls(name string, values [][]string) []string {
	var calls []string
	add := func(arguments []string) {
		call := name + "(" + strings.Join(arguments, ", ") + ")"
		if len(calls) < maxSourceWitnessCalls && !slices.Contains(calls, call) {
			calls = append(calls, call)
		}
	}
	args := make([]string, len(values))
	for i, examples := range values {
		args[i] = examples[0]
	}
	add(args)
	for i, examples := range values {
		for _, value := range examples {
			args[i] = value
			add(args)
		}
		args[i] = examples[0]
	}
	// Visit all argument pairs round-robin before exhausting the call budget.
	for offset := 0; offset < maxWitnessExamples; offset++ {
		for i := range values {
			for j := i + 1; j < len(values); j++ {
				for x, value := range values[i] {
					args[i], args[j] = value, values[j][(x+offset)%len(values[j])]
					add(args)
				}
				args[i], args[j] = values[i][0], values[j][0]
			}
		}
	}
	return calls
}

func sourceWitnessLiteral(text string) bool {
	if text == "" || len(text) > 1024 || !utf8.ValidString(text) {
		return false
	}
	expr, err := parser.ParseExpr(text)
	if err != nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return text == "true" || text == "false"
	case *ast.BasicLit:
		return e.Value == text && (e.Kind == token.INT || e.Kind == token.FLOAT || e.Kind == token.STRING)
	case *ast.UnaryExpr:
		lit, ok := e.X.(*ast.BasicLit)
		return ok && e.Op == token.SUB && text == "-"+lit.Value && (lit.Kind == token.INT || lit.Kind == token.FLOAT)
	default:
		return false
	}
}
