FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/hub ./cmd/hub \
 && CGO_ENABLED=0 go build -o /out/rebuild-catalog ./cmd/rebuild-catalog

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/hub /hub
COPY --from=build /out/rebuild-catalog /rebuild-catalog
# Chunk scratch must outlive container restarts (finalize on next boot).
VOLUME /var/lib/surveillance-hub/chunks
EXPOSE 8080
ENTRYPOINT ["/hub"]
