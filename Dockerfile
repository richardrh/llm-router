# syntax=docker/dockerfile:1
#
# Build:  docker build -t llm-router .
# Run:    docker run --rm -p 8787:8787 \
#           -v $PWD/router.yaml:/etc/llm-router/router.yaml:ro \
#           -e OPENROUTER_API_KEY=sk-or-... \
#           llm-router -config /etc/llm-router/router.yaml
#
# The binary is static (modernc.org/sqlite is pure Go), so the runtime image is
# distroless: no shell, no package manager, runs as the nonroot user.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go capabilities.json ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /llm-router .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /llm-router /llm-router
# config and usage store are mounted, not baked in
EXPOSE 8787
ENTRYPOINT ["/llm-router"]
