package campaign

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/t-k/targeted-mutation-go/internal/target"
)

type coverBlock struct {
	startLine, startCol int
	endLine, endCol     int
	count               int64
}

type coverageMap struct {
	files map[string][]coverBlock
}

func loadCoverageProfile(path string, packages []target.Package) *coverageMap {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	keyToPath := make(map[string]string)
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			keyToPath[pkg.Path+"/"+filepath.Base(file.Path)] = file.Path
		}
	}

	blocks := make(map[string][]coverBlock)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
		}
		if line == "" {
			continue
		}
		fileKey, block, ok := parseCoverLine(line)
		if !ok {
			continue
		}
		absPath, ok := keyToPath[fileKey]
		if !ok {
			continue
		}
		blocks[absPath] = append(blocks[absPath], block)
	}
	if len(blocks) == 0 {
		return nil
	}
	return &coverageMap{files: blocks}
}

func parseCoverLine(line string) (string, coverBlock, bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", coverBlock{}, false
	}
	count, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return "", coverBlock{}, false
	}
	colon := strings.LastIndexByte(fields[0], ':')
	if colon <= 0 {
		return "", coverBlock{}, false
	}
	fileKey := fields[0][:colon]
	rangePart := fields[0][colon+1:]
	startPart, endPart, ok := strings.Cut(rangePart, ",")
	if !ok {
		return "", coverBlock{}, false
	}
	sLine, sCol, ok1 := parseLineCol(startPart)
	eLine, eCol, ok2 := parseLineCol(endPart)
	if !ok1 || !ok2 {
		return "", coverBlock{}, false
	}
	return fileKey, coverBlock{
		startLine: sLine, startCol: sCol,
		endLine: eLine, endCol: eCol,
		count: count,
	}, true
}

func parseLineCol(s string) (int, int, bool) {
	lStr, cStr, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, false
	}
	l, err1 := strconv.Atoi(lStr)
	c, err2 := strconv.Atoi(cStr)
	if err1 != nil || err2 != nil || l < 1 || c < 1 {
		return 0, 0, false
	}
	return l, c, true
}

func cmpPos(l1, c1, l2, c2 int) int {
	if l1 != l2 {
		return l1 - l2
	}
	return c1 - c2
}

// isUncovered returns true only when the edit span lies strictly within at least
// one recorded basic block and every overlapping block executed zero times.
func (c *coverageMap) isUncovered(filePath string, source []byte, edit *target.Edit) bool {
	if c == nil || edit == nil || len(source) == 0 {
		return false
	}
	if bytes.Contains(source, []byte("//line ")) || bytes.Contains(source, []byte("/*line ")) {
		return false
	}
	blocks := c.files[filePath]
	if len(blocks) == 0 {
		return false
	}
	sLine, sCol := edit.StartLine, edit.Column
	eLine, eCol := edit.EndLine, endColumnForOffset(source, edit.End)
	if sLine < 1 || sCol < 1 || eLine < sLine || eCol < 1 {
		return false
	}
	contained := false
	for _, b := range blocks {
		if cmpPos(b.startLine, b.startCol, eLine, eCol) <= 0 && cmpPos(sLine, sCol, b.endLine, b.endCol) <= 0 {
			if b.count > 0 {
				return false
			}
			if cmpPos(b.startLine, b.startCol, sLine, sCol) <= 0 && cmpPos(eLine, eCol, b.endLine, b.endCol) <= 0 {
				contained = true
			}
		}
	}
	return contained
}

func endColumnForOffset(source []byte, end int) int {
	if end < 0 || end > len(source) {
		return 0
	}
	lastNL := bytes.LastIndexByte(source[:end], '\n')
	return end - lastNL
}

// isCoveragePrunableEdit reports whether a non-probe source mutation is
// statically guaranteed to remain compilable without triggering unused-var,
// unused-import, missing-return, token-joining, or constant-overflow errors.
func isCoveragePrunableEdit(edit *target.Edit, source []byte) bool {
	if edit == nil {
		return false
	}
	switch edit.Kind {
	case "source-operator":
		switch edit.Before {
		case "+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>", "&&", "||":
			return !editJoinsNeighbor(source, edit)
		}
		return false
	case "boolean-literal", "string-literal",
		"remove-not", "condition-negation",
		"error-check", "errorf-unwrap",
		"min-max", "rounding-mode",
		"affix-contains", "affix-end",
		"search-direction", "slice-element-remove",
		"integer-rounding-order":
		return true
	}
	return false
}

func dirHasTestFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			return true
		}
	}
	return false
}

func editJoinsNeighbor(source []byte, edit *target.Edit) bool {
	if edit.After == "" || edit.Start < 0 || edit.End > len(source) {
		return true
	}
	if edit.Start > 0 && isOperatorJoinByte(source[edit.Start-1], edit.After[0]) {
		return true
	}
	if edit.End < len(source) && isOperatorJoinByte(edit.After[len(edit.After)-1], source[edit.End]) {
		return true
	}
	return false
}

func isOperatorJoinByte(a, b byte) bool {
	switch [2]byte{a, b} {
	case [2]byte{'+', '+'}, [2]byte{'-', '-'}, [2]byte{'&', '&'}, [2]byte{'&', '^'},
		[2]byte{'|', '|'}, [2]byte{'<', '<'}, [2]byte{'>', '>'}, [2]byte{'<', '-'},
		[2]byte{'/', '*'}, [2]byte{'/', '/'}:
		return true
	}
	return false
}
