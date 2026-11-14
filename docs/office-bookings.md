# Office bookings in Discord

Office bookings use one shared calendar in the configured exec server. All input
and calendar labels use **America/Toronto (Ottawa)**, including daylight saving.

## Commands and buttons

- A calendar is created automatically in the configured `office-tracker` channel.
  Requests, approvals, declines and cancellations refresh it immediately.
- `/calendar [date:YYYY-MM-DD]` creates one shared calendar per channel, or updates
  the existing one. The bot needs View Channel, Send Messages, Embed Links and
  Attach Files permissions. Navigation changes the shared calendar for everyone.
- **Previous week**, **This week**, **Next week** and **Refresh** update the PNG.
  Approved reservations appear as coloured time blocks with the booker's name.
- **Book office time** or `/book` opens a form for purpose, date, start and end.
  Use 24-hour times; `24:00` means midnight at the end of the selected date.
- **My bookings** or `/bookings` privately shows your requests, their status, and
  a cancellation button for your own upcoming pending or approved bookings.
- **Review requests** or `/booking-review` opens a private paginated queue.
  Only members whose interaction includes Signing Officer role
  `1103393189202763855` can review, approve or decline. Administrator permission
  alone does not bypass this role check.

Requests start pending. Pending requests can overlap; they do not reserve time.
Approval is atomic and rejects overlaps with an approved booking. Adjacent slots
are allowed. Requests must be in the future, last at least 15 minutes, finish on
the selected date (or at midnight), and start no more than one year ahead.
Ambiguous or nonexistent times around daylight-saving changes are rejected.

The shared calendar displays the number of pending requests. Notifications go to
`#booking-requests` (`1555060913147486269`). New requests mention Signing Officers
and include Approve/Decline buttons. Decisions mention the requester. Cancellations
are posted without a role ping. The original request card updates to its resolved
status and its buttons are removed. Booking status is also available in My bookings.
Requests whose start time passes before approval leave the review queue and show
as expired in My bookings. My bookings includes the last month and future requests.

Notification events are saved atomically with booking changes in SQLite. Failed
deliveries retry every 30 seconds with backoff up to five minutes, including after
a restart. Saved message IDs and Discord nonces limit duplicate notifications.
Existing upcoming pending requests are added to the notification queue on upgrade.

## Persistence and operations

The startup migration adds `bookings` and `booking_calendars` to the existing
SQLite database. Attendance records are untouched. Booking decisions record the
reviewer's Discord user ID and time. Display names are captured when requested.

Calendar messages and the selected week survive a bot restart. They refresh after
requests, approvals, declines and cancellations, and every five minutes. The `office-tracker` calendar is recreated if deleted, including after a restart.
Other deleted calendar messages are forgotten; `/calendar` recreates those when needed.

The image contains the time grid and named booking blocks. The date range is in
the Discord embed. There are no branding, instructions, city or timezone labels
on the calendar.

The image uses a Go PNG renderer with embedded Go fonts, requiring no browser,
image service or additional OS packages. The default grid is 08:00–22:00 and
expands to include earlier or later reservations. Full booking details are also
available in private booking/review cards; the shared calendar shows the reserved time blocks with names and purposes.

## Verification

```sh
go test ./...
go test -race ./internal/service ./internal/transport/discord/...
CALENDAR_PREVIEW_PATH=/tmp/office-calendar-preview.png go test ./internal/transport/discord/calendarimage -run TestRenderWeeklyCalendar -count=1
```

Tests use temporary databases and a mocked Discord HTTP transport. They cover
role and guild checks, owner-only cancellation, overlapping/concurrent approvals,
interaction replay, DST, saved navigation, multipart image uploads and PNG output.
