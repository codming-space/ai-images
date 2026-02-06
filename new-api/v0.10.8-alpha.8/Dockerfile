# 运行时镜像
FROM debian:bookworm-slim

ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata libasan8 wget \
    && rm -rf /var/lib/apt/lists/* \
    && update-ca-certificates

COPY bin/new-api-${TARGETARCH} /new-api
RUN chmod +x /new-api

EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
