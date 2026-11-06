// Command dockerfile-inject writes a derived Dockerfile with ARG declarations
// inserted after every FROM. Values are supplied separately as --build-arg, so
// kaniko exposes them to RUN without writing them into the final image config
// as ENV.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

func main() {
	in := flag.String("in", "", "input Dockerfile")
	out := flag.String("out", "", "output Dockerfile")
	keys := flag.String("keys", "", "comma-separated ARG names")
	flag.Parse()

	keyList, err := parseKeys(*keys)
	if err != nil {
		fatal(err)
	}
	if *in == "" || *out == "" {
		fatal(fmt.Errorf("-in and -out are required"))
	}

	src, err := os.ReadFile(*in)
	if err != nil {
		fatal(fmt.Errorf("read %s: %w", *in, err))
	}
	derived, err := inject(src, keyList)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, derived, 0o644); err != nil {
		fatal(fmt.Errorf("write %s: %w", *out, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dockerfile-inject:", err)
	os.Exit(1)
}

func parseKeys(raw string) ([]string, error) {
	seen := map[string]bool{}
	keys := make([]string, 0)
	for _, key := range strings.Split(raw, ",") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if !isEnvName(key) {
			return nil, fmt.Errorf("invalid ARG name %q", key)
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("-keys is empty")
	}
	return keys, nil
}

func isEnvName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return s != ""
}

func inject(src []byte, keys []string) ([]byte, error) {
	parseSrc := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	result, err := parser.Parse(bytes.NewReader(parseSrc))
	if err != nil {
		return nil, fmt.Errorf("parse Dockerfile: %w", err)
	}

	var fromLines []int
	collectFrom(result.AST, &fromLines)
	if len(fromLines) == 0 {
		return nil, fmt.Errorf("dockerfile has no FROM instruction")
	}
	sort.Ints(fromLines)

	inserts := make(map[int][]string, len(fromLines))
	for _, line := range fromLines {
		if _, ok := inserts[line]; ok {
			continue
		}
		for _, key := range keys {
			inserts[line] = append(inserts[line], "ARG "+key)
		}
	}

	eol := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		eol = "\r\n"
	}
	lines := strings.Split(string(parseSrc), "\n")
	trailing := strings.HasSuffix(string(parseSrc), "\n")
	if trailing && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}

	outLines := make([]string, 0, len(lines)+len(fromLines)*len(keys))
	for i, line := range lines {
		outLines = append(outLines, line)
		outLines = append(outLines, inserts[i+1]...)
	}
	out := strings.Join(outLines, eol) + eol
	return []byte(out), nil
}

// collectFrom walks the BuildKit AST. The parser correctly keeps FROM-looking
// lines inside RUN/COPY heredocs out of the instruction list.
func collectFrom(node *parser.Node, out *[]int) {
	for n := node; n != nil; n = n.Next {
		if strings.EqualFold(n.Value, "from") && n.StartLine > 0 {
			*out = append(*out, n.EndLine)
		}
		for _, child := range n.Children {
			collectFrom(child, out)
		}
	}
}
