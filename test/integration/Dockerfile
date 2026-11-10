ARG GO_VERSION=1.26.10
ARG NODE_VERSION=24.22.1

FROM golang:${GO_VERSION}-bookworm AS go-toolchain
FROM node:${NODE_VERSION}-bookworm

# Both stages use Debian Bookworm. The Node image includes the C compiler,
# Bash, Git and curl needed by Go's race detector and the installer fixtures.
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}" \
    GOTOOLCHAIN=local \
    AGENTCFG_TEST_NPX=1 \
    AGENTCFG_TEST_RACE=1

RUN mkdir -p /workspace && chown node:node /workspace
USER node
WORKDIR /workspace

COPY --chown=node:node go.mod go.sum ./
RUN go mod download
COPY --chown=node:node cmd ./cmd
COPY --chown=node:node internal ./internal
COPY --chown=node:node examples ./examples
COPY --chown=node:node install.sh install.ps1 ./
COPY --chown=node:node test/integration/run.sh ./test/integration/run.sh

CMD ["bash", "test/integration/run.sh"]
