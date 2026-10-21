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
