# infra-guy

Server-rendered Terraform runs for GitHub repositories.

## Run

```
docker compose up -d
export DATABASE_URL=postgres://infraguy:infraguy@localhost:5432/infraguy?sslmode=disable
go run ./cmd/server
```

Terraform and git must be on `PATH`.

The repository must not contain a `backend` block.

Create a GitHub webhook:

- URL: `{PUBLIC_BASE_URL}/webhooks/github/{project id}`
- secret: the project webhook secret
- content type `application/json`
- events Pull requests and Pushes

`PUBLIC_BASE_URL` must be reachable by GitHub.
