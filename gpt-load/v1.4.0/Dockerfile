FROM golang:alpine AS builder

ARG VERSION=1.0.0
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /build

ADD go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .
RUN go build -ldflags "-s -w -X gpt-load/internal/version.Version=${VERSION}" -o gpt-load


FROM alpine

WORKDIR /app
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata \
    && update-ca-certificates

COPY --from=builder /build/gpt-load .
EXPOSE 3001
ENTRYPOINT ["/app/gpt-load"]