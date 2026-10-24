// Command mdwrap fails when a Markdown file contains hard-wrapped prose: a
// paragraph or list item broken across physical lines. The project rule is
// one physical line per paragraph, bullet and table row, so diffs show what
// changed and renderers do the wrapping.
//
// Usage: go run ./internal/mdwrap [dir…]   (default: the current directory)
//
// It skips fenced code blocks, YAML frontmatter, tables, HTML-style
// component tags and blockquotes, and descends into symlinked directories
// (the docsi/ docs are a symlink).
package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// structural matches lines that start a new block, so a line before them is
// not a wrapped paragraph: headings, list items, tables, HTML/components,
// blockquotes, fences, horizontal rules and indented code.
var structural = regexp.MustCompile(`^(\s{2,}|\t|#{1,6}\s|[-*+]\s|\d+[.)]\s|\||<|>|` + "```" + `|~~~|---|\*\*\*|___|\[[^\]]+\]:)`)

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}
	var findings []string
	for _, root := range roots {
		f, err := scanTree(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mdwrap:", err)
			os.Exit(2)
		}
		findings = append(findings, f...)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "mdwrap: %d hard-wrapped line(s); put each paragraph on one line\n", len(findings))
		os.Exit(1)
	}
}

// scanTree checks every .md file under root, following symlinked
// directories once.
func scanTree(root string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	var walk func(dir string) error
	walk = func(dir string) error {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if seen[real] {
			return nil
		}
		seen[real] = true
		// WalkDir does not follow a symlinked root, so walk the resolved
		// directory and report paths through the link the user knows.
		return filepath.WalkDir(real, func(rp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			p := filepath.Join(dir, strings.TrimPrefix(rp, real))
			name := d.Name()
			if d.IsDir() && rp != real && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			if d.Type()&fs.ModeSymlink != 0 {
				if info, err := os.Stat(p); err == nil && info.IsDir() {
					return walk(p)
				}
			}
			if !d.IsDir() && strings.HasSuffix(name, ".md") {
				f, err := scanFile(rp)
				if err != nil {
					return err
				}
				for _, finding := range f {
					out = append(out, filepath.Join(dir, strings.TrimPrefix(finding, real)))
				}
			}
			return nil
		})
	}
	return out, walk(root)
}

// scanFile reports "path:line: text" for every prose line followed by a
// continuation line.
func scanFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return check(path, lines), nil
}

func check(path string, lines []string) []string {
	var out []string
	inFence, inFrontmatter := false, len(lines) > 0 && lines[0] == "---"
	for i := 0; i < len(lines)-1; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if inFrontmatter {
			if i > 0 && line == "---" {
				inFrontmatter = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" || structural.MatchString(line) {
			continue
		}
		next := lines[i+1]
		if strings.TrimSpace(next) == "" || structural.MatchString(next) {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d: %s", path, i+1, line))
	}
	return out
}
