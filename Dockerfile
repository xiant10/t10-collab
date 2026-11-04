# syntax=docker/dockerfile:1

FROM golang:1.27.3-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/t10-modkit ./third_party/t10-modkit
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/t10-collab ./cmd/t10-collab

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=build /out/t10-collab /usr/local/bin/

USER 65534:65534

EXPOSE 18040

ENTRYPOINT ["/usr/local/bin/t10-collab"]
