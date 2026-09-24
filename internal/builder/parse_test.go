package builder

import (
	"strings"
	"testing"
)

const multiStage = `
# comment
ARG BASE=alpine:3.20
FROM ${BASE} AS build
RUN apk add --no-cache \
      build-base \
      git
COPY --from=0 /x /y
FROM busybox AS assets
RUN echo hi
FROM busybox AS unused
RUN sleep 1
FROM build
COPY --from=assets /a /b
CMD ["app", "--flag"]
ENTRYPOINT /bin/sh -c 'x'
`

func TestParseMultiStage(t *testing.T) {
	rf, err := Parse(strings.NewReader(multiStage), map[string]string{"BASE": "alpine:edge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.Stages) != 4 {
		t.Fatalf("stages = %d", len(rf.Stages))
	}
	if rf.Stages[0].Base != "alpine:edge" {
		t.Fatalf("build arg not applied to FROM: %s", rf.Stages[0].Base)
	}
	run := rf.Stages[0].Steps[0]
	if run.Cmd != "RUN" || !strings.Contains(run.Args[0], "build-base") || !strings.Contains(run.Args[0], "git") {
		t.Fatalf("continuation not joined: %+v", run)
	}
	last := rf.Stages[3]
	if last.Base != "build" || len(last.Deps) != 2 {
		t.Fatalf("final stage should depend on build and assets: %+v", last)
	}
	cmd := last.Steps[1]
	if !cmd.JSON || cmd.Args[1] != "--flag" {
		t.Fatalf("JSON form not parsed: %+v", cmd)
	}
	target, _ := rf.Target("")
	need := rf.Needed(target)
	if need[2] || !need[0] || !need[1] || !need[3] {
		t.Fatalf("needed stages wrong (unused must be pruned): %v", need)
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"no from":       "RUN echo",
		"unsupported":   "FROM x\nHEALTHCHECK CMD true",
		"from args":     "FROM a b c d",
		"empty from":    "",
		"missing value": "FROM x\nWORKDIR",
	} {
		if _, err := Parse(strings.NewReader(src), nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestExpandAndKV(t *testing.T) {
	vars := map[string]string{"A": "1", "B": ""}
	if got := expand("x-$A-${A}-${B:-def}", vars); got != "x-1-1-def" {
		t.Fatalf("expand = %q", got)
	}
	kv, err := parseKV(`A=1 B="two words"`)
	if err != nil || kv[1][1] != "two words" {
		t.Fatalf("kv = %v %v", kv, err)
	}
	legacy, _ := parseKV("PATH /usr/bin:/bin")
	if legacy[0][0] != "PATH" || legacy[0][1] != "/usr/bin:/bin" {
		t.Fatalf("legacy form: %v", legacy)
	}
}

func TestContextIgnoreRules(t *testing.T) {
	c := &buildContext{ignores: []string{"node_modules", "*.log", "build"}}
	for path, want := range map[string]bool{
		"node_modules/x/y.js": true,
		"app.log":             true,
		"build/out.bin":       true,
		"src/main.go":         false,
		"src/build.go":        false,
	} {
		if got := c.ignored(path); got != want {
			t.Errorf("ignored(%s) = %v, want %v", path, got, want)
		}
	}
}
