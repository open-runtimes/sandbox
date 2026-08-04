# The reference sandbox image: the contract server on Alpine, whose busybox
# userland is the shell /execute runs commands through. Language sandboxes layer
# the same binary into a runtime image — see the README.
#
# Bases are pinned by digest: an image that runs untrusted code should never
# change underfoot on a rebuild.
FROM --platform=$BUILDPLATFORM golang:1.25.12-alpine3.24@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /sandbox .

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN adduser -D -H -u 10001 sandbox && mkdir -p /workspace && chown sandbox /workspace
COPY --from=build /sandbox /usr/local/bin/sandbox

ENV SANDBOX_WORKSPACE=/workspace \
    SANDBOX_PORT=3000 \
    SANDBOX_SHELL=/bin/sh

EXPOSE 3000
USER sandbox
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/sandbox"]
