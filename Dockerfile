# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM alpine:3.21 AS terraform
ARG TARGETARCH
ARG TERRAFORM_VERSION=1.9.8
RUN apk add --no-cache curl unzip \
  && curl -fsSL "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip" -o /tmp/terraform.zip \
  && unzip -q -o /tmp/terraform.zip -d /out \
  && chmod +x /out/terraform

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
ARG TARGETOS=linux
ARG TARGETARCH
RUN go mod download \
  && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
     go build -trimpath \
       net/http crypto/tls database/sql html/template archive/zip os/signal flag \
       github.com/golang-migrate/migrate/v4 \
       github.com/golang-migrate/migrate/v4/database/postgres \
       github.com/golang-migrate/migrate/v4/source/iofs \
       github.com/jackc/pgx/v5 \
       github.com/jackc/pgx/v5/pgconn \
       github.com/jackc/pgx/v5/pgxpool \
       github.com/jackc/pgx/v5/stdlib \
       github.com/open-policy-agent/opa/v1/ast \
       github.com/open-policy-agent/opa/v1/rego
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM debian:bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends git ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY --from=terraform /out/terraform /usr/local/bin/terraform
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/migrate /usr/local/bin/migrate
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
