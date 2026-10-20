# Build the authoritative arena server (with the PixiJS client embedded via go:embed).
FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/arena-server ./cmd/arena-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/arena-server /arena-server
EXPOSE 8080
ENTRYPOINT ["/arena-server", "-addr", ":8080"]
