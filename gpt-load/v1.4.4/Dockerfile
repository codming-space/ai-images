FROM alpine

ARG TARGETARCH

WORKDIR /app
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata \
    && update-ca-certificates

COPY bin/gpt-load-${TARGETARCH} /app/gpt-load
RUN chmod +x /app/gpt-load

EXPOSE 3001
ENTRYPOINT ["/app/gpt-load"]
