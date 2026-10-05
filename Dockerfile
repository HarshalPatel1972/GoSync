# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gosync-server ./cmd/gosync-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gosync-server /gosync-server
ENV GOSYNC_ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/gosync-server"]
