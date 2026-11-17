# Hosted test stack

Every release ships the hosted service in a form a client repo's tests can
start: the `api`, `storage` and `mock-oidc` binaries, and a bundle that runs
them against Postgres and MinIO in Docker. It replaces the old stand-in mode.
**It is for tests only:** it signs everything with the public test keys in the
bundle's `test-only-keys/`.

## Release assets

The self-hosted server's package, `futo-notes-server/<version>`, keeps exactly
its files. The hosted assets are a package of their own,
`futo-notes-hosted/<version>`, at
`https://gitlab.futo.org/api/v4/projects/547/packages/generic/futo-notes-hosted/<version>/<asset>`:

| Asset | What |
|---|---|
| `futo-notes-{api,storage,mock-oidc}_<version>_<os>_<arch>[.exe]` | Linux amd64/arm64/armv6, macOS amd64/arm64, Windows amd64, like the server |
| `futo-notes-hosted-stack_<version>.tar.gz` | the bundle |
| `futo-notes-hosted_<version>_checksums.txt` | SHA-256 of every asset above |

The bundle unpacks to `futo-notes-hosted-stack/`:

- `hosted-stack.sh`, the launcher. Its header documents every variable.
- `compose.yml`, Postgres 18 and MinIO, and a one-shot that creates the
  buckets `notes-us-east`, `notes-eu-central` and `notes-ap-southeast`.
- `test-only-keys/`, the committed dev keys (`dev/hosted-keys` in this repo).

The launcher needs bash, curl and Docker with Compose. It runs on Linux and
macOS, not Windows.

## Using it from a client repo

1. **Pin.** Pin the version, and the sha256 of the bundle and of the three
   binaries for each platform you run on. Take the sha256s from the checksums
   file or the package API, as `scripts/lib/sync-server.mjs refresh` does for
   the server.
2. **Up.** Unpack the bundle, then:

   ```bash
   export HOSTED_STACK_API_BIN=/path/to/futo-notes-api_<version>_linux_amd64
   export HOSTED_STACK_STORAGE_BIN=/path/to/futo-notes-storage_<version>_linux_amd64
   export HOSTED_STACK_MOCK_OIDC_BIN=/path/to/futo-notes-mock-oidc_<version>_linux_amd64
   futo-notes-hosted-stack/hosted-stack.sh up > hosted.env
   ```

   `up` waits until every service answers. It prints only `KEY=value` lines on
   stdout, and the same lines again from `hosted-stack.sh env`:

   ```
   HOSTED_STACK_API_URL=http://127.0.0.1:3125
   HOSTED_STACK_ISSUER=http://127.0.0.1:8192
   HOSTED_STACK_REGIONS=us-east,eu-central,ap-southeast
   HOSTED_STACK_STORAGE_URL_US_EAST=http://127.0.0.1:3115
   HOSTED_STACK_STORAGE_URL_EU_CENTRAL=http://127.0.0.1:3116
   HOSTED_STACK_STORAGE_URL_AP_SOUTHEAST=http://127.0.0.1:3117
   ```

   The defaults stay clear of 3005. `HOSTED_STACK_API_PORT`,
   `HOSTED_STACK_STORAGE_PORT` (first of three), `HOSTED_STACK_OIDC_PORT`,
   `HOSTED_STACK_POSTGRES_PORT` and `HOSTED_STACK_MINIO_PORT` move them.
   `HOSTED_STACK_DIR` holds the state and `logs/<service>.log`. The API accepts
   app redirects on `tech.futo.notes` and `tech.futo.notes.dev`, and on
   loopback. Under Docker-in-Docker (`DOCKER_HOST=tcp://docker:2376`) the
   launcher reaches the containers at that host.
3. **Sign in.** The app points at `HOSTED_STACK_API_URL` and starts an app
   sign-in at `/api/auth/oidc/start`, with its `redirect_uri`, PKCE challenge
   and `state`. That redirects to `HOSTED_STACK_ISSUER/authorize?...`, which
   shows a form. A test does not fill it in. It replaces the path with
   `/test/authorize`, keeps the query, and adds `sub=<test user>` (and
   optionally `email=` and `name=`). Then it follows the redirects with a
   cookie jar, back through the API's callback to the app's `redirect_uri`,
   which carries the one-time `code`. The app swaps it at
   `POST /api/auth/oidc/token`. Every new `sub` is a new account. The next
   steps are choosing a region, getting a storage token, uploading, and
   creating objects with the receipt, as in `docs/openapi-hosted.yaml`.
   `TestReleasedStack` in `e2e/hosted/released_test.go` walks them in Go.
4. **Subscribe.** Billing is on (`"billing": true` in `GET /`), with the mock
   web payment provider, so a new account reads but cannot write: writes
   answer `402 subscription_required` and storage tokens are read-only
   (uploads get `403 write_not_allowed`). To subscribe a test user,
   `POST /api/billing/checkout` with its session and then `POST` the `url`
   it returns, as is: the mock completes the checkout and tells the API
   through its signed webhook, as a real provider would. Exchange the
   storage refresh token again afterwards for a token that may upload.
   `GET /api/billing` shows the subscription. The API also runs with
   `DEV_UI`, whose `/dev/billing/*` routes lapse, expire or refund it and
   move the lapse clock (see `/dev`).
5. **Down.** `hosted-stack.sh down`, with the same `HOSTED_STACK_DIR`. It stops
   the processes `up` started (only if they still run the binary it started)
   and removes the Compose project `futo-notes-hosted-stack` with its data. A
   failed `up` cleans up after itself, after printing each service's log tail.

Only one stack per machine runs at a time: the Compose project name is fixed.
The API defaults to port 3125, clear of 3005 and of the 3105 this repo's
comparison harness uses.

## In this repo

`mise run hosted-stack:smoke` builds a release for this machine only
(`FUTO_RELEASE_HOST_ONLY=1 scripts/build-release.sh <version>`, which a client
repo can also use to test against an unreleased server) into a temporary
directory, runs `scripts/hosted-stack-smoke.sh` on it (verify checksums, unpack, `up`,
`TestReleasedStack`, `down`), and leaves nothing running. CI's
`test:hosted-stack` job does the same with the pipeline's `build:binaries`
artifacts, and `publish:binaries` waits for it. The bundle's sources are
`scripts/hosted-stack/`, and its keys come from `dev/hosted-keys/` at build
time.

`test:hosted-stack` runs the stack in the job's Docker-in-Docker service where
the runner provides one. Some runners mount the host's Docker socket into every
container instead, which stops Docker-in-Docker from starting. There the job
starts its containers on the shared host daemon with a Compose project and
ports derived from the job ID, so concurrent pipelines don't collide, and
passes its own container id as `HOSTED_STACK_JOIN_CONTAINER`: the launcher
connects the job to the stack's network and reaches Postgres and MinIO there,
since the host's firewall drops traffic to ports published on another network.
