ARG GO_IMAGE
ARG NODE_IMAGE
FROM ${GO_IMAGE} AS go-toolchain
FROM ${NODE_IMAGE}
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
    GOTOOLCHAIN=local
WORKDIR /build
COPY server/go.mod server/go.sum ./server/
COPY client/package.json client/pnpm-lock.yaml client/pnpm-workspace.yaml ./client/
COPY deploy/versions.json ./deploy/versions.json
RUN --mount=type=secret,id=intake_ca,required=false set -eu; \
    if [ -f /run/secrets/intake_ca ]; then \
      if [ -f /etc/ssl/certs/ca-certificates.crt ]; then \
        cat /etc/ssl/certs/ca-certificates.crt /run/secrets/intake_ca > /tmp/intake-ca-bundle.crt; \
      else \
        cat /run/secrets/intake_ca > /tmp/intake-ca-bundle.crt; \
      fi; \
      export SSL_CERT_FILE=/tmp/intake-ca-bundle.crt NODE_EXTRA_CA_CERTS=/run/secrets/intake_ca; \
    fi; \
    expected_go="$(awk '$1 == "go" { print $2; exit }' server/go.mod)"; \
    test "$(go env GOVERSION)" = "go${expected_go}"; \
    node -e 'const v=require("./deploy/versions.json"); const p=require("./client/package.json"); if (process.versions.node !== v.node || p.packageManager !== `pnpm@${v.pnpm}`) process.exit(1)'; \
    npm install --global "pnpm@$(node -p 'require("./deploy/versions.json").pnpm')"; \
    cd /build/server; GOMODCACHE=/opt/public-deps/go go mod download; \
    cd /build/client; XDG_CACHE_HOME=/opt/public-deps/cache pnpm install --frozen-lockfile --ignore-scripts --store-dir /opt/public-deps/pnpm; \
    chmod -R a+rX /opt/public-deps; \
    rm -rf /build /root/.npm /root/.cache /tmp/intake-ca-bundle.crt
WORKDIR /
