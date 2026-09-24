// Package builder builds OCI images from a Dockerfile-compatible Railfile.
//
// It is a small BuildKit: instructions become a graph of stages, each step
// has a content-addressed cache key, independent stages build concurrently,
// stages the target does not need are never built, and every RUN executes in
// a real container from the runtime package.
package builder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Instruction is one parsed line.
type Instruction struct {
	Cmd  string   // upper-case: FROM, RUN, COPY, ...
	Args []string // shell form: one element; JSON form: the array
	JSON bool     // exec (JSON array) form
	// Flags like --from=builder, --chown=1000:1000.
	Flags map[string]string
	Line  int
	Raw   string
}

// Stage is a FROM and the instructions that follow it.
type Stage struct {
	Index int
	Name  string // from "AS name", may be empty
	Base  string // image reference or earlier stage name
	Steps []Instruction
	// Deps are indexes of stages this one needs (FROM <stage>, COPY --from).
	Deps []int
}

// Railfile is a parsed build definition.
type Railfile struct {
	Stages []*Stage
	// Args declared before the first FROM (usable in FROM lines).
	GlobalArgs map[string]string
}

var supported = map[string]bool{
	"FROM": true, "RUN": true, "COPY": true, "ADD": true, "ENV": true, "ARG": true,
	"WORKDIR": true, "USER": true, "CMD": true, "ENTRYPOINT": true, "EXPOSE": true,
	"LABEL": true, "STOPSIGNAL": true,
}

// Parse reads a Railfile. Build args are applied to ARG defaults.
func Parse(r io.Reader, buildArgs map[string]string) (*Railfile, error) {
	lines, err := logicalLines(r)
	if err != nil {
		return nil, err
	}
	rf := &Railfile{GlobalArgs: map[string]string{}}
	var cur *Stage
	names := map[string]int{}
	for _, ll := range lines {
		ins, err := parseLine(ll.text, ll.line)
		if err != nil {
			return nil, err
		}
		if !supported[ins.Cmd] {
			return nil, fmt.Errorf("line %d: unsupported instruction %s", ins.Line, ins.Cmd)
		}
		switch ins.Cmd {
		case "FROM":
			if len(ins.Args) != 1 {
				return nil, fmt.Errorf("line %d: FROM needs an image", ins.Line)
			}
			f := strings.Fields(ins.Args[0])
			st := &Stage{Index: len(rf.Stages), Base: expand(f[0], rf.GlobalArgs)}
			if len(f) == 3 && strings.EqualFold(f[1], "AS") {
				st.Name = strings.ToLower(f[2])
				names[st.Name] = st.Index
			} else if len(f) != 1 {
				return nil, fmt.Errorf("line %d: expected FROM image [AS name]", ins.Line)
			}
			if dep, ok := names[strings.ToLower(st.Base)]; ok && dep != st.Index {
				st.Deps = append(st.Deps, dep)
			}
			rf.Stages = append(rf.Stages, st)
			cur = st
			continue
		case "ARG":
			if cur == nil {
				k, v, _ := strings.Cut(ins.Args[0], "=")
				if bv, ok := buildArgs[k]; ok {
					v = bv
				}
				rf.GlobalArgs[k] = v
				continue
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: %s before FROM", ins.Line, ins.Cmd)
		}
		if from := ins.Flags["from"]; from != "" && ins.Cmd == "COPY" {
			if dep, ok := names[strings.ToLower(from)]; ok {
				cur.Deps = append(cur.Deps, dep)
			} else if n, err := parseIndex(from); err == nil && n < cur.Index {
				cur.Deps = append(cur.Deps, n)
				ins.Flags["from"] = rf.Stages[n].key()
			}
		}
		cur.Steps = append(cur.Steps, ins)
	}
	if len(rf.Stages) == 0 {
		return nil, fmt.Errorf("no FROM instruction")
	}
	return rf, nil
}

func (s *Stage) key() string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprint(s.Index)
}

func parseIndex(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// Target resolves the stage to build: a name, or the last stage.
func (rf *Railfile) Target(name string) (*Stage, error) {
	if name == "" {
		return rf.Stages[len(rf.Stages)-1], nil
	}
	for _, s := range rf.Stages {
		if s.Name == strings.ToLower(name) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no stage named %q", name)
}

// Needed returns the stages the target transitively depends on (including
// itself). Everything else is skipped — BuildKit does the same, which is
// why unused stages in a multi-stage Dockerfile cost nothing.
func (rf *Railfile) Needed(target *Stage) map[int]bool {
	need := map[int]bool{}
	var visit func(i int)
	visit = func(i int) {
		if need[i] {
			return
		}
		need[i] = true
		for _, d := range rf.Stages[i].Deps {
			visit(d)
		}
	}
	visit(target.Index)
	return need
}

type logicalLine struct {
	text string
	line int
}

// logicalLines joins backslash continuations and drops comments.
func logicalLines(r io.Reader) ([]logicalLine, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var out []logicalLine
	var buf strings.Builder
	start, n := 0, 0
	for sc.Scan() {
		n++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			start = n
		} else if strings.HasPrefix(trimmed, "#") {
			continue // comments inside a continuation are skipped
		}
		if strings.HasSuffix(line, "\\") {
			buf.WriteString(strings.TrimSuffix(line, "\\"))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(line)
		out = append(out, logicalLine{strings.TrimSpace(buf.String()), start})
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, logicalLine{strings.TrimSpace(buf.String()), start})
	}
	return out, sc.Err()
}

func parseLine(text string, line int) (Instruction, error) {
	cmd, rest, _ := strings.Cut(text, " ")
	ins := Instruction{Cmd: strings.ToUpper(cmd), Line: line, Raw: text, Flags: map[string]string{}}
	rest = strings.TrimSpace(rest)
	// Leading --flag=value options (COPY --from=x --chown=y).
	for strings.HasPrefix(rest, "--") {
		f, r, _ := strings.Cut(rest, " ")
		k, v, _ := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		ins.Flags[k] = v
		rest = strings.TrimSpace(r)
	}
	if strings.HasPrefix(rest, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(rest), &arr); err == nil {
			ins.Args, ins.JSON = arr, true
			return ins, nil
		}
	}
	if rest == "" && ins.Cmd != "RUN" {
		return ins, fmt.Errorf("line %d: %s needs arguments", line, ins.Cmd)
	}
	ins.Args = []string{rest}
	return ins, nil
}

// expand substitutes $VAR and ${VAR} (and ${VAR:-default}) from vars.
func expand(s string, vars map[string]string) string {
	return os.Expand(s, func(k string) string {
		if name, def, ok := strings.Cut(k, ":-"); ok {
			if v := vars[name]; v != "" {
				return v
			}
			return def
		}
		return vars[k]
	})
}

// splitWords splits on whitespace, honoring simple quotes (for COPY/EXPOSE).
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseKV parses `ENV A=1 B="two words"` or the legacy `ENV A 1`.
func parseKV(s string) ([][2]string, error) {
	words := splitWords(s)
	if len(words) == 0 {
		return nil, fmt.Errorf("expected KEY=VALUE")
	}
	if !strings.Contains(words[0], "=") {
		k, v, _ := strings.Cut(strings.TrimSpace(s), " ")
		return [][2]string{{k, strings.TrimSpace(v)}}, nil
	}
	var out [][2]string
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected KEY=VALUE, got %q", w)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}
