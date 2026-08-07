# 多阶段构建 Go Demo
FROM golang:1.25-alpine AS builder
WORKDIR /src
RUN go env -w GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/go_es_demo ./examples

# 运行时:等待 ES 就绪后执行 demo
FROM alpine:3.24
RUN apk add --no-cache curl bash
COPY --from=builder /out/go_es_demo /usr/local/bin/go_es_demo
COPY scripts/wait-for-es.sh /usr/local/bin/wait-for-es.sh
RUN chmod +x /usr/local/bin/wait-for-es.sh

ENV ES_ADDR=http://es:9200 \
    ES_WAIT_TIMEOUT=120

ENTRYPOINT ["/usr/local/bin/wait-for-es.sh", "/usr/local/bin/go_es_demo"]
