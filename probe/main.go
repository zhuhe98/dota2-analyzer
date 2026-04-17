// probe: Dumps FINAL entity state (last seen, not first seen)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/dotabuff/manta"
)

type FieldEntry struct {
	Field  string `json:"field"`
	Sample string `json:"sample"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: probe [-o output.json] <replay.dem> [filter]")
		os.Exit(1)
	}

	outputPath := ""
	args := os.Args[1:]
	if args[0] == "-o" {
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: probe [-o output.json] <replay.dem> [filter]")
			os.Exit(1)
		}
		outputPath = args[1]
		args = args[2:]
	}

	filter := ""
	if len(args) > 1 {
		filter = args[1]
	}

	f, err := os.Open(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	p, err := manta.NewStreamParser(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parser: %v\n", err)
		os.Exit(1)
	}

	allClasses := make(map[string]bool)
	// Store LAST seen state for each entity class
	fields := make(map[string]map[string]string)

	p.OnEntity(func(e *manta.Entity, op manta.EntityOp) error {
		name := e.GetClassName()
		allClasses[name] = true

		if filter != "" && !strings.Contains(name, filter) {
			return nil
		}

		// Overwrite with latest state (not just first seen)
		m := e.Map()
		if _, ok := fields[name]; !ok {
			fields[name] = make(map[string]string)
		}
		for k, v := range m {
			s := fmt.Sprintf("%v", v)
			if len(s) > 80 {
				s = s[:80]
			}
			fields[name][k] = s
		}
		return nil
	})

	if err := p.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "parse: %v\n", err)
		os.Exit(1)
	}

	if filter == "" {
		var classes []string
		for c := range allClasses {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for _, c := range classes {
			fmt.Println(c)
		}
		return
	}

	result := make(map[string][]FieldEntry)
	for class, fieldMap := range fields {
		var entries []FieldEntry
		for k, v := range fieldMap {
			entries = append(entries, FieldEntry{Field: k, Sample: v})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Field < entries[j].Field })
		result[class] = entries
	}

	var out io.Writer = os.Stdout
	if outputPath != "" {
		of, err := os.Create(outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot create output file: %v\n", err)
			os.Exit(1)
		}
		defer of.Close()
		out = of
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.Encode(result)
}
