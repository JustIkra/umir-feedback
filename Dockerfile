# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY bootstrap ./bootstrap
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/feedback ./cmd/feedback
RUN mkdir -p /state && chmod 0700 /state

FROM build AS otp-probe-build
COPY tests/container/otp-probe ./tests/container/otp-probe
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/otp-smoke-probe ./tests/container/otp-probe && \
    mkdir -p /otp-state && chmod 0700 /otp-state

FROM scratch AS otp-smoke-probe
COPY --link --from=otp-probe-build --chown=65532:65532 --chmod=0755 /out/otp-smoke-probe /otp-smoke-probe
COPY --link --from=otp-probe-build --chown=65532:65532 --chmod=0700 /otp-state /var/lib/otp-probe
USER 65532:65532
ENTRYPOINT ["/otp-smoke-probe"]

FROM scratch AS runtime
ARG VCS_REF
WORKDIR /app
COPY --link --from=build /out/server /server
COPY --link --from=build /out/feedback /feedback
COPY --link --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --link --from=build --chown=65532:65532 --chmod=0700 /state /var/lib/umir-feedback
ENV TZ=Europe/Moscow
LABEL org.opencontainers.image.source="https://github.com/JustIkra/umir-feedback" \
      org.opencontainers.image.revision=$VCS_REF
USER 65532:65532
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/server", "healthcheck"]
ENTRYPOINT ["/server"]
