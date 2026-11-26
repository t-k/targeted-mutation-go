package campaign

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type compilerDiagnostic struct {
	path, message, build string
	line, column         int
}

var diagnosticPattern = regexp.MustCompile(`^(.+?):(\d+):(\d+): (.+)$`)

func parseCompilerOutput(build, output string) []compilerDiagnostic {
	var diagnostics []compilerDiagnostic
	for text := range strings.SplitSeq(output, "\n") {
		if m := diagnosticPattern.FindStringSubmatch(text); m != nil {
			line, _ := strconv.Atoi(m[2])
			column, _ := strconv.Atoi(m[3])
			diagnostics = append(diagnostics, compilerDiagnostic{path: m[1], line: line, column: column, message: m[4], build: build})
		}
	}
	return diagnostics
}

func candidateDiagnostics(run commandResult, workingDir, overlayFile string) []compilerDiagnostic {
	if run.moreDiagnostics {
		return nil
	}
	diagnostics := slices.Clone(run.diagnostics)
	for build := range run.failedBuilds {
		if build == "" || !slices.ContainsFunc(diagnostics, func(d compilerDiagnostic) bool { return d.build == build }) {
			return nil
		}
	}
	for i := range diagnostics {
		d := &diagnostics[i]
		if !filepath.IsAbs(d.path) {
			d.path = filepath.Join(workingDir, d.path)
		}
		if filepath.Clean(d.path) != overlayFile {
			return nil
		}
		d.path = overlayFile
	}
	return diagnostics
}

type sourcePatch struct {
	start, end token.Pos
	text       string
}

func repairUnused(data []byte, path string, diagnostics []compilerDiagnostic) []byte {
	if len(diagnostics) == 0 {
		return nil
	}
	for _, diag := range diagnostics {
		if filepath.Clean(diag.path) != path ||
			!unusedImport(diag.message) &&
				!strings.HasPrefix(diag.message, "declared and not used: ") {
			return nil
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	positions := fset.File(file.Pos())
	var patches []sourcePatch
	for _, diag := range diagnostics {
		patch, ok := findUnused(file, positions, diag)
		if !ok {
			return nil
		}
		if !slices.Contains(patches, patch) {
			patches = append(patches, patch)
		}
	}
	return applyRepairs(data, positions, patches)
}

func unusedImport(message string) bool {
	_, suffix, ok := strings.Cut(message, " imported ")
	return ok && (suffix == "and not used" || strings.HasPrefix(suffix, "as ") && strings.HasSuffix(suffix, " and not used"))
}

func findUnused(file *ast.File, positions *token.File, diag compilerDiagnostic) (sourcePatch, bool) {
	matches := func(pos token.Pos) bool {
		p := positions.PositionFor(pos, false)
		return p.Line == diag.line && p.Column == diag.column
	}
	if name, local := strings.CutPrefix(diag.message, "declared and not used: "); local {
		var patch sourcePatch
		var found bool
		ast.PreorderStack(file, nil, func(n ast.Node, stack []ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name && matches(id.Pos()) {
				patch, found = consumeLocal(id, stack)
			}
			return !found
		})
		return patch, found
	}
	for _, imported := range file.Imports {
		if !matches(imported.Pos()) && !matches(imported.Path.Pos()) {
			continue
		}
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return sourcePatch{}, false
		}
		prefix := strconv.Quote(path) + " imported"
		expected := prefix + " and not used"
		if imported.Name != nil {
			aliasMessage := prefix + " as " + imported.Name.Name + " and not used"
			if diag.message != expected && diag.message != aliasMessage {
				return sourcePatch{}, false
			}
			return sourcePatch{imported.Name.Pos(), imported.Name.End(), "_"}, true
		}
		// An implicit import can name a package whose declared name differs
		// from the final path component, including versioned module imports.
		implicit, ok := strings.CutPrefix(diag.message, prefix+" as ")
		name, unused := strings.CutSuffix(implicit, " and not used")
		if diag.message == expected || ok && unused && token.IsIdentifier(name) {
			return sourcePatch{imported.Path.Pos(), imported.Path.Pos(), "_ "}, true
		}
		return sourcePatch{}, false
	}
	return sourcePatch{}, false
}

func consumeLocal(id *ast.Ident, stack []ast.Node) (sourcePatch, bool) {
	if len(stack) < 2 {
		return sourcePatch{}, false
	}
	parent, grandparent := stack[len(stack)-1], stack[len(stack)-2]
	use := "_ = &" + id.Name
	insert := func(pos token.Pos, text string) (sourcePatch, bool) {
		return sourcePatch{pos, pos, text}, true
	}
	switch n := parent.(type) {
	case *ast.AssignStmt:
		if n.Tok != token.DEFINE || !slices.Contains(n.Lhs, ast.Expr(id)) {
			break
		}
		switch enclosing := grandparent.(type) {
		case *ast.IfStmt:
			return insert(enclosing.Body.Lbrace+1, " "+use+";")
		case *ast.ForStmt:
			return insert(enclosing.Body.Lbrace+1, " "+use+";")
		case *ast.SwitchStmt:
			return useInSwitch(enclosing.Body, use)
		case *ast.TypeSwitchStmt:
			return useInSwitch(enclosing.Body, use)
		case *ast.CommClause:
			if enclosing.Comm == n {
				return insert(enclosing.Colon+1, " "+use+";")
			}
			return insert(n.End(), "; "+use)
		default:
			return insert(n.End(), "; "+use)
		}
	case *ast.RangeStmt:
		if n.Tok == token.DEFINE && (n.Key == id || n.Value == id) {
			return insert(n.Body.Lbrace+1, " "+use+";")
		}
	case *ast.ValueSpec:
		if slices.Contains(n.Names, id) && len(stack) >= 3 {
			if declaration, ok := stack[len(stack)-3].(*ast.DeclStmt); ok {
				return insert(declaration.End(), "; "+use)
			}
		}
	}
	return sourcePatch{}, false
}

func useInSwitch(body *ast.BlockStmt, use string) (sourcePatch, bool) {
	if len(body.List) == 0 {
		pos := body.Lbrace + 1
		return sourcePatch{pos, pos, " default: " + use + ";"}, true
	}
	if clause, ok := body.List[0].(*ast.CaseClause); ok {
		pos := clause.Colon + 1
		return sourcePatch{pos, pos, " " + use + ";"}, true
	}
	return sourcePatch{}, false
}

func applyRepairs(data []byte, positions *token.File, patches []sourcePatch) []byte {
	slices.SortStableFunc(patches, func(a, b sourcePatch) int { return int(a.start - b.start) })
	var result bytes.Buffer
	last := 0
	for _, patch := range patches {
		start, end := positions.Offset(patch.start), positions.Offset(patch.end)
		if start < last {
			return nil
		}
		result.Write(data[last:start])
		result.WriteString(patch.text)
		last = end
	}
	result.Write(data[last:])
	return result.Bytes()
}
