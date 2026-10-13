# syntax=docker/dockerfile:1
FROM golang:1.26.9-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/fixture-server ./tests/fixtures/server && go build -trimpath -o /out/fixture-control ./tests/fixtures/control
FROM alpine:3.22 AS runtime
ARG SOURCE_REVISION
LABEL org.opencontainers.image.source="https://github.com/JustIkra/umir-feedback" org.opencontainers.image.revision=$SOURCE_REVISION
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /var/lib/umir-feedback /run/umir-fixtures /usr/local/lib/umir && chown 65532:65532 /var/lib/umir-feedback /run/umir-fixtures && chmod 0700 /var/lib/umir-feedback /run/umir-fixtures
COPY --from=build /out/fixture-server /fixture-server
COPY --from=build /out/fixture-control /fixture-control
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /usr/local/lib/umir/zoneinfo.zip
USER 65532:65532
ENTRYPOINT ["/fixture-server"]
CMD ["serve"]
HEALTHCHECK --interval=5s --timeout=10s --start-period=5s CMD ["/fixture-server", "healthcheck"]
