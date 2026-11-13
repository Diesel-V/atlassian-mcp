# syntax=docker/dockerfile:1
# Tests run in CI before this build (see .gitlab-ci.yml); the image only compiles.
ARG GO_IMAGE=golang:1.25-alpine
FROM ${GO_IMAGE} AS build
WORKDIR /src
ARG GOPROXY
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod GOPROXY=${GOPROXY}
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/atlassian-mcp ./cmd/atlassian-mcp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/atlassian-mcp /atlassian-mcp
# Non-root, no shell, no package manager; the filesystem can be mounted read-only.
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/atlassian-mcp"]
CMD ["serve"]
