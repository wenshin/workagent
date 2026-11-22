package main

import (
	"fmt"
	"unicode/utf8"

	"github.com/workagent/auto-agent/internal/tool"
)

func main() {
	for _, m := range tool.BuiltinManifests() {
		fmt.Printf("%-22s %d\n", m.Name, utf8.RuneCountInString(m.Description))
	}
}
