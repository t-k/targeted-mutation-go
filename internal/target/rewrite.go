package target

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

func Rewrite(file *File, prefix string) []byte {
	sites := slices.Clone(file.Sites)
	slices.SortFunc(sites, func(a, b *Site) int { return a.Start - b.Start })
	var result bytes.Buffer
	previous := 0
	for _, site := range sites {
		result.Write(file.Data[previous:site.Start])
		if followsIdentifierByte(file.Data, site.Start) {
			result.WriteByte(' ')
		}
		if site.Arith {
			fmt.Fprintf(&result, "%sArith(%d, %d, %s, %s", prefix, site.ID, site.ArithOp, site.Left, site.Right)
			lines := strings.Count(site.Expression, "\n") - strings.Count(site.Left, "\n") - strings.Count(site.Right, "\n")
			if lines > 0 {
				result.WriteByte(',')
				result.WriteString(strings.Repeat("\n", lines))
			}
			result.WriteString(")")
			previous = site.End
			continue
		}
		compare := "Compare"
		mask := site.Op.Mask()
		if site.String {
			compare = "CompareString"
		} else if site.Float {
			compare = "CompareFloat"
			mask = site.Op.FloatMask()
		}
		fmt.Fprintf(&result, "%s%s(%d, %d, %s, %s", prefix, compare, site.ID, mask, site.Left, site.Right)
		lines := strings.Count(site.Expression, "\n") - strings.Count(site.Left, "\n") - strings.Count(site.Right, "\n")
		if lines > 0 {
			result.WriteByte(',')
			result.WriteString(strings.Repeat("\n", lines))
		}
		result.WriteString(") != 0")
		previous = site.End
	}
	result.Write(file.Data[previous:])
	return result.Bytes()
}

func followsIdentifierByte(data []byte, start int) bool {
	if start == 0 {
		return false
	}
	prev := data[start-1]
	return 'a' <= prev && prev <= 'z' || 'A' <= prev && prev <= 'Z' || '0' <= prev && prev <= '9' || prev == '_' || prev >= 0x80
}
