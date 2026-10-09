# Building OneDroid Argus runner

## Requirements

- Go 1.26.2 or newer (`go.mod` names `go 1.26.2` and `toolchain go1.26.5`; with Go 1.21+ the `go` command
  fetches the toolchain by itself from the public Go toolchain mirror).
- Nothing else for the CLI and its tests. No GitHub token, no private module, no database, no Docker.
- Docker (with BuildKit) to build the image; JDK 21 and Maven 3.9 only if you build the AMQP sampler by hand.

Every dependency is a public Go module. Build with the public proxy and checksum database as they are:

```sh
env -u GITHUB_TOKEN -u GH_TOKEN GOFLAGS=-mod=mod GOPRIVATE= GONOSUMDB= go build ./...
```

## CLI

```sh
go build -o argus ./cmd/argus
./argus version
```

Stamp a release identity and a different default control plane:

```sh
PKG=github.com/OneDro1d/argus-runner/internal/buildinfo
go build -trimpath -o argus \
  -ldflags "-X $PKG.Version=0.1.0 -X $PKG.Commit=$(git rev-parse --short HEAD) -X $PKG.ControlPlaneURL=https://argus.example.com" \
  ./cmd/argus
```

`ControlPlaneURL`, `DocsURL` and `GrafanaURL` must be absolute `https://` addresses; the binary refuses to start
otherwise. The default control plane is `https://argus-dev.onedroid.ai`.

## Tests

```sh
go test ./...
```

The suite needs no network and no database. A few tests skip when a tool they exercise (for example `docker`,
`kubectl`, a PostgreSQL server, a RabbitMQ broker) is absent; `go test -v ./... | grep SKIP` lists them.
Tests that need the hosted control plane, a database, or the onboarding scripts are not part of this repository.

Run it against an empty module cache to prove nothing private is needed:

```sh
GOMODCACHE=$(mktemp -d) GOFLAGS=-mod=mod GOPRIVATE= GONOSUMDB= go test ./...
```

## The AMQP sampler

```sh
cd jmeter-plugins/amqp
mvn -B package          # unit tests need no broker; tests tagged "live" are excluded
```

## Image

```sh
docker build -t ghcr.io/onedro1d/argus-runner:dev .                  # slim: argus + JMeter + AMQP sampler
docker build --target ui -t ghcr.io/onedro1d/argus-runner:dev-ui .   # plus the Playwright harness and Chromium
```

Optional build arguments: `VERSION`, `COMMIT`, `BUILD_DATE`, and `ARGUS_DEFAULT_CONTROL_PLANE_URL`,
`ARGUS_DEFAULT_DOCS_URL`, `ARGUS_DEFAULT_GRAFANA_URL` (each replaces an address built into the binary). Apache JMeter and the
PostgreSQL JDBC driver are downloaded from the Apache archive and Maven Central. Publish to your own registry; this
repository does not publish anything by itself.

The image is multi-arch capable (the binary is cross-compiled; the sampler jar is bytecode). This `Dockerfile` was
assembled with the repository and has not been built yet: build it once before you rely on it.

## Layout notes

- `internal/chainread` is a read-only chain reader (block reads and signer recovery, anchor read-back under a signer
  allow-list, OpenTimestamps receipt checking). It has no function that sends a transaction and no key handling.
- `internal/msgenvelope` is the example Avro message envelope used by the `envelope` preset of the AMQP chain step.
- Packages are `internal/`; the supported interface is the `argus` command line.
