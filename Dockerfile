# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/gitlab-mcp ./cmd/gitlab-mcp

FROM alpine:3.21
RUN apk --no-cache add ca-certificates \
	&& adduser -D -u 65532 nonroot
WORKDIR /
COPY --from=build /out/gitlab-mcp /gitlab-mcp
USER nonroot
ENTRYPOINT ["/gitlab-mcp"]
