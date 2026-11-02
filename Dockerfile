# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go test ./... && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/atlassian-mcp ./cmd/atlassian-mcp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/atlassian-mcp /atlassian-mcp
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/atlassian-mcp"]
CMD ["serve"]
