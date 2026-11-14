# Server deployment and USB relay

The Discord bot, scheduler, API and SQLite database run in `ieee-office-tracker`
on ieeeuottawa-server. The Pi runs only `office-reader-relay.service`, forwarding
the ESP32's USB frames to `http://100.116.212.50:8080/api/reader/request` over
Tailscale. Firmware and the USB request/response format are unchanged.

The endpoint requires API key authentication even if the rest of the API does not.
The relay holds only `API_KEY`, `READER_SERVER_URL` and `USB_READER_PORT` in
`/etc/office-reader-relay.env` (root-readable); it has no Discord token or database.
All request IDs are preserved. SQLite receipts commit with attendance, so retrying
a request after a lost response does not toggle attendance twice. On connectivity
failure the reader receives an error; there is no offline scan queue.

Build the server with `docker build -f build/Dockerfile -t office-tracker:server-migration .`.
Build the relay for the Pi with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o reader-relay ./cmd/reader-relay`.
Install the relay under `/opt/office-reader-relay`, its unit from `deploy/`, and
configure its environment file. Preserve the stable serial path and dialout access.

Deploy with `docker compose -f deploy/compose.yaml up -d`, supplying the original
production environment in ignored `deploy/tracker.env` and a consistent SQLite
backup in `deploy/data/office.db`. Stop the old bot before the final database copy.
Enable the relay only after the server starts. Never run both USB consumers.
The container API is bound to loopback and the server's Tailscale address.
Docker networks preserve access from the existing Cloudflare/monitoring stack.

Check container health, Discord gateway connection logs and fresh ESP32 heartbeats
at `/api/devices/health` using `X-API-Key`. A physical card scan is the final hardware
acceptance test. The old Pi service is disabled, with its binary and migration-time
database snapshot retained for rollback. To roll back after new writes, stop the
server and copy its current database back first; do not restore a stale snapshot.
