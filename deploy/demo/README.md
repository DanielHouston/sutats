# Public demo

`compose.yaml` runs sutats with invented data, so you can show it to people
without a Kubernetes cluster. It shows two groups of components with HTTP
probes and Kubernetes checks, an active incident, scheduled maintenance,
past incidents and 90 days of history. Clicking a day on a bar opens that
day in 15-minute cells (hours on a phone).

## What it needs

- Docker with Compose, or a platform that runs Compose files (Coolify,
  Portainer, Dokku and others).
- About 250 MB of memory: around 200 MB for TimescaleDB and 20 MB for
  sutats.
- A reverse proxy with TLS in front of the app's port 8080. Platforms such
  as Coolify provide one; the compose file only exposes the port to it.
- The image. The default is `ghcr.io/danielhouston/sutats:1`, which follows
  the latest 1.x release.

The demo makes no outgoing requests: mock mode has no cluster, and its
probe results are invented too.

## Run it on Coolify

1. Create a resource from this repository with the Docker Compose build
   pack, and set the compose file to `/deploy/demo/compose.yaml`. Or create
   an empty Docker Compose resource and paste the file in.
2. Give the `sutats` service a domain ending in the app's port, such as
   `https://status-demo.example.com:8080`. The `:8080` tells Coolify which
   port to route to; visitors still use plain https. Leave `db` without a
   domain.
3. In the environment variables Coolify lists from the file, change
   `POSTGRES_PASSWORD` and, if you like, `DEMO_TITLE`.
4. Deploy. The first start takes about a minute (see below).

Coolify shows `sutats` as running without a health status: the image has
no shell for a container health check. That does not affect routing.

## Run it on your own machine

The local override publishes the port on the host:

```bash
docker compose -f deploy/demo/compose.yaml -f deploy/demo/compose.local.yaml up -d
```

Open http://localhost:8080. On first start the database is filled with 90
days of samples, which takes about a minute before the page answers. After
that, the demo records its own changes as it runs, the way a real install
would.

## Settings

Set these in the environment or in a `.env` file next to `compose.yaml`.

| Variable            | Default                           | Meaning |
| ------------------- | --------------------------------- | ------- |
| `DEMO_TITLE`        | `Acme`                            | Title at the top of the page. |
| `PORT`              | `8080`                            | Port published on the host, with the local override only. |
| `POSTGRES_PASSWORD` | `sutats-demo`                     | Database password. The database is not published, but change it anyway. |
| `SUTATS_IMAGE`      | `ghcr.io/danielhouston/sutats:1`  | Image to run; pin a version such as `:1.0.2` to stop updates. |
| `DB_MEMORY`         | `512MB`                           | Memory TimescaleDB sizes itself for. Read only when the database is first created. |
| `DEMO_HOST`         | unset                             | Host of the main page, such as `status-demo.example.com`. |
| `DEMO_PAGE_HOST`    | unset                             | Host template for the team pages, such as `{slug}-status-demo.example.com`. |

The mock has two team pages, `edge` and `platform`. To show them, set both
host variables and point DNS and the proxy at the demo for each page host.
The main page then links to them. Without them the demo is one page with
every component on it.

Keep the page hosts one level under your domain, as in
`{slug}-status-demo.example.com`, not `{slug}.status-demo.example.com`. A
wildcard certificate covers one level only, and some providers issue nothing
deeper: Cloudflare's free certificate for `*.example.com` does not cover
`edge.status-demo.example.com`, and browsers then fail with
`ERR_SSL_VERSION_OR_CIPHER_MISMATCH`.

In Coolify, no wildcard route is needed: list the three hosts as the
`sutats` service's domains, separated by commas:

```text
https://status-demo.example.com:8080,https://edge-status-demo.example.com:8080,https://platform-status-demo.example.com:8080
```

with `DEMO_HOST=status-demo.example.com` and
`DEMO_PAGE_HOST={slug}-status-demo.example.com`. Coolify applies new
domains when it deploys, so redeploy after changing them.

## Health checks

The image has no shell, so the container has no health check of its own.
Where the platform or proxy can check over HTTP, point it at:

- `GET /healthz`: `ok` while the process is up.
- `GET /readyz`: `ok` once it is ready to serve.

## Starting over

The history keeps growing from the moment of the first start. To reset it
to a fresh 90-day story:

```bash
docker compose -f deploy/demo/compose.yaml -f deploy/demo/compose.local.yaml down -v
docker compose -f deploy/demo/compose.yaml -f deploy/demo/compose.local.yaml up -d
```
