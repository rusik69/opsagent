# syntax=docker/dockerfile:1
FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/opsagent ./cmd/opsagent && \
    CGO_ENABLED=0 go build -o /out/mockllm ./deploy/demo/llm && \
    CGO_ENABLED=0 go build -o /out/mockgitlab ./deploy/demo/gitlab

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git openssh-client tzdata
WORKDIR /app
COPY --from=build /out/opsagent /app/opsagent
COPY --from=build /out/mockllm /app/mockllm
COPY --from=build /out/mockgitlab /app/mockgitlab
COPY agent-rules.md /app/agent-rules.md
EXPOSE 8080
ENTRYPOINT ["/app/opsagent"]
CMD ["-config", "/etc/opsagent/config.yaml"]
