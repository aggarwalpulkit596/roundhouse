package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/image"
)

func newServer(t *testing.T) (*httptest.Server, image.Reference, *image.Client) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Logf = nil
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "http://")
	ref, err := image.ParseReference(host + "/team/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	c := image.NewClient()
	c.HTTP = ts.Client()
	return ts, ref, c
}

func TestPushThenPullRoundTrip(t *testing.T) {
	_, ref, c := newServer(t)
	ctx := context.Background()
	layer := []byte("pretend this is a gzipped tar")
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	ld, cd := image.FromBytes(layer), image.FromBytes(cfg)

	for _, b := range [][]byte{layer, cfg} {
		b := b
		d := image.FromBytes(b)
		if ok, _ := c.BlobExists(ctx, ref, d); ok {
			t.Fatal("blob should not exist yet")
		}
		open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		if err := c.PutBlob(ctx, ref, d, int64(len(b)), open); err != nil {
			t.Fatal(err)
		}
		if ok, _ := c.BlobExists(ctx, ref, d); !ok {
			t.Fatal("blob should exist after upload")
		}
	}
	man, _ := json.Marshal(image.Manifest{
		SchemaVersion: 2, MediaType: image.MediaTypeOCIManifest,
		Config: image.Descriptor{MediaType: image.MediaTypeOCIConfig, Digest: cd, Size: int64(len(cfg))},
		Layers: []image.Descriptor{{MediaType: image.MediaTypeOCILayerGzip, Digest: ld, Size: int64(len(layer))}},
	})
	if err := c.PutManifest(ctx, ref, "v1", image.MediaTypeOCIManifest, man); err != nil {
		t.Fatal(err)
	}
	got, mt, err := c.GetManifest(ctx, ref, "v1")
	if err != nil || mt != image.MediaTypeOCIManifest || !bytes.Equal(got, man) {
		t.Fatalf("manifest: %v %s", err, mt)
	}
	rc, _, err := c.GetBlob(ctx, ref, ld)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, layer) {
		t.Fatal("layer content differs")
	}
}

func TestUploadWithWrongDigestIsRejected(t *testing.T) {
	ts, ref, c := newServer(t)
	lie := image.FromBytes([]byte("what I claim"))
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("what I send")), nil }
	if err := c.PutBlob(context.Background(), ref, lie, 11, open); err == nil {
		t.Fatal("registry accepted content that does not match its digest")
	}
	resp, _ := http.Head(ts.URL + "/v2/team/app/blobs/" + string(lie))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad blob was stored: %s", resp.Status)
	}
}

func TestManifestReferencingMissingBlobIsRejected(t *testing.T) {
	_, ref, c := newServer(t)
	man, _ := json.Marshal(image.Manifest{
		SchemaVersion: 2,
		Config:        image.Descriptor{Digest: image.FromBytes([]byte("nope")), Size: 4},
	})
	err := c.PutManifest(context.Background(), ref, "v1", image.MediaTypeOCIManifest, man)
	if err == nil || !strings.Contains(err.Error(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("want MANIFEST_BLOB_UNKNOWN, got %v", err)
	}
}

func TestChunkedUploadAndTagsList(t *testing.T) {
	ts, _, _ := newServer(t)
	resp, err := http.Post(ts.URL+"/v2/a/b/blobs/uploads/", "", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %v %v", err, resp.Status)
	}
	loc := ts.URL + resp.Header.Get("Location")
	for _, chunk := range []string{"hello ", "world"} {
		req, _ := http.NewRequest(http.MethodPatch, loc, strings.NewReader(chunk))
		r, err := http.DefaultClient.Do(req)
		if err != nil || r.StatusCode != http.StatusAccepted {
			t.Fatalf("patch: %v %v", err, r.Status)
		}
	}
	d := image.FromBytes([]byte("hello world"))
	req, _ := http.NewRequest(http.MethodPut, loc+"?digest="+string(d), nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != http.StatusCreated {
		t.Fatalf("finish: %v %v", err, r.Status)
	}
	// Cross-repo mount of an existing blob needs no upload.
	r, _ = http.Post(ts.URL+"/v2/other/repo/blobs/uploads/?mount="+string(d)+"&from=a/b", "", nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("mount: %s", r.Status)
	}
	r, _ = http.Get(ts.URL + "/v2/nothing/here/tags/list")
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("tags of unknown repo: %s", r.Status)
	}
}
