# The reference sandbox image: the contract server, a shell to run commands
# with, and nothing else. Language images layer on top — see the README.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sandbox . \
    && mkdir /workspace

FROM gcr.io/distroless/static-debian12:nonroot
# busybox is static and self-contained, so its /bin — one binary plus applet
# symlinks — is all /execute needs for a POSIX shell and the usual coreutils.
COPY --from=busybox:1.37-musl /bin /bin
COPY --from=build /sandbox /sandbox
COPY --from=build --chown=nonroot:nonroot /workspace /workspace

ENV SANDBOX_WORKSPACE=/workspace \
    SANDBOX_PORT=3000 \
    SANDBOX_SHELL=/bin/sh \
    PATH=/bin:/usr/bin

EXPOSE 3000
USER nonroot
ENTRYPOINT ["/sandbox"]
