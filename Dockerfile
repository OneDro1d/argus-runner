# OneDroid Argus runner image.
#
#   docker build -t ghcr.io/onedro1d/argus-runner:dev .                  # default target: slim
#   docker build --target ui -t ghcr.io/onedro1d/argus-runner:dev-ui .   # slim plus the Playwright UI harness
#
# slim: the `argus` binary, Apache JMeter, the PostgreSQL JDBC driver, the JMeter templates and the AMQP
#       sampler. This is what HTTP, database, message-flow and load scenarios need.
# ui:   slim plus the vendored Playwright harness (testkit/ui) and Chromium, for Web UI scenarios. Large.
#
# Everything is fetched from public sources (Go modules from the public proxy, Maven Central, the Apache
# archive). No registry credentials and no build secrets are needed.
#
# Build arguments for the binary (all optional):
#   VERSION / COMMIT / BUILD_DATE          stamped into `argus version`
#   ARGUS_DEFAULT_CONTROL_PLANE_URL        the control plane the CLI talks to by default
#                                          (default: https://argus-dev.onedroid.ai)

# -- stage 1: the argus binary ------------------------------------------------------------------
# Cross-compiled on the builder's own architecture for the image's. The binary is static.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=
ARG COMMIT=
ARG BUILD_DATE=
ARG ARGUS_DEFAULT_CONTROL_PLANE_URL=
ARG ARGUS_DEFAULT_DOCS_URL=
ARG ARGUS_DEFAULT_GRAFANA_URL=
RUN PKG=github.com/OneDro1d/argus-runner/internal/buildinfo; HOSTFLAGS=""; \
    [ -z "${ARGUS_DEFAULT_CONTROL_PLANE_URL}" ] || HOSTFLAGS="$HOSTFLAGS -X $PKG.ControlPlaneURL=${ARGUS_DEFAULT_CONTROL_PLANE_URL}"; \
    [ -z "${ARGUS_DEFAULT_DOCS_URL}" ] || HOSTFLAGS="$HOSTFLAGS -X $PKG.DocsURL=${ARGUS_DEFAULT_DOCS_URL}"; \
    [ -z "${ARGUS_DEFAULT_GRAFANA_URL}" ] || HOSTFLAGS="$HOSTFLAGS -X $PKG.GrafanaURL=${ARGUS_DEFAULT_GRAFANA_URL}"; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-X $PKG.Version=${VERSION} -X $PKG.Commit=${COMMIT} -X $PKG.Date=${BUILD_DATE}${HOSTFLAGS}" \
      -o /out/argus ./cmd/argus

# -- stage 2: the AMQP sampler (a JMeter plugin jar) --------------------------------------------
# The jar is bytecode, so this stage runs on the builder's architecture. The sampler's unit tests need
# no broker and run here.
FROM --platform=$BUILDPLATFORM maven:3.9-eclipse-temurin-21 AS amqp
WORKDIR /src
COPY jmeter-plugins/amqp/pom.xml ./
COPY jmeter-plugins/amqp/src ./src
RUN mvn -B -q package && \
    mkdir -p /out && cp target/jmeter-amqp-sampler-*.jar /out/ && rm -f /out/original-*.jar

# -- stage 3: base ------------------------------------------------------------------------------
FROM eclipse-temurin:21-jre AS base
ARG JMETER_VERSION=5.6.3
ARG PG_JDBC=42.7.4
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    curl -fsSL "https://archive.apache.org/dist/jmeter/binaries/apache-jmeter-${JMETER_VERSION}.tgz" -o /tmp/jmeter.tgz; \
    mkdir -p /opt/jmeter; tar -xzf /tmp/jmeter.tgz -C /opt/jmeter --strip-components=1; rm /tmp/jmeter.tgz; \
    curl -fsSL "https://repo1.maven.org/maven2/org/postgresql/postgresql/${PG_JDBC}/postgresql-${PG_JDBC}.jar" -o /opt/jmeter/lib/postgresql.jar
ENV PATH="/opt/jmeter/bin:${PATH}"

COPY --from=build /out/argus /usr/local/bin/argus
COPY templates /templates
COPY --from=amqp /out/jmeter-amqp-sampler-*.jar /opt/jmeter/lib/ext/
# Fail the build if the sampler jar is missing.
RUN set -e; ls -l /opt/jmeter/lib/ext; test -s /opt/jmeter/lib/ext/jmeter-amqp-sampler-*.jar

EXPOSE 8765
ENTRYPOINT ["argus"]
CMD ["serve", "--jmeter", "local", "--templates", "/templates", "--addr", ":8765"]

# -- ui: base plus the Playwright harness -------------------------------------------------------
FROM base AS ui
RUN set -eux; apt-get update; apt-get install -y --no-install-recommends nodejs npm; rm -rf /var/lib/apt/lists/*
COPY testkit/ui /testkit/ui
# Install the browsers with the harness's OWN Playwright and prove one launches, so the versions cannot drift.
RUN cd /testkit/ui && npm ci --no-audit --no-fund && npx playwright install --with-deps chromium && \
    node -e "const{chromium}=require('playwright');chromium.launch().then(b=>b.close()).then(()=>console.log('playwright launch check: OK')).catch(e=>{console.error('playwright launch check FAILED:',e.message);process.exit(1)})"
LABEL org.opencontainers.image.title="OneDroid Argus runner (ui)" \
      org.opencontainers.image.source="https://github.com/OneDro1d/argus-runner" \
      org.opencontainers.image.licenses="MIT"

# -- slim: the default (last stage) -------------------------------------------------------------
FROM base AS slim
ARG VERSION=
ARG COMMIT=
LABEL org.opencontainers.image.title="OneDroid Argus runner" \
      org.opencontainers.image.source="https://github.com/OneDro1d/argus-runner" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
