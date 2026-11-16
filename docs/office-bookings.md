# Office bookings in Discord

The office is booked as a whole: one booking at a time. A request **holds its
time as soon as it is made**, so nobody else can request an overlapping time,
but it is only confirmed once an approver approves it. All times are
**America/Toronto (Ottawa)**, including daylight saving.

## Website

https://office-tracker.ieeeuottawa.ca is the same calendar on the web, styled
with the IEEE uOttawa website's design system. People sign in with Discord; the
same roles decide who can book, approve, and open the admin pages (Manage
Server). Everything here and in Discord shares one database, so a booking made
in either place shows up in both, and Discord messages go out either way.

- Drag across empty time (or press **Book office time**) to request the office.
  Drag your own booking to move it, or its bottom edge to resize it.
- Approvers review from **Requests** or by clicking a pending booking, and can
  untick individual dates of a repeat before approving.
- **Google Calendar** subscribes to a private iCal feed of approved bookings.
- `/admin` replaces the old API-key management page. The management API now
  needs an admin's sign-in or the `API_KEY`; device endpoints are unchanged.

Styles are compiled from `web-src/` (`cd web-src && npm install && npm run build`)
into `internal/transport/http/web/assets/app.css`, which is committed.

## Members

| Do this | How |
|---|---|
| Book | `/book date start end purpose [repeat] [until]` — every field autocompletes, and start/end only suggest free times. Dates accept `tomorrow`, `fri`, `nov 20` or `2026-11-20`; times accept `14:00`, `2pm`, `noon`. An end at or before the start runs past midnight (22:00 → 02:00). |
| Book with clicks | **Book office time** under the calendar opens a private picker: day, start, length and repeat dropdowns showing only free options, then a short form for the purpose (and the last date of a repeat). Mistakes keep your choices. |
| Confirm | Both ways end on a confirm card. Nothing is requested until **Submit**. For a repeat, dates that are already taken are listed and left out. |
| Get the answer | A DM: approved (with **Change time** / **Cancel booking**), declined with the optional reason, revoked, or expired. If your DMs are closed, the bot mentions you once in the calendar channel instead. |
| Change or cancel | `/bookings` (or **My bookings**) lists your bookings. Pick one to **Change time**, **Cancel booking**, or **Cancel this & later dates** for a repeat. A change to an approved booking keeps the original until the new time is approved; a change to a pending request replaces it straight away. |
| See other weeks | `/calendar [date]` or **Other weeks** shows a private copy you can page through. |

## Approvers

- Each request gets **one card** in the officer channel with **Approve** and
  **Decline**, pinging the approver roles once. The card is edited in place as
  the request changes; there are no follow-up messages there.
- A repeating request is one card and one decision for all of its dates.
- **Decline** asks for an optional reason, which is sent to the requester.
- Approvers **cannot approve their own requests**; another approver must.
- If a request is still undecided 2 hours before it starts (and has waited at
  least 30 minutes), the approver roles get **one** reminder under its card. If
  nobody decides before the start, it **expires**, the time is freed and the
  requester gets a DM.
- `/office-review` (or **Review requests**) lists everything waiting, and
  **Revoke an approved booking** withdraws an approved booking (or it and the
  rest of its repeat) with an optional reason; the booker gets a DM.

## Admins

`/office-admin` is limited to members with Manage Server (or Administrator).

| Command | Effect |
|---|---|
| `/office-admin roles add role capability` | Let a role `book` or `approve`. |
| `/office-admin roles remove role capability` | Take it away. |
| `/office-admin roles list` | Show who can book and approve. |
| `/office-admin channel type channel` | Move the shared `calendar` (the old message is removed) or the `officers` request cards. |
| `/office-admin status` | Show the setup and any permissions the bot is missing in those channels. |

Until roles are assigned: anyone in the exec server can book, and the Signing
Officer role (`1103393189202763855`) approves. Once any role is given a
capability, only the assigned roles have it. Every button, dropdown and form
re-checks the member's roles, including buttons in DMs. The defaults are the
`office-tracker` channel for the calendar and `#booking-requests`
(`1555060913147486269`) for request cards.

The bot needs View Channel, Send Messages, Embed Links and Attach Files in the
calendar channel, and View Channel, Send Messages and Embed Links in the officer
channel (plus Mention @everyone, unless the approver roles are mentionable).

## Shared calendar

There is one shared calendar, in the calendar channel. It always shows the
current week: the image (pending bookings hatched and labelled "Pending", past
days greyed, a red line at the current time) and a text list of the week's
bookings for phones. It refreshes after every change and every five minutes.
Navigation is private, so nobody moves the calendar for everyone. Calendars left
in other channels by older versions are removed.

## Rules

Bookings start in the future, last at least 15 minutes and at most 24 hours,
and start within a year. A repeat is weekly or every 2 weeks with up to 26
dates, ending within a year. Wall times skipped or repeated by daylight-saving
changes are rejected. Adjacent bookings are allowed.

## Persistence and operations

Startup migrations rebuild the `bookings` and `booking_notifications` tables
once in place (keeping every row and ID) and add `booking_role_capabilities`
and `booking_settings`. Attendance records are untouched.

Notifications are saved in the same transaction as each booking change and
delivered every 30 seconds with backoff up to five minutes, including after a
restart. Saved message IDs and Discord nonces prevent duplicate pings and DMs.
Booking forms in progress live in memory for two hours; after a restart a member
starts the form again.

## Verification

```sh
go test ./...
go test -race ./internal/service ./internal/transport/discord/...
CALENDAR_PREVIEW_PATH=/tmp/office-calendar-preview.png go test ./internal/transport/discord/calendarimage -run TestRenderWeeklyCalendar -count=1
```

Tests use temporary databases and a mocked Discord HTTP transport.
