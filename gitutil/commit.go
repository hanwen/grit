package gitutil

import (
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
)

func Subject(c *object.Commit) string {
	lines := strings.Split(c.Message, "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}
