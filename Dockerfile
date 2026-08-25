# syntax=docker/dockerfile:1

ARG GO_IMAGE=public.ecr.aws/docker/library/golang:1.24.13-alpine3.23
ARG TAILSCALE_IMAGE=ghcr.io/tailscale/tailscale:v1.102.3

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/tailgate-entrypoint/ ./cmd/tailgate-entrypoint/
COPY cmd/tailgate-socks/ ./cmd/tailgate-socks/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/tailgate-entrypoint ./cmd/tailgate-entrypoint \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/tailgate-socks ./cmd/tailgate-socks

FROM ${TAILSCALE_IMAGE}
COPY --from=build /out/tailgate-entrypoint /usr/local/bin/tailgate-entrypoint
COPY --from=build /out/tailgate-socks /usr/local/bin/tailgate-socks
ENTRYPOINT ["/usr/local/bin/tailgate-entrypoint"]
CMD []
