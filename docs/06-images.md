# Module 6: Images and registries

> **Goal:** know every object in an OCI image, every HTTP request in a pull and a push, and why content addressing makes image transfer efficient.

## The objects

An image is a small graph of JSON documents and tarballs, each named by the sha256 of its bytes (digests below are shortened examples):

```text
tag "alpine:3.20" ──► index (multi-platform)                        sha256:9cee…
                        ├── linux/amd64 ──► manifest                sha256:33735b…
                        │                     ├── config ──►  {"Env":[…],"Cmd":[…],
                        │                     │                "rootfs":{"diff_ids":[sha256:…]}}
                        │                     └── layers[] ──► layer.tar.gz  sha256:…  (compressed)
                        └── linux/arm64 ──► manifest …
```

| Object                | Media type (OCI / Docker)                                                   | Named by                           |
| --------------------- | --------------------------------------------------------------------------- | ---------------------------------- |
| Index / manifest list | `application/vnd.oci.image.index.v1+json` / `…docker…manifest.list.v2+json` | digest of its JSON                 |
| Manifest              | `application/vnd.oci.image.manifest.v1+json` / `…docker…manifest.v2+json`   | digest of its JSON                 |
| Config                | `application/vnd.oci.image.config.v1+json`                                  | digest of its JSON = **image ID**  |
| Layer                 | `application/vnd.oci.image.layer.v1.tar+gzip` (or `+zstd`, or plain tar)    | digest of the **compressed** bytes |
| Diff ID               | (not a blob)                                                                | digest of the **uncompressed** tar |

Three identifiers people mix up:

- **Manifest digest:** what `alpine@sha256:…` pins. Changes if anything changes, including compression.
- **Image ID:** the config digest. Two registries can hold the same image ID under different manifest digests (for example, the same layers compressed differently).
- **Diff ID:** identifies layer _content_ independently of compression. Unpacked layers are keyed by diff ID ([`Store.LayerDir`](../internal/image/store.go)), so the same content pulled from two registries is stored once.

Roundhouse verifies every hash: blobs are streamed through sha256 into a temp file and only renamed into place if they match ([`PutBlob`](../internal/image/store.go)), and each unpacked layer's diff ID must match the config's `rootfs.diff_ids`. This is what makes a registry, a proxy or a CDN in the middle **untrusted**: it can withhold data but not alter it undetected, provided you started from a trusted manifest digest.

## A pull, request by request

```text
GET  /v2/library/alpine/manifests/3.20           Accept: index, list, manifest types
  ◄  401  WWW-Authenticate: Bearer realm="https://auth.docker.io/token",
           service="registry.docker.io",scope="repository:library/alpine:pull"
GET  https://auth.docker.io/token?service=…&scope=…       (anonymous, or with basic auth)
  ◄  200  {"token": "…"}
GET  /v2/library/alpine/manifests/3.20           Authorization: Bearer …
  ◄  200  index → pick linux/amd64
GET  /v2/library/alpine/manifests/sha256:33735b…
  ◄  200  manifest
GET  /v2/library/alpine/blobs/sha256:<config>
GET  /v2/library/alpine/blobs/sha256:<layer>     ◄ 307 redirect to a CDN, then the bytes
```

Read [`Client.do`](../internal/image/registry.go) for the token dance and [`Store.Pull`](../internal/image/store.go) for the flow. Layers are fetched and unpacked in parallel (four at a time), and a layer whose diff ID is already unpacked is not even downloaded.

Try the dance yourself:

```sh
TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull" | jq -r .token)
curl -s -H "Authorization: Bearer $TOKEN" \
     -H "Accept: application/vnd.oci.image.index.v1+json" \
     https://registry-1.docker.io/v2/library/alpine/manifests/3.20 | jq '.manifests[] | {digest, platform}'
```

(Docker Hub limits anonymous pulls per IP. Shared CI and cloud egress IPs hit this often, which is why Roundhouse's examples use `mirror.gcr.io`, and why platforms run pull-through caches.)

## A push, request by request

```text
HEAD /v2/team/app/blobs/sha256:<layer>           ◄ 200: already there, skip   (the whole trick)
POST /v2/team/app/blobs/uploads/                 ◄ 202 Location: /v2/team/app/blobs/uploads/<uuid>
PUT  /v2/team/app/blobs/uploads/<uuid>?digest=sha256:<layer>   (body = the bytes)
  ◄  201   (the registry hashed the body and it matched)
… same for the config …
PUT  /v2/team/app/manifests/v2                   (body = manifest JSON)
  ◄  201   (only if every referenced blob exists)
```

Push a one-line change to a 1 GB image and only the changed layer plus two small JSON documents move. `rh push` prints `already exists` for each skipped blob. Pushing the same image under a new tag costs one manifest PUT. Two more optimizations the spec allows:

- **Cross-repository mount:** `POST /blobs/uploads/?mount=<digest>&from=<other-repo>`. If the registry already has the blob in another repository, it links it without any upload.
- **Chunked uploads** (`PATCH` with ranges) make large uploads resumable.

Roundhouse's [registry](../internal/registry/registry.go) implements all of these. Its storage is content-addressed and shared across repositories, so a base layer used by a hundred apps is stored once. It deliberately skips per-repository access control, and its package comment explains why a production registry cannot.

## Efficient transfer, beyond what Roundhouse does

These come up in platform interviews because image transfer is often the slowest part of a deploy:

- **Pull-through caches / registry mirrors** close to the compute, so a popular base layer crosses the internet once per region, not once per host.
- **Pre-pulling** images onto hosts that are likely to run them (placement-aware).
- **zstd compression** (`+zstd` layers) decompresses several times faster than gzip. Roundhouse rejects zstd with a clear error: an [exercise](12-exercises.md).
- **Lazy pulling** ([eStargz / stargz-snapshotter](https://github.com/containerd/stargz-snapshotter), [SOCI](https://github.com/awslabs/soci-snapshotter)): start the container before the image is downloaded and fetch file contents on first access. Startup becomes proportional to what the app reads at boot, not to image size.
- **Peer-to-peer distribution** ([Dragonfly](https://d7y.io/), Uber's Kraken) for fleets that pull the same image at once.
- **Build close to where you run.** A platform that controls both the builders and the runtime hosts can place the first deploy of a fresh build on a host that already has most of its layers, or stream layers directly instead of through a central registry.

## Lab 6

```sh
sudo -E rh pull mirror.gcr.io/library/alpine:3.20
IMG=$(sudo -E rh images | awk '/alpine/{print $2}')
cd $RH_ROOT/images
jq . images.json
M=$(jq -r '.[] | select(.name|test("alpine")) | .manifest' images.json | cut -d: -f2)
jq . blobs/sha256/$M                                  # the manifest
C=$(jq -r .config.digest blobs/sha256/$M | cut -d: -f2)
jq '.rootfs, .config' blobs/sha256/$C                 # diff_ids and the run defaults
sha256sum blobs/sha256/$C                             # = the image ID, by definition

# Your own registry
sudo -E rh registry &
sudo -E rh push mirror.gcr.io/library/alpine:3.20 localhost:5000/base/alpine:1
sudo -E rh push mirror.gcr.io/library/alpine:3.20 localhost:5000/base/alpine:2   # all blobs skipped
curl -s localhost:5000/v2/base/alpine/tags/list
```

## Check yourself

1. Which of manifest digest, image ID and diff ID change if you recompress every layer with zstd?
2. Why must the client verify digests even over HTTPS?
3. Walk through a push of an image whose base layers already exist in another repository on the same registry.
4. Why does the registry refuse a manifest that references a blob it does not have?
5. Deploys on your platform are slow because image pulls are slow. List five fixes in order of effort.
