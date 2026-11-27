package target

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

func Rewrite(file *File, prefix string) []byte {
	sites := slices.Clone(file.Sites)
	slices.SortFunc(sites, func(a, b *Site) int {
		if a.Start != b.Start {
			return a.Start - b.Start
		}
		return b.End - a.End
	})
	return []byte(rewriteSpan(file.Data, prefix, 0, len(file.Data), sites))
}

func rewriteSpan(data []byte, prefix string, start, end int, sites []*Site) string {
	var result bytes.Buffer
	previous := start
	for i := 0; i < len(sites); {
		site := sites[i]
		j := i + 1
		for j < len(sites) && sites[j].End <= site.End {
			j++
		}
		descendants := sites[i+1 : j]
		result.Write(data[previous:site.Start])
		if followsIdentifierByte(data, site.Start) {
			result.WriteByte(' ')
		}
		writeSite(&result, data, prefix, site, descendants)
		previous = site.End
		i = j
	}
	result.Write(data[previous:end])
	return result.String()
}

func writeSite(result *bytes.Buffer, data []byte, prefix string, site *Site, descendants []*Site) {
	if site.Bool {
		inner := rewriteSpan(data, prefix, site.Start, site.End, descendants)
		fmt.Fprintf(result, "%sBool(%d, %s)", prefix, site.ID, inner)
		return
	}
	if site.Assign {
		leftDesc := filterSpan(descendants, site.LeftStart, site.LeftEnd)
		left := rewriteSpan(data, prefix, site.LeftStart, site.LeftEnd, leftDesc)
		if site.IncDec {
			fmt.Fprintf(result, "%sAssign(%d, %d, &(%s), 1", prefix, site.ID, site.ArithOp, left)
			lines := bytes.Count(data[site.Start:site.End], []byte("\n")) - bytes.Count(data[site.LeftStart:site.LeftEnd], []byte("\n"))
			if lines > 0 {
				result.WriteByte(',')
				result.WriteString(strings.Repeat("\n", lines))
			}
			result.WriteString(")")
			return
		}
		rightDesc := filterSpan(descendants, site.RightStart, site.RightEnd)
		right := rewriteSpan(data, prefix, site.RightStart, site.RightEnd, rightDesc)
		fmt.Fprintf(result, "%sAssign(%d, %d, &(%s), %s", prefix, site.ID, site.ArithOp, left, right)
		writeTrailingNewlines(result, data, site)
		result.WriteString(")")
		return
	}
	leftDesc := filterSpan(descendants, site.LeftStart, site.LeftEnd)
	rightDesc := filterSpan(descendants, site.RightStart, site.RightEnd)
	left := rewriteSpan(data, prefix, site.LeftStart, site.LeftEnd, leftDesc)
	right := rewriteSpan(data, prefix, site.RightStart, site.RightEnd, rightDesc)
	if site.Logic {
		fmt.Fprintf(result, "%sLogic(%d, %d, func() %sBoolType { return %s }, func() %sBoolType { return %s }", prefix, site.ID, site.ArithOp, prefix, left, prefix, right)
		writeTrailingNewlines(result, data, site)
		result.WriteString(")")
		return
	}
	if site.Arith || site.Shift {
		fn := "Arith"
		if site.Shift {
			fn = "Shift"
			if site.UntypedShift {
				right = fmt.Sprintf("%sUint(%s)", prefix, right)
			}
		}
		fmt.Fprintf(result, "%s%s(%d, %d, %s, %s", prefix, fn, site.ID, site.ArithOp, left, right)
		writeTrailingNewlines(result, data, site)
		result.WriteString(")")
		return
	}
	compare := "Compare"
	mask := site.Op.Mask()
	if site.String {
		compare = "CompareString"
	} else if site.Float {
		compare = "CompareFloat"
		mask = site.Op.FloatMask()
	}
	fmt.Fprintf(result, "%s%s(%d, %d, %s, %s", prefix, compare, site.ID, mask, left, right)
	writeTrailingNewlines(result, data, site)
	result.WriteString(") != 0")
}

func filterSpan(sites []*Site, start, end int) []*Site {
	if len(sites) == 0 {
		return nil
	}
	var out []*Site
	for _, s := range sites {
		if s.Start >= start && s.End <= end {
			out = append(out, s)
		}
	}
	return out
}

func writeTrailingNewlines(result *bytes.Buffer, data []byte, site *Site) {
	lines := bytes.Count(data[site.Start:site.End], []byte("\n")) -
		bytes.Count(data[site.LeftStart:site.LeftEnd], []byte("\n")) -
		bytes.Count(data[site.RightStart:site.RightEnd], []byte("\n"))
	if lines > 0 {
		result.WriteByte(',')
		result.WriteString(strings.Repeat("\n", lines))
	}
}

func followsIdentifierByte(data []byte, start int) bool {
	if start == 0 {
		return false
	}
	prev := data[start-1]
	return 'a' <= prev && prev <= 'z' || 'A' <= prev && prev <= 'Z' || '0' <= prev && prev <= '9' || prev == '_' || prev >= 0x80
}
