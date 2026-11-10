# No secret at build time: the API key reaches the container only as a runtime env var.

FROM node:26.13.0-alpine3.24 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# Every .go file in package web, so a file added later fails here too, not only in Docker.
COPY web/*.go web/
COPY --from=web /src/web/dist web/dist
# SQLite is pure Go, so the binary is static and runs on an image with no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/coldprobe ./cmd/coldprobe
# The final image has no shell to mkdir /data with.
RUN mkdir /out/data

# Distroless: no shell or package manager. Pinned by tag; production would pin the digest.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/coldprobe /coldprobe
# A named volume copies this owner on first mount, so the database is writable.
COPY --from=build --chown=65532:65532 /out/data /data
# Every interface inside the container; publish it on 127.0.0.1 only.
ENV COLDPROBE_ADDR=:8080 \
    COLDPROBE_DB=/data/coldprobe.db
USER nonroot
EXPOSE 8080
# No shell or curl, so the binary checks itself.
HEALTHCHECK CMD ["/coldprobe", "healthcheck"]
# Exec form: coldprobe is PID 1 and gets SIGTERM directly, so runs tear down their endpoints.
ENTRYPOINT ["/coldprobe"]
CMD ["serve"]
