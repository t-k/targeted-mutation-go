package campaign

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSourceWitnessCalls = 80

func sourceWitnessCalls(job sourceJob) []string {
	if job.edit == nil || len(job.source) > 256*1024 || job.edit.Start < 0 ||
		job.edit.End < job.edit.Start || job.edit.End > len(job.source) ||
		string(job.source[job.edit.Start:job.edit.End]) != job.edit.Before {
		return nil
	}
	fset := token.NewFileSet()
	tree, err := parser.ParseFile(fset, job.edit.File, job.source, parser.SkipObjectResolution)
	if err != nil || slices.ContainsFunc(tree.Imports, func(spec *ast.ImportSpec) bool { return spec.Path.Value == `"C"` }) {
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
		if !witnessScalar(funcDecl.Type.Results.List[0].Type) {
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
		if len(result) < 12 && !slices.Contains(result, value) {
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
	ast.Inspect(body, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || len(result) >= 12 {
			return len(result) < 12
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
				if v, ok := constant.Int64Val(value); ok && v >= 0 && v < 127 {
					for _, neighbor := range []int64{v, max(0, v-1), v + 1} {
						add(strconv.FormatInt(neighbor, 10))
					}
				}
			}
		}
		return true
	})
	return result
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
	for offset := 0; offset < 12; offset++ {
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
