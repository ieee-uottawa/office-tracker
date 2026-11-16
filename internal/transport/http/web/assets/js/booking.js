// Office booking calendar: week/day/month views, drag to book, drag your own
// booking to move or resize it, and the request/review side panel.
;(function () {
  'use strict'
  const { api, toast, escapeHTML: esc } = window.Office
  const CFG = window.OFFICE || {}
  const USER = CFG.user || {}
  const SNAP = 15
  const DAY = 1440
  const $ = (s, el = document) => el.querySelector(s)
  const $$ = (s, el = document) => [...el.querySelectorAll(s)]

  // ---------- dates (all values are Ottawa wall time from the server) ----------
  const pad = (n) => String(n).padStart(2, '0')
  const ymd = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  const parse = (s) => { const [y, m, d] = s.split('-').map(Number); return new Date(y, m - 1, d) }
  const addDays = (s, n) => { const d = parse(s); d.setDate(d.getDate() + n); return ymd(d) }
  const weekStart = (s) => { const d = parse(s); return addDays(s, -((d.getDay() + 6) % 7)) }
  const DOW = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']
  const MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
  const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December']
  const fmtDay = (s) => { const d = parse(s); return `${DOW[d.getDay()]}, ${MON[d.getMonth()]} ${d.getDate()}` }
  const clock = (m) => {
    m = ((m % DAY) + DAY) % DAY
    const h = Math.floor(m / 60), mm = m % 60, h12 = ((h + 11) % 12) + 1
    return `${h12}${mm ? ':' + pad(mm) : ''} ${h < 12 ? 'AM' : 'PM'}`
  }
  const endClock = (start, end) => (end === DAY ? 'midnight' : clock(end) + (end > DAY ? ' (next day)' : ''))
  const span = (b) => `${clock(b.start)} – ${endClock(b.start, b.end)}`
  const when = (b) => `${fmtDay(b.date)} · ${span(b)}`
  const duration = (m) => { const h = Math.floor(m / 60), r = m % 60; return h ? `${h}h${r ? ' ' + r + 'm' : ''}` : `${r}m` }
  const STATUS = { pending: 'Pending', approved: 'Approved', declined: 'Declined', revoked: 'Revoked', cancelled: 'Cancelled', expired: 'Expired', replaced: 'Moved' }
  const statusText = (b) => (b.replaces && b.status === 'pending' ? 'Change pending' : STATUS[b.status] || b.status)

  // ---------- state ----------
  const narrow = () => matchMedia('(max-width: 767px)').matches
  const state = { view: narrow() ? 'day' : 'week', anchor: CFG.today, today: CFG.today, now: 0, bookings: [], panel: null, draft: null, loading: false }
  const isPast = (date, minute) => date < state.today || (date === state.today && minute <= state.now)
  const active = (b) => b.status === 'pending' || b.status === 'approved'
  const canMove = (b) => b.mine && active(b) && !b.replaces && !isPast(b.date, b.start) && !state.bookings.some((x) => x.replaces === b.id && x.status === 'pending')

  function range() {
    if (state.view === 'day') return [state.anchor, state.anchor]
    if (state.view === 'week') { const ws = weekStart(state.anchor); return [ws, addDays(ws, 6)] }
    const d = parse(state.anchor)
    const first = weekStart(ymd(new Date(d.getFullYear(), d.getMonth(), 1)))
    return [first, addDays(first, 41)]
  }

  let loadSeq = 0
  async function load() {
    const [from, to] = range()
    const seq = ++loadSeq
    try {
      // Include the previous day so overnight bookings show their second half.
      const data = await api(`/api/web/bookings?from=${addDays(from, -1)}&to=${to}`)
      if (seq !== loadSeq) return
      state.bookings = data.bookings
      state.today = data.today
      state.now = data.now
      if (!state.anchor) state.anchor = data.today
      render()
    } catch (e) {
      if (seq === loadSeq) $('[data-calendar]').innerHTML = `<p class="py-12 text-muted-foreground">${esc(e.message)}</p>`
    }
  }

  // ---------- rendering ----------
  function rangeLabel() {
    const d = parse(state.anchor)
    if (state.view === 'month') return `${MONTHS[d.getMonth()]} ${d.getFullYear()}`
    if (state.view === 'day') return fmtDay(state.anchor)
    const ws = parse(weekStart(state.anchor)), we = parse(addDays(weekStart(state.anchor), 6))
    return ws.getMonth() === we.getMonth()
      ? `${MON[ws.getMonth()]} ${ws.getDate()} – ${we.getDate()}, ${we.getFullYear()}`
      : `${MON[ws.getMonth()]} ${ws.getDate()} – ${MON[we.getMonth()]} ${we.getDate()}, ${we.getFullYear()}`
  }

  function render() {
    if (!state.anchor) return
    $('[data-range]').textContent = rangeLabel()
    $$('[data-view]').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.view === state.view)))
    state.view === 'month' ? renderMonth() : renderGrid()
  }

  // Pieces of bookings that fall on a day (overnight bookings appear on both days).
  function piecesOn(date) {
    const out = []
    for (const b of state.bookings) {
      if (!active(b)) continue
      if (b.date === date) out.push({ b, from: b.start, to: Math.min(b.end, DAY), first: true })
      else if (b.end > DAY && addDays(b.date, 1) === date) out.push({ b, from: 0, to: b.end - DAY, first: false })
    }
    return out
  }

  const top = (m) => `calc(var(--hour) * ${m / 60})`
  const tall = (a, z) => `calc(var(--hour) * ${(z - a) / 60} - 2px)`

  function renderGrid() {
    const host = $('[data-calendar]')
    const prevScroll = $('[data-scroller]', host)?.scrollTop
    const ws = state.view === 'week' ? weekStart(state.anchor) : state.anchor
    const days = state.view === 'week' ? Array.from({ length: 7 }, (_, i) => addDays(ws, i)) : [state.anchor]
    const cols = `3.5rem repeat(${days.length}, minmax(${days.length > 1 && narrow() ? '7rem' : '0'}, 1fr))`
    const selectedLead = state.panel && state.panel.lead
    const ghosts = new Set(state.bookings.filter((b) => b.replaces && b.status === 'pending').map((b) => b.replaces))

    let head = `<div class="sticky top-0 z-10 grid gap-px bg-foreground/10" style="grid-template-columns:${cols}"><div class="bg-background"></div>`
    for (const d of days) {
      const dt = parse(d), isToday = d === state.today
      head += `<div class="flex items-baseline gap-2 bg-background px-3 pb-3 pt-2 ${isToday ? 'shadow-[inset_0_2px_0_hsl(var(--primary))]' : ''}">
        <span class="font-mono text-xs uppercase ${isToday ? 'text-primary' : 'text-muted-foreground'}">${DOW[dt.getDay()]}</span>
        <span class="font-display text-2xl font-medium tabular-nums ${d < state.today ? 'text-muted-foreground' : isToday ? 'text-primary' : ''}">${dt.getDate()}</span></div>`
    }
    head += '</div>'

    let body = `<div class="grid gap-px bg-foreground/10" style="grid-template-columns:${cols}"><div class="relative bg-background" style="height:calc(var(--hour) * 24)">`
    for (let h = 1; h < 24; h++) body += `<span class="absolute right-2 -translate-y-1/2 font-mono text-[0.7rem] text-muted-foreground" style="top:${top(h * 60)}">${clock(h * 60)}</span>`
    body += '</div>'
    for (const d of days) {
      const bookable = USER.book && d >= state.today
      body += `<div class="cal-lines relative bg-background ${bookable ? 'cursor-crosshair' : ''}" data-col="${d}" style="height:calc(var(--hour) * 24);touch-action:pan-y">`
      if (d === state.today) body += `<div class="pointer-events-none absolute -left-px right-0 z-[4] h-0.5 bg-primary" style="top:${top(state.now)}"><span class="absolute -left-1 -top-[3px] h-2 w-2 rounded-full bg-primary"></span></div>`
      for (const p of piecesOn(d)) body += eventHTML(p, ghosts.has(p.b.id), selectedLead === p.b.lead)
      const dr = state.draft
      if (dr && dr.date === d) {
        const bad = dr.bad
        body += `<div class="pointer-events-none absolute inset-x-1 z-[5] rounded-sm border-2 ${bad ? 'border-dashed border-foreground/40' : 'border-primary'} bg-primary/10 px-2 py-1 font-mono text-[0.7rem] font-medium text-primary" style="top:${top(dr.start)};height:${tall(dr.start, Math.min(dr.end, DAY))}">${clock(dr.start)} – ${endClock(dr.start, dr.end)}${bad ? ' · taken' : ''}</div>`
      }
      body += '</div>'
    }
    body += '</div>'
    host.innerHTML = `<div data-scroller class="cal max-h-[calc(100vh-12rem)] min-h-[28rem] overflow-auto overscroll-contain border-y border-border">${head}${body}</div>`
    const scroller = $('[data-scroller]', host)
    scroller.scrollTop = prevScroll ?? hourPx() * (days.includes(state.today) ? Math.max(0, Math.min(state.now / 60 - 2, 15)) : 7.5)
  }

  function eventHTML(p, ghost, selected) {
    const b = p.b
    const past = isPast(p.first ? b.date : addDays(b.date, 1), p.to)
    const look = b.status === 'pending' ? 'border border-dashed border-primary/60 bg-background text-foreground' : b.mine ? 'bg-primary text-primary-foreground' : 'bg-muted text-foreground'
    const short = p.to - p.from <= 45
    const movable = p.first && canMove(b)
    return `<div data-id="${b.id}" role="button" tabindex="0" aria-label="${esc(b.title)}, ${esc(when(b))}, ${statusText(b)}"
      class="absolute inset-x-1 z-[2] select-none overflow-hidden rounded-sm px-2 py-1 transition-shadow hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${look} ${past ? 'opacity-50' : ''} ${ghost ? 'pointer-events-none opacity-30' : ''} ${selected ? 'ring-2 ring-ring ring-offset-1 ring-offset-background' : ''} ${movable ? 'cursor-grab' : 'cursor-pointer'}"
      style="top:${top(p.from)};height:${tall(p.from, p.to)}">` +
      (b.status === 'pending' && !short ? `<div class="kicker text-[0.65rem] text-primary">${b.replaces ? 'Change' : 'Pending'}</div>` : '') +
      `<div class="truncate text-sm font-medium leading-tight">${esc(b.title)}</div>` +
      (short ? '' : `<div class="truncate font-mono text-[0.7rem] opacity-75">${span(b)} · ${esc(b.mine ? 'You' : b.name)}</div>`) +
      (movable && b.end <= DAY ? '<div data-resize class="absolute inset-x-0 bottom-0 h-2 cursor-ns-resize"></div>' : '') +
      '</div>'
  }

  function renderMonth() {
    const [first] = range()
    const month = parse(state.anchor).getMonth()
    let html = '<div class="grid grid-cols-7 gap-px border-y border-border bg-foreground/10">' +
      ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'].map((x) => `<div class="bg-background px-2 py-2 font-mono text-xs uppercase text-muted-foreground">${x}</div>`).join('')
    for (let i = 0; i < 42; i++) {
      const day = addDays(first, i), dt = parse(day)
      const items = state.bookings.filter((b) => b.date === day && active(b)).sort((a, b) => a.start - b.start)
      html += `<button type="button" data-day="${day}" class="flex min-h-[5rem] flex-col items-stretch gap-1 bg-background p-2 text-left transition-colors hover:bg-muted/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring md:min-h-[7rem] ${dt.getMonth() !== month ? 'text-muted-foreground' : ''}">
        <span class="font-display text-lg font-medium tabular-nums ${day === state.today ? 'text-primary underline underline-offset-4' : ''}">${dt.getDate()}</span>` +
        items.slice(0, 3).map((b) => `<span class="hidden truncate rounded-sm px-1.5 text-xs md:block ${b.status === 'pending' ? 'border border-dashed border-primary/60' : b.mine ? 'bg-primary text-primary-foreground' : 'bg-muted text-foreground'}">${clock(b.start)} ${esc(b.title)}</span>`).join('') +
        (items.length ? `<span class="font-mono text-[0.7rem] text-muted-foreground md:hidden">${items.length} booked</span>` : '') +
        (items.length > 3 ? `<span class="hidden font-mono text-[0.7rem] text-muted-foreground md:block">+${items.length - 3} more</span>` : '') +
        '</button>'
    }
    $('[data-calendar]').innerHTML = html + '</div>'
  }

  // ---------- panel ----------
  const panel = $('[data-panel]')
  const scrim = $('[data-scrim]')
  function openPanel(p) {
    state.panel = p
    panel.classList.remove('translate-y-full', 'md:translate-x-full')
    scrim.classList.toggle('hidden', !narrow())
    drawPanel()
    render()
  }
  function closePanel() {
    state.panel = null
    state.draft = null
    panel.classList.add('translate-y-full', 'md:translate-x-full')
    scrim.classList.add('hidden')
    panel.innerHTML = ''
    render()
  }

  function frame({ eyebrow, title, body, foot }) {
    panel.innerHTML = `<div class="flex items-start justify-between gap-4 border-b border-border px-6 pb-5 pt-6">
        <div class="min-w-0 space-y-3"><span class="eyebrow">${eyebrow}</span>
        <h2 id="panel-title" class="break-words font-display text-3xl font-medium leading-[1.1] tracking-tight">${title}</h2></div>
        <button type="button" class="btn btn-icon -mr-2 shrink-0 text-primary hover:bg-card" data-close aria-label="Close"><svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg></button></div>
      <div class="flex flex-1 flex-col gap-6 overflow-auto px-6 py-6">${body}</div>` +
      (foot ? `<div class="flex gap-2 border-t border-border px-6 pb-4 pt-4 [&>*]:flex-1">${foot}</div>` : '')
  }
  const loadingFrame = (eyebrow) => frame({ eyebrow, title: '&nbsp;', body: '<p class="meta">Loading…</p>' })
  const kv = (k, v) => `<div class="grid grid-cols-[6rem_1fr] gap-4 border-b border-border py-3 first:border-t"><dt class="meta pt-0.5">${k}</dt><dd class="min-w-0 text-sm">${v}</dd></div>`
  const field = (id, label, control) => `<div class="space-y-2"><label class="label" for="${id}">${label}</label>${control}</div>`
  const row = (left, right) => `<li class="flex items-center justify-between gap-4 border-b border-border py-3 first:border-t">${left}${right}</li>`
  const sectionHead = (title, meta = '') => `<div class="mb-2 flex items-baseline justify-between gap-4 border-b border-border pb-3"><h3 class="font-display text-xl font-medium">${title}</h3><span class="meta">${meta}</span></div>`

  function drawPanel() {
    const p = state.panel
    if (!p) return
    ;({ new: drawNew, details: drawDetails, mine: drawMine, requests: drawRequests, google: drawGoogle })[p.type](p)
  }

  // Two-step confirmation (no browser dialogs).
  function armed(btn, label) {
    if (btn.dataset.armed) return true
    btn.dataset.armed = '1'
    btn.textContent = label
    btn.classList.remove('btn-outline')
    btn.classList.add('btn-default')
    return false
  }
  async function busy(btn, fn) {
    btn.disabled = true
    try { await fn() } catch (e) { toast(e.message) } finally { btn.disabled = false }
  }

  // New request
  const timeOptions = (sel, from, to, base = 0) => {
    let o = ''
    for (let m = from; m <= to; m += SNAP) o += `<option value="${m}" ${m === sel ? 'selected' : ''}>${m === DAY ? 'midnight' : clock(m)}${m > DAY ? ' (next day)' : ''}${base ? ' · ' + duration(m - base) : ''}</option>`
    return o
  }
  const dateOptions = (sel) => {
    let o = '', seen = false
    for (let i = 0; i < 180; i++) {
      const d = addDays(state.today, i)
      seen = seen || d === sel
      o += `<option value="${d}" ${d === sel ? 'selected' : ''}>${i === 0 ? 'Today, ' : i === 1 ? 'Tomorrow, ' : ''}${fmtDay(d)}</option>`
    }
    return seen ? o : `<option value="${sel}" selected>${fmtDay(sel)}</option>` + o
  }
  let planTimer, planSeq = 0
  function drawNew(p) {
    const plan = p.plan || { slots: [] }
    const free = plan.slots.filter((s) => !s.taken).length
    const valid = !plan.error && free > 0 && p.title.trim() !== ''
    frame({
      eyebrow: p.replaces ? 'Change time' : 'New request',
      title: p.replaces ? esc(p.title) : 'Book office time',
      body:
        (p.replaces ? '' : field('f-title', 'Purpose', `<input class="input" id="f-title" maxlength="80" autocomplete="off" placeholder="WIE committee meeting" value="${esc(p.title)}">`)) +
        field('f-date', 'Day', `<select class="select" id="f-date">${dateOptions(p.date)}</select>`) +
        `<div class="grid grid-cols-2 gap-4">${field('f-start', 'Start', `<select class="select" id="f-start">${timeOptions(p.start, 0, DAY - SNAP)}</select>`)}${field('f-end', 'End', `<select class="select" id="f-end">${timeOptions(p.end, p.start + SNAP, p.start + DAY - SNAP, p.start)}</select>`)}</div>` +
        (p.replaces ? '' : `<div class="space-y-2"><span class="label">Repeat</span><div class="btn-group flex" id="f-repeat">${[['', 'None'], ['weekly', 'Weekly'], ['biweekly', 'Every 2 weeks']].map(([v, l]) => `<button type="button" class="btn btn-outline btn-sm flex-1" data-v="${v}" aria-pressed="${p.repeat === v}">${l}</button>`).join('')}</div></div>`) +
        (p.repeat ? field('f-until', 'Until', `<input class="input" type="date" id="f-until" min="${p.date}" value="${p.until}">`) : '') +
        (plan.error ? `<p class="text-sm">${esc(plan.error)}</p>` : !p.repeat && plan.slots[0]?.taken ? '<p class="text-sm">That time is already taken.</p>' : '') +
        (p.repeat && plan.slots.length ? `<div>${sectionHead('Dates', `${free} of ${plan.slots.length} free`)}<ul>${plan.slots.map((s) => row(`<span class="text-sm ${s.taken ? 'text-muted-foreground line-through' : ''}">${fmtDay(s.date)}</span>`, `<span class="meta">${s.taken ? 'Taken' : 'Free'}</span>`)).join('')}</ul></div>` : '') +
        (p.original && p.original.status === 'approved' ? '<p class="text-sm text-muted-foreground">Your current time stays booked until the change is approved.</p>' : ''),
      foot: `<button type="button" class="btn btn-outline" data-close>Cancel</button><button type="button" class="btn btn-default" id="f-submit" ${valid ? '' : 'disabled'}>${p.replaces ? (p.original?.status === 'approved' ? 'Request change' : 'Move') : p.repeat ? `Request ${free} date${free === 1 ? '' : 's'}` : 'Request'}</button>`,
    })
    const update = (k, v) => {
      p[k] = v
      if (k === 'start' && p.end <= p.start) p.end = p.start + 60
      if (k === 'start' && p.end - p.start >= DAY) p.end = p.start + 60
      if (k === 'date' && p.until < p.date) p.until = addDays(p.date, 56)
      syncDraft(p)
      if (k === 'date' && state.view !== 'month' && !visible(p.date)) { state.anchor = p.date; load() }
      drawPanel()
      render()
      schedulePlan(p)
    }
    $('#f-title', panel)?.addEventListener('input', (e) => { p.title = e.target.value; $('#f-submit', panel).disabled = !(!plan.error && free > 0 && p.title.trim()) })
    $('#f-date', panel).addEventListener('change', (e) => update('date', e.target.value))
    $('#f-start', panel).addEventListener('change', (e) => update('start', +e.target.value))
    $('#f-end', panel).addEventListener('change', (e) => update('end', +e.target.value))
    $$('#f-repeat button', panel).forEach((b) => b.addEventListener('click', () => update('repeat', b.dataset.v)))
    $('#f-until', panel)?.addEventListener('change', (e) => update('until', e.target.value))
    $('#f-submit', panel).addEventListener('click', (e) => busy(e.currentTarget, async () => {
      const res = await api('/api/web/bookings', { method: 'POST', body: { request_id: p.requestId, title: p.title, date: p.date, start: p.start, end: p.end, repeat: p.repeat, until: p.until, replaces: p.replaces || 0 } })
      state.draft = null
      toast(p.replaces ? (p.original?.status === 'approved' ? 'Change requested' : 'Moved') : res.skipped ? `Requested · ${res.skipped} taken date${res.skipped === 1 ? '' : 's'} left out` : 'Requested')
      await load()
      openDetails(res.bookings[0].id)
    }))
    if (!p.title && !p.replaces) setTimeout(() => $('#f-title', panel)?.focus(), 60)
  }
  function visible(date) { const [a, z] = range(); return date >= a && date <= z }
  function syncDraft(p) {
    const clash = state.bookings.some((b) => active(b) && b.id !== p.replaces && b.date === p.date && b.start < p.end && b.end > p.start)
    state.draft = { date: p.date, start: p.start, end: p.end, bad: clash || isPast(p.date, p.start) }
  }
  function schedulePlan(p) {
    clearTimeout(planTimer)
    const seq = ++planSeq
    planTimer = setTimeout(async () => {
      try {
        const res = await api('/api/web/plan', { method: 'POST', body: { title: p.title || 'x', date: p.date, start: p.start, end: p.end, repeat: p.repeat, until: p.until, replaces: p.replaces || 0 } })
        if (seq !== planSeq || state.panel !== p) return
        p.plan = res
        drawPanel()
      } catch (e) { /* the submit will report the problem */ }
    }, 150)
  }
  function startNew(date, start, end) {
    if (!USER.book) return
    const base = date || (state.view === 'day' ? state.anchor : state.today)
    let s = start ?? (base === state.today ? Math.ceil((state.now + 1) / 30) * 30 : 600)
    if (s >= DAY - SNAP) s = 600
    const d = base === state.today && s <= state.now ? addDays(state.today, 1) : base
    const p = { type: 'new', title: '', date: d, start: s, end: end ?? s + 60, repeat: '', until: addDays(d, 56), requestId: (crypto.randomUUID && crypto.randomUUID()) || String(Date.now()) + Math.random() }
    if (state.view !== 'month' && !visible(d)) { state.anchor = d; load() }
    syncDraft(p)
    openPanel(p)
    schedulePlan(p)
  }
  function startChange(b, date, start, end) {
    const p = { type: 'new', replaces: b.id, original: b, title: b.title, date, start, end, repeat: '', until: date, requestId: (crypto.randomUUID && crypto.randomUUID()) || String(Date.now()) }
    syncDraft(p)
    openPanel(p)
    schedulePlan(p)
  }

  // Details and review
  async function openDetails(id) {
    const p = { type: 'details', id, lead: null, data: null }
    openPanel(p)
    try {
      p.data = await api(`/api/web/bookings/${id}`)
      p.lead = p.data.booking.lead
      if (state.panel === p) { drawPanel(); render() }
    } catch (e) { toast(e.message); closePanel() }
  }
  function drawDetails(p) {
    if (!p.data) return loadingFrame('Booking')
    const { booking: b, series, original, change, history } = p.data
    const waiting = series.filter((r) => r.status === 'pending' && !isPast(r.date, r.start))
    const repeats = series.length > 1
    let body = `<dl>${kv('Status', `<span class="kicker ${b.status === 'pending' ? 'text-primary' : 'text-muted-foreground'}">${statusText(b)}</span>`)}` +
      kv('When', `${original ? `<span class="text-muted-foreground line-through">${esc(when(original))}</span><br>` : ''}${esc(when(b))}`) +
      kv('Booked by', b.mine ? 'You' : esc(b.name)) +
      (repeats ? kv('Repeats', `${(parse(series[1].date) - parse(series[0].date)) / 864e5 > 8 ? 'Every 2 weeks' : 'Weekly'}, ${series.length} dates`) : '') +
      (b.reason ? kv('Reason', esc(b.reason)) : '') +
      (change ? kv('Change', `${esc(when(change))} · waiting for approval`) : '') + '</dl>'
    let foot = ''
    if (USER.approve && waiting.length) {
      if (b.mine) body += '<p class="text-sm text-muted-foreground">Another officer has to decide your own request.</p>'
      else {
        body += `<div>${sectionHead('Review', `${waiting.length} waiting`)}` +
          (waiting.length > 1 ? `<ul data-dates>${waiting.map((r) => row(`<label class="flex flex-1 cursor-pointer items-center gap-3 text-sm"><input type="checkbox" class="checkbox" value="${r.id}" checked>${fmtDay(r.date)}</label>`, `<span class="meta">${span(r)}</span>`)).join('')}</ul>` : '') +
          `<div class="mt-4 space-y-2"><label class="label" for="r-reason">Reason <span class="font-normal text-muted-foreground">(optional)</span></label><textarea class="textarea" id="r-reason" maxlength="300"></textarea></div></div>`
        foot = `<button type="button" class="btn btn-outline" data-decline>Decline</button><button type="button" class="btn btn-default" data-approve>${waiting.length > 1 ? `Approve ${waiting.length}` : original ? 'Approve change' : 'Approve'}</button>`
      }
    }
    if (b.mine && active(b) && !isPast(b.date, b.start)) {
      const later = series.filter((r) => r.date >= b.date && active(r)).length
      foot = `<button type="button" class="btn btn-outline" data-cancel>${b.replaces ? 'Withdraw change' : 'Cancel booking'}</button>` +
        (repeats && later > 1 ? `<button type="button" class="btn btn-outline" data-cancel-rest>Cancel ${later} dates</button>` : '')
      if (!b.replaces && !change) body += `<button type="button" class="btn btn-link self-start" data-change>Change time</button>`
    }
    if (USER.approve && !b.mine && b.status === 'approved' && !isPast(b.date, b.start)) {
      body += `<details><summary class="btn btn-link cursor-pointer list-none">Revoke</summary><div class="mt-4 space-y-2"><label class="label" for="v-reason">Reason <span class="font-normal text-muted-foreground">(optional)</span></label><textarea class="textarea" id="v-reason" maxlength="300"></textarea>
        <div class="flex gap-2 pt-2 [&>*]:flex-1"><button type="button" class="btn btn-outline" data-revoke>Revoke ${esc(fmtDay(b.date))}</button>${repeats ? '<button type="button" class="btn btn-outline" data-revoke-rest>Revoke from here</button>' : ''}</div></div></details>`
    }
    body += `<div>${sectionHead('History')}<ol class="space-y-3">${history.map((h) => `<li class="text-sm">${esc(h.text)}<span class="meta block">${esc(h.at)}</span></li>`).join('')}</ol></div>`
    body += `<a class="btn btn-link inline-flex items-center gap-2 self-start" href="${googleEventURL(b)}" target="_blank" rel="noopener">Add to Google Calendar <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M7 17 17 7M7 7h10v10"/></svg></a>`
    frame({ eyebrow: repeats ? `${series.findIndex((r) => r.id === b.id) + 1} of ${series.length}` : original ? 'Time change' : 'Booking', title: esc(b.title), body, foot })

    const act = (btn, path, payload, done) => busy(btn, async () => {
      p.data = await api(`/api/web/bookings/${b.id}/${path}`, { method: 'POST', body: payload })
      toast(done)
      drawPanel()
      load()
    })
    $('[data-approve]', panel)?.addEventListener('click', (e) => {
      const ids = waiting.length > 1 ? $$('[data-dates] input:checked', panel).map((i) => +i.value) : waiting.map((r) => r.id)
      act(e.currentTarget, 'decide', { approve: ids, reason: $('#r-reason', panel).value }, waiting.length > 1 ? `Approved ${ids.length} of ${waiting.length}` : 'Approved')
    })
    $('[data-decline]', panel)?.addEventListener('click', (e) => act(e.currentTarget, 'decide', { approve: [], reason: $('#r-reason', panel).value }, 'Declined'))
    $('[data-cancel]', panel)?.addEventListener('click', (e) => { if (armed(e.currentTarget, 'Confirm')) act(e.currentTarget, 'cancel', { rest: false }, b.replaces ? 'Withdrawn' : 'Cancelled') })
    $('[data-cancel-rest]', panel)?.addEventListener('click', (e) => { if (armed(e.currentTarget, 'Confirm')) act(e.currentTarget, 'cancel', { rest: true }, 'Cancelled') })
    $('[data-revoke]', panel)?.addEventListener('click', (e) => act(e.currentTarget, 'revoke', { rest: false, reason: $('#v-reason', panel).value }, 'Revoked'))
    $('[data-revoke-rest]', panel)?.addEventListener('click', (e) => act(e.currentTarget, 'revoke', { rest: true, reason: $('#v-reason', panel).value }, 'Revoked'))
    $('[data-change]', panel)?.addEventListener('click', () => startChange(b, b.date, b.start, b.end))
  }
  function googleEventURL(b) {
    const stamp = (date, m) => { const d = parse(addDays(date, Math.floor(m / DAY))); m %= DAY; return `${ymd(d).replace(/-/g, '')}T${pad(Math.floor(m / 60))}${pad(m % 60)}00` }
    return `https://calendar.google.com/calendar/render?action=TEMPLATE&text=${encodeURIComponent(b.title)}&location=${encodeURIComponent('IEEE uOttawa office')}&dates=${stamp(b.date, b.start)}/${stamp(b.date, b.end)}&ctz=America/Toronto`
  }

  // Lists
  const listRow = (b, extra = '') => `<li class="border-b border-border first:border-t"><button type="button" data-open="${b.id}" class="group flex w-full items-center justify-between gap-4 py-4 text-left transition-colors hover:bg-muted/50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-4">
      <span class="flex min-w-0 flex-col gap-1"><span class="truncate font-display text-xl font-medium leading-tight tracking-tight">${esc(b.title)}</span>
      <span class="meta">${esc(when(b))}${b.mine ? '' : ' · ' + esc(b.name)}${extra}</span></span>
      <span class="flex shrink-0 items-center gap-3"><span class="kicker text-[0.65rem] ${b.status === 'pending' ? 'text-primary' : 'text-muted-foreground'}">${statusText(b)}</span>
      <svg class="h-5 w-5 shrink-0 text-primary transition-transform group-hover:translate-x-1" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M5 12h14M12 5l7 7-7 7"/></svg></span></button></li>`
  const bindOpen = () => $$('[data-open]', panel).forEach((x) => x.addEventListener('click', () => {
    const id = +x.dataset.open
    const b = (state.panel.rows || []).find((r) => r.id === id)
    if (b && state.view !== 'month' && !visible(b.date)) { state.anchor = b.date; load() }
    openDetails(id)
  }))
  async function drawMine(p) {
    if (!p.rows) {
      loadingFrame(esc(USER.name || ''))
      try { p.rows = (await api('/api/web/mine')).bookings } catch (e) { toast(e.message); return closePanel() }
      if (state.panel !== p) return
    }
    const rows = p.rows.slice().sort((a, b) => a.date.localeCompare(b.date) || a.start - b.start)
    const up = rows.filter((b) => active(b) && !isPast(b.date, b.end))
    const past = rows.filter((b) => !up.includes(b)).reverse()
    frame({
      eyebrow: esc(USER.name || ''), title: 'My bookings',
      body: `<div>${sectionHead('Upcoming', up.length)}${up.length ? `<ul>${up.map((b) => listRow(b)).join('')}</ul>` : '<p class="text-muted-foreground">Nothing booked.</p>'}</div>` +
        `<div>${sectionHead('Past week', past.length)}${past.length ? `<ul>${past.map((b) => listRow(b)).join('')}</ul>` : '<p class="text-muted-foreground">Nothing yet.</p>'}</div>`,
      foot: USER.book ? '<button type="button" class="btn btn-default" data-mine-book>Book office time</button>' : '',
    })
    bindOpen()
    $('[data-mine-book]', panel)?.addEventListener('click', () => startNew())
  }
  async function drawRequests(p) {
    if (!p.rows) {
      loadingFrame('Signing Officers')
      try { p.rows = (await api('/api/web/requests')).bookings } catch (e) { toast(e.message); return closePanel() }
      if (state.panel !== p) return
    }
    const groups = new Map()
    for (const b of p.rows) { if (!groups.has(b.lead)) groups.set(b.lead, []); groups.get(b.lead).push(b) }
    frame({
      eyebrow: 'Signing Officers', title: 'Requests',
      body: groups.size ? `<ul>${[...groups.values()].map((g) => listRow(g[0], g.length > 1 ? ` · ${g.length} dates` : g[0].replaces ? ' · time change' : '')).join('')}</ul>` : '<p class="text-muted-foreground">Nothing waiting.</p>',
    })
    bindOpen()
  }
  function drawGoogle() {
    const ics = CFG.ics || ''
    const google = 'https://calendar.google.com/calendar/render?cid=' + encodeURIComponent(ics.replace(/^https:/, 'webcal:'))
    frame({
      eyebrow: 'Google Calendar', title: 'Subscribe',
      body: '<p class="text-muted-foreground">Approved bookings appear in the shared IEEE uOttawa Office calendar. Google refreshes subscribed calendars every few hours.</p>' +
        field('g-ics', 'Calendar link', `<input class="input font-mono" id="g-ics" readonly value="${esc(ics)}">`),
      foot: `<button type="button" class="btn btn-outline" data-copy>Copy link</button><a class="btn btn-default" href="${esc(google)}" target="_blank" rel="noopener">Add to Google</a>`,
    })
    $('[data-copy]', panel).addEventListener('click', () => {
      const input = $('#g-ics', panel)
      const fallback = () => { input.select(); toast('Press Ctrl+C to copy') }
      try { navigator.clipboard.writeText(input.value).then(() => toast('Copied'), fallback) } catch (e) { fallback() }
    })
  }

  // ---------- pointer: drag to book; drag your booking to move or resize ----------
  const hourPx = () => (parseFloat(getComputedStyle($('[data-scroller]') || document.body).getPropertyValue('--hour')) || 3.5) * (parseFloat(getComputedStyle(document.documentElement).fontSize) || 16)
  const minuteAt = (col, y) => Math.max(0, Math.min(DAY, Math.round(((y - col.getBoundingClientRect().top) / hourPx()) * 60 / SNAP) * SNAP))
  const colAt = (x, y) => document.elementsFromPoint(x, y).find((e) => e.dataset && e.dataset.col)
  let drag = null
  document.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return
    const ev = e.target.closest('[data-id]')
    const col = e.target.closest('[data-col]')
    if (ev && col) {
      const b = state.bookings.find((x) => x.id === +ev.dataset.id)
      if (!b) return
      drag = { kind: e.target.closest('[data-resize]') ? 'resize' : 'move', b, x0: e.clientX, y0: e.clientY, grab: minuteAt(col, e.clientY) - b.start }
      return
    }
    if (col && USER.book && col.dataset.col >= state.today) {
      drag = { kind: 'create', date: col.dataset.col, anchor: Math.min(Math.floor(minuteAt(col, e.clientY) / SNAP) * SNAP, DAY - SNAP), x0: e.clientX, y0: e.clientY, touch: e.pointerType === 'touch' }
    }
  })
  document.addEventListener('pointermove', (e) => {
    if (!drag) return
    if (!drag.moved && Math.hypot(e.clientX - drag.x0, e.clientY - drag.y0) < 6) return
    if (drag.kind === 'create' && drag.touch) { drag = null; return } // let touch scroll; a tap books instead
    drag.moved = true
    const col = colAt(e.clientX, e.clientY)
    if (drag.kind === 'create') {
      if (!col || col.dataset.col !== drag.date) return
      const m = minuteAt(col, e.clientY)
      const p = { date: drag.date, start: Math.min(drag.anchor, m), end: Math.max(drag.anchor + SNAP, m) }
      syncDraft(p)
      render()
      return
    }
    if (!canMove(drag.b)) return
    const b = drag.b, len = b.end - b.start
    if (drag.kind === 'move' && col && col.dataset.col >= state.today) {
      const s = Math.max(0, Math.min(DAY - SNAP, Math.round((minuteAt(col, e.clientY) - drag.grab) / SNAP) * SNAP))
      syncDraft({ date: col.dataset.col, start: s, end: s + len, replaces: b.id })
    } else if (drag.kind === 'resize') {
      const c = $(`[data-col="${b.date}"]`)
      syncDraft({ date: b.date, start: b.start, end: Math.max(b.start + SNAP, minuteAt(c, e.clientY)), replaces: b.id })
    }
    render()
  })
  document.addEventListener('pointerup', () => {
    const d = drag
    drag = null
    if (!d) return
    if (d.kind === 'create') {
      if (d.moved && state.draft) startNew(state.draft.date, state.draft.start, state.draft.end)
      else startNew(d.date, d.anchor, d.anchor + 60)
      return
    }
    if (!d.moved || !canMove(d.b) || !state.draft) { state.draft = null; openDetails(d.b.id); return }
    const dr = state.draft
    if (dr.date === d.b.date && dr.start === d.b.start && dr.end === d.b.end) { state.draft = null; render(); return }
    startChange(d.b, dr.date, dr.start, dr.end)
  })
  // Keyboard: Enter or Space opens a booking.
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && state.panel) closePanel()
    const ev = e.target.closest && e.target.closest('[data-id]')
    if (ev && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openDetails(+ev.dataset.id) }
  })

  // ---------- controls ----------
  const shift = (dir) => {
    const d = parse(state.anchor)
    state.anchor = state.view === 'month' ? ymd(new Date(d.getFullYear(), d.getMonth() + dir, 1)) : addDays(state.anchor, dir * (state.view === 'week' ? 7 : 1))
    load()
  }
  $('[data-prev]').addEventListener('click', () => shift(-1))
  $('[data-next]').addEventListener('click', () => shift(1))
  $('[data-today]').addEventListener('click', () => { state.anchor = state.today; load() })
  $$('[data-view]').forEach((b) => b.addEventListener('click', () => { state.view = b.dataset.view; load() }))
  $('[data-book]')?.addEventListener('click', () => startNew())
  $('[data-subscribe]').addEventListener('click', () => openPanel({ type: 'google' }))
  scrim.addEventListener('click', closePanel)
  panel.addEventListener('click', (e) => { if (e.target.closest('[data-close]')) closePanel() })
  $('[data-calendar]').addEventListener('click', (e) => {
    const c = e.target.closest('[data-day]')
    if (c) { state.anchor = c.dataset.day; state.view = narrow() ? 'day' : 'week'; load() }
  })
  $$('[data-open-mine]').forEach((a) => a.addEventListener('click', (e) => { e.preventDefault(); a.closest('details')?.removeAttribute('open'); openPanel({ type: 'mine' }) }))
  $$('[data-open-requests]').forEach((a) => a.addEventListener('click', (e) => { e.preventDefault(); a.closest('details')?.removeAttribute('open'); openPanel({ type: 'requests' }) }))
  let touchX = null
  $('[data-calendar]').addEventListener('touchstart', (e) => { touchX = e.touches[0].clientX }, { passive: true })
  $('[data-calendar]').addEventListener('touchend', (e) => {
    if (touchX === null || state.view !== 'day') return
    const dx = e.changedTouches[0].clientX - touchX
    if (Math.abs(dx) > 70) shift(dx < 0 ? 1 : -1)
    touchX = null
  })
  let lastNarrow = narrow()
  addEventListener('resize', () => { if (narrow() !== lastNarrow) { lastNarrow = narrow(); render() } })
  // Keep the calendar current with changes made in Discord or by others.
  setInterval(() => { if (!document.hidden && !drag) load() }, 60000)
  document.addEventListener('visibilitychange', () => { if (!document.hidden) load() })

  load().then(() => {
    if (CFG.view === 'mine') openPanel({ type: 'mine' })
    if (CFG.view === 'requests' && USER.approve) openPanel({ type: 'requests' })
    if (CFG.view) history.replaceState(null, '', '/')
  })
})()
