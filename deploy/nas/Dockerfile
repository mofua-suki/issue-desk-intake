ARG GO_IMAGE=golang:1.27.3-bookworm
ARG TOOLS_IMAGE=issue-desk-tools:bc0356ff9e2e
ARG CODEX_TOOLS_IMAGE=issue-desk-tools:ca73ecf2fc7f
FROM ${GO_IMAGE} AS server-build
WORKDIR /build/server
ENV GOTOOLCHAIN=local GODEBUG="" CGO_ENABLED=0
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
ARG ISSUE_DESK_REVISION
RUN test "${#ISSUE_DESK_REVISION}" -eq 40 \
    && case "$ISSUE_DESK_REVISION" in *[!0-9a-f]*) exit 1;; esac \
    && go build -trimpath -ldflags="-s -w -X main.revision=${ISSUE_DESK_REVISION}" -o /out/intake ./cmd/intake

FROM ${CODEX_TOOLS_IMAGE} AS codex-tools
FROM ${TOOLS_IMAGE} AS tools
FROM tools AS runtime-tools
USER root
RUN rm -rf /opt/agent-tools/lib/node_modules/@openai/codex
COPY --from=codex-tools /opt/agent-tools/lib/node_modules/@openai/codex /opt/agent-tools/lib/node_modules/@openai/codex

FROM tools AS client-build
USER root
RUN mkdir -p /build/client && chown -R node:node /build
USER node
WORKDIR /build/client
COPY --chown=node:node client/package.json client/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --ignore-scripts --ignore-pnpmfile --config.minimum-release-age=20160
COPY --chown=node:node client/ ./
COPY --chown=node:node api/ /build/api/
COPY --chown=node:node shared/src/ /build/shared/src/
RUN node scripts/typecheck.ts && node scripts/build.ts

FROM node:26.13.1-bookworm-slim AS intake
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libatomic1 \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app /data/codex /data/claude /state \
    && chown -R node:node /app /data /state
COPY --from=runtime-tools /opt/agent-tools /opt/agent-tools
COPY --from=server-build /out/intake /usr/local/bin/intake
COPY --from=client-build --chown=node:node /build/client/dist /app/client/dist
COPY --chown=node:node api/openapi.json /app/api/openapi.json
COPY --chown=node:node server/schemas /app/server/schemas
COPY --chown=node:node docs/intake-skill-details.md /app/docs/intake-skill-details.md
COPY --chown=node:node assignments.json fleet.json host-ssh.json /app/
COPY --chown=node:node scripts/ /app/scripts/
COPY --chown=node:node shared/src/ /app/shared/src/
COPY --chown=node:node deploy/versions.json /app/deploy/versions.json
COPY --chown=node:node deploy/nas/chat-local.ts /app/deploy/nas/chat-local.ts
COPY --chown=node:node deploy/nas/intake-settings.json /app/deploy/nas/intake-settings.json
COPY --chown=node:node deploy/nas/intake-entrypoint.sh /usr/local/bin/intake-entrypoint
USER node
WORKDIR /app
ENV PATH=/opt/agent-tools/bin:$PATH CODEX_HOME=/data/codex CLAUDE_CONFIG_DIR=/data/claude \
    ISSUE_DESK_CLAUDE=/opt/agent-tools/bin/claude DISABLE_AUTOUPDATER=1 TZ=Asia/Tokyo
RUN test "$(codex --version)" = "codex-cli $(node -p 'require("/app/deploy/versions.json").codex')"
ARG ISSUE_DESK_REVISION
ENV ISSUE_DESK_REVISION=${ISSUE_DESK_REVISION}
ENTRYPOINT ["sh", "/usr/local/bin/intake-entrypoint"]
