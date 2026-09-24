package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseReference(t *testing.T) {
	cases := map[string]string{
		"alpine":                          "docker.io/library/alpine:latest",
		"alpine:3.20":                     "docker.io/library/alpine:3.20",
		"org/app":                         "docker.io/org/app:latest",
		"ghcr.io/org/app:v1":              "ghcr.io/org/app:v1",
		"localhost:5000/app":              "localhost:5000/app:latest",
		"127.0.0.1:5000/apps/hello:v2":    "127.0.0.1:5000/apps/hello:v2",
		"mirror.gcr.io/library/busybox:1": "mirror.gcr.io/library/busybox:1",
	}
	for in, want := range cases {
		r, err := ParseReference(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if r.String() != want {
			t.Errorf("%s -> %s, want %s", in, r.String(), want)
		}
	}
	d := "sha256:" + string(bytes.Repeat([]byte("a"), 64))
	r, err := ParseReference("alpine@" + d)
	if err != nil || r.Identifier() != d || r.Tag != "" {
		t.Fatalf("digest ref: %+v %v", r, err)
	}
	if r, _ := ParseReference("alpine"); r.Host() != "registry-1.docker.io" || r.Familiar() != "alpine:latest" {
		t.Fatalf("docker hub host/familiar wrong: %s %s", r.Host(), r.Familiar())
	}
	if r, _ := ParseReference("localhost:5000/x"); r.Scheme() != "http" {
		t.Fatal("localhost registries use http")
	}
	for _, bad := range []string{"", "UPPER/case", "a/../b", "alpine@sha256:xyz"} {
		if _, err := ParseReference(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDigestValidationBlocksPathTraversal(t *testing.T) {
	for _, d := range []Digest{"sha256:../../etc/passwd", "md5:abc", "sha256:ABC"} {
		if d.Validate() == nil {
			t.Errorf("%s should be invalid", d)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/alpine:pull" {
		t.Fatalf("got %s %v", s, p)
	}
}

func TestSelectPlatform(t *testing.T) {
	idx := Index{Manifests: []Descriptor{
		{Digest: "sha256:1", Platform: &Platform{OS: "linux", Architecture: "arm64"}},
		{Digest: "sha256:2", Platform: &Platform{OS: "linux", Architecture: "amd64"}},
	}}
	d, err := SelectPlatform(idx, "linux", "amd64")
	if err != nil || d.Digest != "sha256:2" {
		t.Fatalf("got %v %v", d, err)
	}
	if _, err := SelectPlatform(idx, "linux", "s390x"); err == nil {
		t.Fatal("missing platform must error")
	}
}

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func makeTar(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body)), Linkname: e.link, ModTime: time.Unix(1700000000, 0)}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func needRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (mknod, xattrs in the trusted namespace)")
	}
}

func TestUnpackConvertsWhiteoutsToOverlayFormat(t *testing.T) {
	needRoot(t)
	dir := t.TempDir()
	data := makeTar(t, []entry{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/.wh.motd", typ: tar.TypeReg},
		{name: "var/cache/", typ: tar.TypeDir, mode: 0o755},
		{name: "var/cache/.wh..wh..opq", typ: tar.TypeReg},
		{name: "bin/sh", typ: tar.TypeReg, body: "#!/bin/true", mode: 0o755},
		{name: "bin/ash", typ: tar.TypeLink, link: "bin/sh"},
	})
	d, err := UnpackTar(bytes.NewReader(data), dir)
	if err != nil {
		t.Fatal(err)
	}
	if d != FromBytes(data) {
		t.Fatalf("diff ID %s is not the hash of the tar stream", d)
	}
	fi, err := os.Lstat(filepath.Join(dir, "etc/motd"))
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("whiteout should be a char device: %v %v", fi, err)
	}
	if v, err := getXattr(filepath.Join(dir, "var/cache"), opaqueXattr); err != nil || string(v) != "y" {
		t.Fatalf("opaque xattr missing: %q %v", v, err)
	}
	a, _ := os.Stat(filepath.Join(dir, "bin/sh"))
	b, _ := os.Stat(filepath.Join(dir, "bin/ash"))
	if !os.SameFile(a, b) {
		t.Fatal("hardlink not preserved")
	}
}

func TestUnpackCannotEscapeThroughSymlinks(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	data := makeTar(t, []entry{
		// A symlink pointing outside, then a write "through" it.
		{name: "escape", typ: tar.TypeSymlink, link: outside},
		{name: "escape/pwned", typ: tar.TypeReg, body: "gotcha"},
		{name: "../../dotdot", typ: tar.TypeReg, body: "gotcha"},
	})
	if _, err := UnpackTar(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("layer wrote outside its directory through a symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, outside, "pwned")); err != nil {
		t.Fatalf("the write should be contained inside the layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dotdot")); err != nil {
		t.Fatalf("../ entries should be clamped inside the layer: %v", err)
	}
}

func TestLayerRoundTripIsReproducible(t *testing.T) {
	needRoot(t)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "app"), 0o755)
	os.WriteFile(filepath.Join(src, "app", "main"), []byte("binary"), 0o755)
	os.Symlink("main", filepath.Join(src, "app", "link"))
	epoch := time.Unix(0, 0)

	var a, b bytes.Buffer
	d1, err := WriteLayer(src, &a, LayerOptions{Epoch: &epoch})
	if err != nil {
		t.Fatal(err)
	}
	// Touch the files: with an epoch clamp the bytes must not change.
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "app", "main"), future, future)
	d2, _ := WriteLayer(src, &b, LayerOptions{Epoch: &epoch})
	if d1 != d2 || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("layer bytes depend on mtimes despite the epoch clamp")
	}

	out := t.TempDir()
	if _, err := UnpackTar(bytes.NewReader(a.Bytes()), out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "app", "main")); string(got) != "binary" {
		t.Fatalf("content lost: %q", got)
	}
	if l, _ := os.Readlink(filepath.Join(out, "app", "link")); l != "main" {
		t.Fatalf("symlink lost: %q", l)
	}
}

func TestStoreBlobsAreVerified(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := FromBytes([]byte("hello"))
	if _, _, err := s.PutBlob(bytes.NewReader([]byte("tampered")), want); err == nil {
		t.Fatal("a blob that does not match its digest must be rejected")
	}
	if s.HasBlob(want) {
		t.Fatal("rejected blob must not be stored")
	}
	d, n, err := s.PutBlob(bytes.NewReader([]byte("hello")), want)
	if err != nil || d != want || n != 5 || !s.HasBlob(want) {
		t.Fatalf("store: %v %d %v", d, n, err)
	}
}
