# The reference sandbox image: the contract server on Alpine, whose busybox
# userland is the shell /execute runs commands through. Language sandboxes layer
# the same binary into a runtime image — see the README.
#
# Bases are pinned by digest: an image that runs untrusted code should never
# change underfoot on a rebuild.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /sandbox .

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN adduser -D -H -u 10001 sandbox && mkdir -p /workspace && chown sandbox /workspace
COPY --from=build /sandbox /usr/local/bin/sandbox

ENV SANDBOX_WORKSPACE=/workspace \
    SANDBOX_PORT=3000 \
    SANDBOX_SHELL=/bin/sh

EXPOSE 3000
USER sandbox
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/sandbox"]
