// Command covmerge merges Go coverage profiles of the same packages run in
// different environments (the records suite on three database engines),
// counting a block covered when any run covered it.
//
// Usage: go run ./internal/covmerge -o merged.out a.out b.out …
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	out := flag.String("o", "", "merged profile to write")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: covmerge -o merged.out a.out b.out …")
		os.Exit(2)
	}
	merged, err := merge(flag.Args())
	if err == nil {
		err = os.WriteFile(*out, []byte(merged), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "covmerge:", err)
		os.Exit(1)
	}
}

// merge reads profiles and returns one in "set" mode: a block is 1 when
// any profile counted it.
func merge(paths []string) (string, error) {
	covered := map[string]bool{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "mode:") || line == "" {
				continue
			}
			i := strings.LastIndexByte(line, ' ')
			if i < 0 {
				_ = f.Close()
				return "", fmt.Errorf("%s: malformed line %q", p, line)
			}
			block := line[:i]
			covered[block] = covered[block] || line[i+1:] != "0"
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return "", err
		}
	}
	blocks := make([]string, 0, len(covered))
	for b := range covered {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)
	var b strings.Builder
	b.WriteString("mode: set\n")
	for _, blk := range blocks {
		n := "0"
		if covered[blk] {
			n = "1"
		}
		fmt.Fprintf(&b, "%s %s\n", blk, n)
	}
	return b.String(), nil
}
