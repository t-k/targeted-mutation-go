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
		fmt.Fprintf(&result, "%sCompare(%d, %d, %s, %s", prefix, site.ID, site.Op.Mask(), site.Left, site.Right)
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
