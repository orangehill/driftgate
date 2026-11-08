# Install DriftGate

DriftGate is one static binary for Linux, macOS and Windows, on amd64 and
arm64. It needs nothing installed in your application.

## Download a release binary

```sh
gh release download v1.0.2 --repo orangehill/driftgate \
  --pattern driftgate-linux-amd64 --pattern 'SHA256SUMS*'
```

Take the file for your system: `driftgate-<os>-<arch>`, with `.exe` on
Windows. Other ways to get it are below.

## Verify the download

Verify the signature of `SHA256SUMS` first, then the binary against it.
Then install it:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/orangehill/driftgate/.github/workflows/release.yml@refs/tags/v1.0.2 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS      # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
install -m 0755 driftgate-linux-amd64 /usr/local/bin/driftgate
```

SBOMs, build provenance and reproducible builds are in
[release verification](../security.md#release-verification).

## Build from source

You need the Go version that `go.mod` names.

```sh
go install github.com/orangehill/driftgate/cmd/driftgate@latest
# or, in a checkout of this repository:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o driftgate ./cmd/driftgate
```

A source build reports its version as `dev`. Its help links the docs of
the release it was installed at (`go install …@<tag>`), or of the
`development` branch when built from a checkout.

## Run it in Docker

Build the image from the [`Dockerfile`](../../Dockerfile); no image is
published yet.

```sh
docker build -t driftgate .
docker run --rm -v "$PWD:/work" --network host driftgate run
```

The image holds only the binary: it runs as non-root and has no shell.
The commands `driftgate.yaml` runs (seed, restore, `php artisan` …) must
exist where DriftGate runs. For those, copy the binary into your
application's image:

```
COPY --from=driftgate /usr/local/bin/driftgate /usr/local/bin/
```

## Check that it works

```sh
driftgate version
driftgate help
```

`driftgate version` prints the version, and `driftgate help` lists every
command ([help in the terminal](../reference/cli.md#get-help-in-the-terminal)).

---
**Next:** [Tutorial: your first baseline](tutorial.md): see DriftGate find a change on a demo API.
**Related:** [Release verification](../security.md#release-verification) · [Command line basics](../reference/cli.md)
