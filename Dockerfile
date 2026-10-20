FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM debian:bookworm-slim
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl unzip \
  && curl -fsSL "https://releases.hashicorp.com/terraform/1.9.8/terraform_1.9.8_linux_${TARGETARCH}.zip" -o /tmp/terraform.zip \
  && unzip -o /tmp/terraform.zip -d /usr/local/bin \
  && rm /tmp/terraform.zip \
  && apt-get purge -y curl unzip \
  && apt-get autoremove -y \
  && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
