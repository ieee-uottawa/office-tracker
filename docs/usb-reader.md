# USB card reader

The ESP32-C3 sends versioned JSON lines prefixed with `@office:` at 115200 baud.
`USB_READER_PORT` selects a serial device, preferably its stable `/dev/serial/by-id/` path.
The service account needs serial-device access (the deployed unit adds `dialout`).

Operations are `scan`, `checkout_all`, and `heartbeat`. Each request includes a random
boot nonce and sequence number in `id`, and an object in `data`. Responses echo `id`
and include `status`, plus `action` and `jingle` for successful scans.

Attendance changes and response receipts commit atomically to `reader_requests` in
SQLite. Replayed IDs return the saved result even after restart; reusing an ID for
another payload returns 409. Diagnostics such as `Card:` never trigger attendance.
The normal attendance callback refreshes Discord after committed changes.

HTTP and USB start before Discord initialization. Discord startup retries without
stopping local scans. The reader does not use Wi-Fi or require network credentials.
Health is available at `/api/devices/health` with `transport: usb`.

Stop `office-tracker.service` before flashing or opening a second serial monitor.
The deployed udev rule excludes this specific reader from modem probing. The unit
clears serial hang-up/reset behavior before starting. USB reconnects are automatic.

Rollback binary, database snapshot, original source changes, and a full pre-change
reader flash image are saved in `/var/backups/office-tracker/before-usb-20261002T203605/`.
Restore firmware only with the backend stopped. Do not roll back the live database
blindly: attendance and registrations may have changed since the snapshot.

The server deployment and Pi relay are described in [server-migration.md](server-migration.md).
