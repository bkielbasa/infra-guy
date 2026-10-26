# infra-guy

Server-rendered Terraform runs for GitHub repositories.

## Run

```
docker compose up -d
export DATABASE_URL=postgres://infraguy:infraguy@localhost:5432/infraguy?sslmode=disable
go run ./cmd/server
```

Terraform and git must be on `PATH`.

A `backend` or `cloud` block in the repository is overridden. infra-guy stores the state.

The server requires `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_WEBHOOK_SECRET`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, and `PUBLIC_BASE_URL`. `PUBLIC_BASE_URL` must be reachable by GitHub.

Register one public GitHub App. Set the callback URL to `{PUBLIC_BASE_URL}/github/callback`, the setup URL to `{PUBLIC_BASE_URL}/github/setup`, and the webhook URL to `{PUBLIC_BASE_URL}/webhooks/github`. Grant Contents read, Metadata read, and Pull requests read. Subscribe to Installation, Push, Pull request, and Repository.

A person signs in with GitHub, installs the app, grants repositories, and creates a project from that list.

## Security & Envelope Encryption

Sensitive artifacts and credentials in PostgreSQL (`context_entries`, `cloud_integrations`, `projects.state_token_ciphertext`, `states.data`, `runs.plan_file`, and `runs.plan_json`) are protected at rest using versioned AES-256-GCM envelope encryption (`v1:<key-id>:<hex-wrapped-dek>:<nonce+ciphertext>`) bound to per-record Additional Authenticated Data (AAD). Session tokens (`sessions.id`) and project backend state tokens (`projects.state_token_hash`) are stored as SHA-256 digests (`sha256:<hex>`).

### Keyring & Zero-Downtime Key Rotation

Configure either a single key via `CONTEXT_ENCRYPTION_KEY` or a multi-key keyring via `ENCRYPTION_KEYRING` and `ENCRYPTION_ACTIVE_KEY_ID`:

```bash
export ENCRYPTION_KEYRING="k2:<64-char-hex-key>,k1:<64-char-hex-key>"
export ENCRYPTION_ACTIVE_KEY_ID="k2"
```

To re-encrypt all ciphertexts and seal any legacy records under the active key:

```bash
go run ./cmd/migrate rotate-keys
```

### External Secret References

Secret context entries (`secret_env`, `secret_file`) and Cloud Integration static secrets support runtime resolution from external secret managers without storing secret material in PostgreSQL:

- `env://VAR_NAME`
- `vault://secret/data/path#field`
- `aws-sm://secret-id#jsonKey`
- `ssm:///path/to/parameter`

## Observability (OpenTelemetry & Grafana)

### Metrics & Distributed Traces

- **Prometheus Metrics**: Exposed at `GET /metrics` (HTTP request rate/duration/in-flight, Terraform run & step counters/histograms, queue depth, active workers, OPA policy evaluations, cloud credential mints, state lock conflicts, drift status, and PostgreSQL pool gauges).
- **OTLP Distributed Tracing**: Set `OTEL_EXPORTER_OTLP_ENDPOINT` (e.g. `http://otel-collector:4318`), `OTEL_SERVICE_NAME` (default `infra-guy`), and `OTEL_TRACES_SAMPLER_ARG` (default `1.0`) to export W3C TraceContext spans across HTTP requests, worker job execution, `git`, `terraform`, OPA evaluations, and cloud STS calls.

### Grafana Dashboard

A ready-to-import Grafana dashboard (`uid: infra-guy-overview`) is provided at [`deploy/grafana/dashboards/infra-guy-overview.json`](deploy/grafana/dashboards/infra-guy-overview.json) along with file-based provisioning manifests under [`deploy/grafana/provisioning/`](deploy/grafana/provisioning/).

- **UI Import**: In Grafana, navigate to **Dashboards → New → Import**, upload `deploy/grafana/dashboards/infra-guy-overview.json`, and select your Prometheus datasource.
- **Declarative Provisioning**: Mount `deploy/grafana/provisioning/` into `/etc/grafana/provisioning` and `deploy/grafana/dashboards/` into `/var/lib/grafana/dashboards`.

