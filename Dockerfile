# Build stages run on the build machine and cross-compile, so a multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS app
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY migrations ./migrations
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /money-tracker . \
 && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/cullenmcdermott/money-tracker" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.title="money-tracker"
COPY --from=app /money-tracker /money-tracker
# Writable dir for backups, owned by the nonroot user (uid 65532); a mounted volume must be writable by that uid.
# The database is Postgres; supply DATABASE_URL at runtime.
COPY --from=app --chown=65532:65532 /data /data
ENV BACKUP_DIR=/data/backups
VOLUME /data
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/money-tracker"]
