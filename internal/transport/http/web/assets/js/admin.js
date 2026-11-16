// Admin pages: presence, members, sessions, scans and statistics.
// Every request goes through the management API with the admin's session cookie.
;(function () {
  'use strict'
  const { api, toast, escapeHTML: esc } = window.Office
  const $ = (sel, root = document) => root.querySelector(sel)
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel))

  // ---- time (the office runs on Ottawa time) ----

  const TZ = 'America/Toronto'
  const partsFmt = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' })
  const dayFmt = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, month: 'short', day: 'numeric' })
  const secondsFmt = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' })
  const timeFmt = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
  const dateFmt = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, year: 'numeric', month: 'short', day: 'numeric' })

  function toDate(v) {
    if (!v) return null
    const d = new Date(v)
    return isNaN(d.getTime()) ? null : d
  }
  function parts(d) {
    const o = {}
    for (const p of partsFmt.formatToParts(d)) o[p.type] = p.value
    return o
  }
  // "Nov 14, 16:29", with the year only when it isn't this year.
  function fmtDateTime(v, seconds) {
    const d = toDate(v)
    if (!d) return '—'
    const day = parts(d).year === parts(new Date()).year ? dayFmt.format(d) : dateFmt.format(d)
    return `${day}, ${(seconds ? secondsFmt : timeFmt).format(d)}`
  }
  const fmtTime = (v) => { const d = toDate(v); return d ? timeFmt.format(d) : '—' }
  const fmtDate = (v) => { const d = toDate(v); return d ? dateFmt.format(d) : '—' }
  // YYYY-MM-DD of an instant in Ottawa.
  function ymd(v) {
    const p = parts(toDate(v))
    return `${p.year}-${p.month}-${p.day}`
  }
  // Value for <input type="datetime-local"> in Ottawa time.
  function toLocalInput(v) {
    const d = toDate(v)
    if (!d) return ''
    const p = parts(d)
    return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`
  }
  // Ottawa wall time from a datetime-local value to an instant.
  function fromLocalInput(s) {
    const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(s || '')
    if (!m) return null
    const wall = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5])
    const offset = (t) => { const p = parts(new Date(t)); return Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour, +p.minute, +p.second) - t }
    let t = wall - offset(wall)
    t = wall - offset(t)
    return new Date(t)
  }
  function duration(ms) {
    const mins = Math.max(0, Math.floor(ms / 60000))
    const h = Math.floor(mins / 60)
    return h ? `${h}h ${mins % 60}m` : `${mins}m`
  }
  function uptime(seconds) {
    const s = Math.max(0, Number(seconds) || 0)
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60)
    if (d) return `${d}d ${h}h`
    if (h) return `${h}h ${m}m`
    return `${m}m`
  }
  function ago(seconds) {
    const s = Math.max(0, Number(seconds) || 0)
    if (s < 60) return `${s} s ago`
    if (s < 3600) return `${Math.floor(s / 60)} min ago`
    if (s < 86400) return `${Math.floor(s / 3600)} h ago`
    return `${Math.floor(s / 86400)} d ago`
  }

  const num = (v) => Number(v) || 0
  const fixed = (v, n) => num(v).toFixed(n)
  const count = (v) => num(v).toLocaleString('en-CA')
  const plural = (n, one, many) => `${count(n)} ${n === 1 ? one : many || one + 's'}`
  const list = (v) => (Array.isArray(v) ? v : [])

  function methodLabel(method, active) {
    if (active) return '—'
    const m = String(method || '').toLowerCase()
    return { rfid: 'RFID', discord: 'Discord', api: 'API', auto: 'Auto' }[m] || (m ? m.toUpperCase() : 'Unknown')
  }

  function qs(params) {
    const q = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== '' && v != null) q.append(k, v)
    const s = q.toString()
    return s ? '?' + s : ''
  }

  // Words for state: primary when on, muted when off.
  const state = (label, on) => `<span class="kicker ${on ? 'text-primary' : 'text-muted-foreground'}">${esc(label)}</span>`
  const emptyRow = (cols, text) => `<tr class="hover:bg-transparent"><td colspan="${cols}" class="py-6 text-muted-foreground">${esc(text)}</td></tr>`

  // ---- busy state and two-step confirm ----

  // Disable a panel's buttons while a change is saving.
  async function busy(root, fn) {
    const buttons = $$('button', root).filter((b) => !b.disabled)
    buttons.forEach((b) => { b.disabled = true })
    try {
      return await fn()
    } finally {
      buttons.forEach((b) => { b.disabled = false })
    }
  }

  // Buttons marked data-confirm need a second click: the label becomes "Confirm" for a few seconds.
  function disarm(btn) {
    if (btn.dataset.armed !== '1') return
    btn.dataset.armed = ''
    btn.textContent = btn.dataset.label
    btn.classList.remove('border-foreground', 'font-semibold')
    clearTimeout(btn._disarm)
  }
  function confirmed(btn) {
    if (!btn.hasAttribute('data-confirm')) return true
    if (btn.dataset.armed === '1') { disarm(btn); return true }
    $$('[data-confirm][data-armed="1"]').forEach(disarm)
    btn.dataset.label = btn.textContent
    btn.dataset.armed = '1'
    btn.textContent = btn.dataset.confirm || 'Confirm'
    btn.classList.add('border-foreground', 'font-semibold')
    btn._disarm = setTimeout(() => disarm(btn), 4000)
    return false
  }
  document.addEventListener('focusout', (e) => {
    const b = e.target.closest && e.target.closest('[data-confirm]')
    if (b && !b.contains(e.relatedTarget)) setTimeout(() => disarm(b), 150)
  })

  // ---- drawers ----

  let lastFocus = null
  function openDrawer(name) {
    const d = $(`[data-drawer="${name}"]`)
    lastFocus = document.activeElement
    d.hidden = false
    document.body.style.overflow = 'hidden'
    $('[data-form-error]', d).textContent = ''
    const first = $('input:not([type=hidden]):not([readonly])', d)
    if (first) setTimeout(() => first.focus(), 0)
  }
  function closeDrawer() {
    $$('[data-drawer]').forEach((d) => { d.hidden = true; $$('[data-confirm]', d).forEach(disarm) })
    document.body.style.overflow = ''
    if (lastFocus && lastFocus.focus) lastFocus.focus()
  }
  const openDrawerEl = () => $$('[data-drawer]').find((d) => !d.hidden)
  document.addEventListener('keydown', (e) => {
    const d = openDrawerEl()
    if (!d) return
    if (e.key === 'Escape') { e.preventDefault(); closeDrawer() }
    if (e.key === 'Tab') {
      const f = $$('input:not([type=hidden]),select,button,a[href]', d).filter((el) => !el.disabled && el.offsetParent !== null)
      if (!f.length) return
      if (e.shiftKey && document.activeElement === f[0]) { e.preventDefault(); f[f.length - 1].focus() }
      else if (!e.shiftKey && document.activeElement === f[f.length - 1]) { e.preventDefault(); f[0].focus() }
    }
  })
  $$('[data-drawer-close]').forEach((el) => el.addEventListener('click', closeDrawer))

  // ---- tabs ----

  const TABS = ['dashboard', 'users', 'sessions', 'scans', 'reports']
  const loaders = {}
  let current = null

  function showTab(name) {
    if (!TABS.includes(name)) name = 'dashboard'
    current = name
    $$('[data-tab]').forEach((t) => {
      const on = t.dataset.tab === name
      t.setAttribute('aria-selected', on ? 'true' : 'false')
      t.tabIndex = on ? 0 : -1
    })
    $$('[data-panel]').forEach((p) => { p.hidden = p.dataset.panel !== name })
    if (name !== 'dashboard') stopAutoRefresh()
    loaders[name]()
  }
  addEventListener('hashchange', () => showTab(location.hash.slice(1)))
  $('[role=tablist]').addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return
    const i = TABS.indexOf(current) + (e.key === 'ArrowRight' ? 1 : -1)
    const next = TABS[(i + TABS.length) % TABS.length]
    location.hash = next
    $(`[data-tab="${next}"]`).focus()
  })

  // Reload what the other tabs show after a change.
  function refreshAfterChange() {
    loaders[current]()
  }

  // ---- dashboard ----

  const dash = $('[data-panel="dashboard"]')
  let autoTimer = null

  function setStat(name, value) { $(`[data-stat="${name}"]`).textContent = value }

  async function loadDashboard() {
    const [users, total, scans, open, env, devices] = await Promise.allSettled([
      api('/api/users?limit=0'),
      api('/api/sessions/count'),
      api('/api/rfid/scans'),
      api('/api/sessions/open'),
      api('/api/environment'),
      api('/api/devices/health'),
    ])
    const failed = [users, total, scans, open, env, devices].find((r) => r.status === 'rejected')
    if (failed) toast(failed.reason.message)

    if (users.status === 'fulfilled') setStat('users', count(list(users.value).length))
    if (total.status === 'fulfilled') setStat('sessions', count((total.value || {}).total))
    if (scans.status === 'fulfilled') setStat('scans', count(list(scans.value).length))

    if (env.status === 'fulfilled') {
      const e = env.value || {}
      const label = $('[data-stat="temperature-label"]')
      if (e.available && e.fresh) {
        setStat('temperature', `${fixed(e.temperature_c, 1)} °C`)
        label.textContent = 'Temperature'
      } else if (e.available) {
        setStat('temperature', '—')
        label.textContent = 'Temperature · stale'
        label.title = `Last reading ${ago(e.age_seconds)}`
      } else {
        setStat('temperature', '—')
        label.textContent = 'Temperature'
      }
    }

    if (open.status === 'fulfilled') renderPresence(list(open.value))
    else $('[data-presence]').innerHTML = `<li class="border-b border-border py-6 text-muted-foreground">${esc(open.reason.message)}</li>`

    if (devices.status === 'fulfilled') renderDevices(list(devices.value))
    else $('[data-devices]').innerHTML = emptyRow(10, devices.reason.message)

    $('[data-dash-updated]').textContent = `Updated ${timeFmt.format(new Date())}`
  }

  function renderPresence(sessions) {
    setStat('active', count(sessions.length))
    $('[data-action="checkout-all"]').hidden = sessions.length === 0
    const ul = $('[data-presence]')
    if (!sessions.length) {
      ul.innerHTML = '<li class="py-2 text-muted-foreground">Nobody is checked in.</li>'
      return
    }
    const now = Date.now()
    ul.innerHTML = sessions.map((s) => {
      const name = s.user_name || s.UserName || 'Unknown'
      const since = toDate(s.check_in)
      return `<li class="flex items-center justify-between gap-4 border-b border-border py-6">
        <div class="flex min-w-0 flex-col gap-1.5">
          <span class="truncate font-display text-2xl font-medium leading-tight tracking-tight">${esc(name)}</span>
          <span class="meta tabular-nums">In since ${esc(fmtTime(since))} · ${esc(since ? duration(now - since.getTime()) : '—')}</span>
        </div>
        <button type="button" class="btn btn-outline btn-sm shrink-0" data-checkout="${num(s.user_id || s.UserID)}">Check out</button>
      </li>`
    }).join('')
  }

  function renderDevices(devices) {
    $('[data-devices-meta]').textContent = devices.length ? `${devices.filter((d) => d.fresh).length} of ${plural(devices.length, 'device')} online` : ''
    const tbody = $('[data-devices]')
    if (!devices.length) { tbody.innerHTML = emptyRow(10, 'No device has reported yet.'); return }
    tbody.innerHTML = devices.map((d) => {
      const link = d.transport === 'usb' ? 'USB' : d.wifi_connected ? 'Wi-Fi' : 'Disconnected'
      return `<tr>
        <td class="font-mono text-xs">${esc(d.device_id || 'default')}</td>
        <td>${state(d.fresh ? 'Online' : 'Offline', d.fresh)}</td>
        <td class="tabular-nums text-muted-foreground">${esc(ago(d.age_seconds))}</td>
        <td class="tabular-nums">${esc(uptime(d.uptime_seconds))}</td>
        <td class="tabular-nums">${esc(count(d.free_heap_bytes))} B</td>
        <td>${esc(link)}</td>
        <td class="tabular-nums">${d.transport === 'usb' ? '—' : esc(d.rssi ?? '—')}</td>
        <td class="font-mono text-xs">${esc(d.ip || '—')}</td>
        <td class="font-mono text-xs">${esc(d.firmware_version || '—')}</td>
        <td class="text-muted-foreground">${esc(d.reset_reason || '—')}</td>
      </tr>`
    }).join('')
  }

  function stopAutoRefresh() {
    clearInterval(autoTimer)
    autoTimer = null
    $('[data-autorefresh]').checked = false
  }
  $('[data-autorefresh]').addEventListener('change', (e) => {
    clearInterval(autoTimer)
    autoTimer = null
    if (e.target.checked) {
      loadDashboard()
      autoTimer = setInterval(loadDashboard, 30000)
    }
  })

  async function checkOut(userID, root) {
    await busy(root, async () => {
      try {
        await api('/api/sessions/checkout', { method: 'POST', body: { user_id: userID } })
        toast('Checked out')
      } catch (err) { toast(err.message) }
    })
    refreshAfterChange()
  }
  async function checkIn(userID, root) {
    await busy(root, async () => {
      try {
        await api('/api/sessions/checkin', { method: 'POST', body: { user_id: userID } })
        toast('Checked in')
      } catch (err) { toast(err.message) }
    })
    refreshAfterChange()
  }

  dash.addEventListener('click', async (e) => {
    const btn = e.target.closest('button')
    if (!btn) return
    if (btn.dataset.action === 'dashboard-refresh') loadDashboard()
    else if (btn.dataset.checkout) checkOut(Number(btn.dataset.checkout), dash)
    else if (btn.dataset.action === 'checkout-all' && confirmed(btn)) {
      await busy(dash, async () => {
        try {
          const r = await api('/api/sessions/checkout-all', { method: 'POST' })
          toast(r && typeof r === 'object' && 'count' in r ? `Checked out ${plural(r.count, 'person', 'people')}` : 'Checked out everyone')
        } catch (err) { toast(err.message) }
      })
      loadDashboard()
    }
  })
  loaders.dashboard = loadDashboard

  // ---- pager ----

  function pager(name, { offset, limit, shown, total }) {
    const nav = $(`[data-pager="${name}"]`)
    const from = shown ? offset + 1 : 0
    const to = offset + shown
    nav.hidden = offset === 0 && shown < limit
    $('[data-pager-label]', nav).textContent = shown ? `${count(from)}–${count(to)}${total != null ? ` of ${count(total)}` : ''}` : ''
    $('[data-pager-prev]', nav).disabled = offset === 0
    $('[data-pager-next]', nav).disabled = total != null ? to >= total : shown < limit
  }
  function clampLimit(input, fallback) {
    const n = Math.floor(Number(input.value))
    return n >= 1 ? Math.min(n, 500) : fallback
  }
  function debounce(fn, ms) {
    let t
    return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms) }
  }

  // ---- users ----

  const usersPanel = $('[data-panel="users"]')
  const usersForm = $('[data-users-filters]')
  let usersOffset = 0
  let usersRows = []

  function userParams() {
    const f = usersForm.elements
    return { search: f.search.value.trim(), sort_by: f.sort_by.value, order: f.order.value, limit: clampLimit(f.limit, 50) }
  }

  async function loadUsers() {
    const p = userParams()
    const usersExport = $('[data-users-export]')
    usersExport.href = '/api/users/export' + qs({ ...p, offset: usersOffset })
    usersExport.download = `users_${ymd(new Date())}.csv`
    const tbody = $('[data-users]')
    let users, open
    try {
      ;[users, open] = await Promise.all([api('/api/users' + qs({ ...p, offset: usersOffset })), api('/api/sessions/open').catch(() => null)])
    } catch (err) {
      tbody.innerHTML = emptyRow(6, err.message)
      return
    }
    usersRows = list(users)
    const inOffice = open ? new Set(list(open).map((s) => num(s.user_id || s.UserID))) : null
    $('[data-users-meta]').textContent = usersOffset === 0 && usersRows.length < p.limit ? plural(usersRows.length, p.search ? 'match' : 'member', p.search ? 'matches' : 'members') : ''
    pager('users', { offset: usersOffset, limit: p.limit, shown: usersRows.length })
    if (!usersRows.length) {
      tbody.innerHTML = emptyRow(6, usersOffset ? 'No more members.' : p.search ? 'No members match.' : 'No members yet.')
      return
    }
    tbody.innerHTML = usersRows.map((u) => {
      const isIn = inOffice ? inOffice.has(u.id) : null
      const status = isIn == null ? state('Unknown', false) : state(isIn ? 'In office' : 'Out', isIn)
      const presence = isIn == null
        ? `<button type="button" class="btn btn-outline btn-sm" data-checkin="${u.id}">Check in</button><button type="button" class="btn btn-outline btn-sm" data-checkout="${u.id}">Check out</button>`
        : isIn
          ? `<button type="button" class="btn btn-outline btn-sm" data-checkout="${u.id}">Check out</button>`
          : `<button type="button" class="btn btn-outline btn-sm" data-checkin="${u.id}">Check in</button>`
      return `<tr>
        <td class="font-mono text-xs tabular-nums text-muted-foreground">${u.id}</td>
        <td class="font-medium">${esc(u.name)}</td>
        <td class="font-mono text-xs">${esc(u.rfid_uid || '—')}</td>
        <td class="font-mono text-xs">${esc(u.discord_id || '—')}</td>
        <td>${status}</td>
        <td class="pr-0"><div class="flex justify-end gap-2">${presence}
          <button type="button" class="btn btn-ghost btn-sm" data-edit-user="${u.id}">Edit</button>
          <button type="button" class="btn btn-outline btn-sm" data-delete-user="${u.id}" data-confirm>Delete</button>
        </div></td>
      </tr>`
    }).join('')
  }

  const reloadUsers = () => { usersOffset = 0; loadUsers() }
  usersForm.addEventListener('input', debounce((e) => { if (e.target.name === 'search' || e.target.name === 'limit') reloadUsers() }, 300))
  usersForm.addEventListener('change', (e) => { if (e.target.tagName === 'SELECT') reloadUsers() })
  usersForm.addEventListener('submit', (e) => { e.preventDefault(); reloadUsers() })
  usersForm.addEventListener('reset', () => setTimeout(reloadUsers, 0))
  $('[data-pager="users"]').addEventListener('click', (e) => {
    const limit = userParams().limit
    if (e.target.closest('[data-pager-prev]')) usersOffset = Math.max(0, usersOffset - limit)
    else if (e.target.closest('[data-pager-next]')) usersOffset += limit
    else return
    loadUsers()
  })

  const userForm = $('[data-user-form]')
  function editUser(u) {
    const f = userForm.elements
    f.id.value = u ? u.id : ''
    f.name.value = u ? u.name : ''
    f.rfid_uid.value = u ? u.rfid_uid || '' : ''
    f.discord_id.value = u ? u.discord_id || '' : ''
    $('[data-user-form-title]').textContent = u ? 'Edit member' : 'Add member'
    openDrawer('user')
  }
  userForm.addEventListener('submit', async (e) => {
    e.preventDefault()
    const f = userForm.elements
    const body = { name: f.name.value.trim(), rfid_uid: f.rfid_uid.value.trim(), discord_id: f.discord_id.value.trim() }
    const error = $('[data-form-error]', userForm)
    if (!body.name || !body.rfid_uid || !body.discord_id) {
      error.textContent = 'Name, RFID UID and Discord ID are all required.'
      return
    }
    const id = f.id.value
    await busy(userForm, async () => {
      try {
        await api(id ? `/api/users/${id}` : '/api/users', { method: id ? 'PUT' : 'POST', body })
        toast(id ? 'Member saved' : 'Member added')
        closeDrawer()
        loadUsers()
      } catch (err) { error.textContent = err.message }
    })
  })

  usersPanel.addEventListener('click', async (e) => {
    const btn = e.target.closest('button')
    if (!btn) return
    const d = btn.dataset
    if (d.action === 'user-new') editUser(null)
    else if (d.action === 'users-import') $('[data-users-file]').click()
    else if (d.editUser) editUser(usersRows.find((u) => u.id === Number(d.editUser)))
    else if (d.checkin) checkIn(Number(d.checkin), usersPanel)
    else if (d.checkout) checkOut(Number(d.checkout), usersPanel)
    else if (d.deleteUser && confirmed(btn)) {
      await busy(usersPanel, async () => {
        try {
          await api(`/api/users/${d.deleteUser}`, { method: 'DELETE' })
          toast('Member deleted')
        } catch (err) { toast(err.message) }
      })
      loadUsers()
    }
  })

  $('[data-users-file]').addEventListener('change', async (e) => {
    const file = e.target.files[0]
    if (!file) return
    const data = new FormData()
    data.append('file', file)
    await busy(usersPanel, async () => {
      try {
        const res = await fetch('/api/users/import', { method: 'POST', body: data, credentials: 'same-origin', headers: { Accept: 'application/json', 'X-CSRF-Token': (window.OFFICE && window.OFFICE.csrf) || '' } })
        if (res.status === 401) { location.href = '/login?next=' + encodeURIComponent(location.pathname); return }
        const type = res.headers.get('Content-Type') || ''
        const out = type.includes('application/json') ? await res.json() : await res.text()
        if (!res.ok) throw new Error((out && out.error) || (typeof out === 'string' && out.trim()) || 'Import failed.')
        toast(out.failed ? `Imported ${count(out.imported)}, ${count(out.failed)} failed` : `Imported ${plural(out.imported, 'member')}`)
      } catch (err) { toast(err.message) }
    })
    e.target.value = ''
    reloadUsers()
  })
  loaders.users = loadUsers

  // ---- sessions ----

  const sessionsPanel = $('[data-panel="sessions"]')
  const sessionsForm = $('[data-sessions-filters]')
  let sessionsOffset = 0
  let sessionsRows = []

  function sessionFilters() {
    const f = sessionsForm.elements
    return {
      name: f.name.value.trim(),
      user_id: f.user_id.value.trim(),
      discord_id: f.discord_id.value.trim(),
      from: f.from.value,
      to: f.to.value,
      status: f.status.value === 'all' ? '' : f.status.value,
      check_out_method: f.check_out_method.value === 'all' ? '' : f.check_out_method.value,
    }
  }
  function sessionParams() {
    const f = sessionsForm.elements
    return { ...sessionFilters(), order: f.order.value, sort_by: f.sort_by.value, limit: clampLimit(f.limit, 100), offset: sessionsOffset }
  }

  async function loadSessions() {
    const p = sessionParams()
    const sessionsExport = $('[data-sessions-export]')
    sessionsExport.href = '/api/sessions/export' + qs(p)
    sessionsExport.download = `sessions_${ymd(new Date())}.csv`
    $('[data-sessions-export-all]').download = `sessions_all_${ymd(new Date())}.csv`
    const tbody = $('[data-sessions]')
    let rows, total
    try {
      ;[rows, total] = await Promise.all([api('/api/sessions' + qs(p)), api('/api/sessions/count' + qs(sessionFilters())).catch(() => null)])
    } catch (err) {
      sessionsRows = []
      tbody.innerHTML = emptyRow(7, err.message)
      return
    }
    sessionsRows = list(rows)
    const n = total ? num(total.total) : null
    $('[data-sessions-meta]').textContent = n == null ? '' : plural(n, 'session')
    pager('sessions', { offset: sessionsOffset, limit: p.limit, shown: sessionsRows.length, total: n })
    const deleteShown = $('[data-action="sessions-delete-shown"]')
    deleteShown.disabled = !sessionsRows.length || !qs(sessionFilters())
    deleteShown.title = qs(sessionFilters()) ? '' : 'Set a filter to delete sessions'
    deleteShown.dataset.confirm = `Delete ${plural(sessionsRows.length, 'session')}`
    if (!sessionsRows.length) {
      tbody.innerHTML = emptyRow(7, 'No sessions match these filters.')
      return
    }
    const now = Date.now()
    tbody.innerHTML = sessionsRows.map((s) => {
      const inAt = toDate(s.check_in), outAt = toDate(s.check_out)
      const active = s.active === true || !outAt
      const length = inAt ? duration((outAt ? outAt.getTime() : now) - inAt.getTime()) : '—'
      return `<tr>
        <td class="font-medium">${esc(s.user_name || '—')}</td>
        <td class="tabular-nums">${esc(fmtDateTime(inAt))}</td>
        <td class="tabular-nums">${esc(outAt ? fmtDateTime(outAt) : '—')}</td>
        <td class="tabular-nums">${esc(length)}</td>
        <td>${state(active ? 'Active' : 'Done', active)}</td>
        <td>${esc(methodLabel(s.check_out_method, active))}</td>
        <td class="pr-0"><div class="flex justify-end gap-2">
          <button type="button" class="btn btn-ghost btn-sm" data-edit-session="${s.id}">Edit</button>
          <button type="button" class="btn btn-outline btn-sm" data-delete-session="${s.id}" data-confirm>Delete</button>
        </div></td>
      </tr>`
    }).join('')
  }

  const reloadSessions = () => { sessionsOffset = 0; loadSessions() }
  sessionsForm.addEventListener('input', debounce((e) => { if (e.target.tagName === 'INPUT' && e.target.type !== 'date') reloadSessions() }, 300))
  sessionsForm.addEventListener('change', (e) => { if (e.target.tagName === 'SELECT' || e.target.type === 'date') reloadSessions() })
  sessionsForm.addEventListener('submit', (e) => { e.preventDefault(); reloadSessions() })
  sessionsForm.addEventListener('reset', () => setTimeout(reloadSessions, 0))
  $('[data-pager="sessions"]').addEventListener('click', (e) => {
    const limit = sessionParams().limit
    if (e.target.closest('[data-pager-prev]')) sessionsOffset = Math.max(0, sessionsOffset - limit)
    else if (e.target.closest('[data-pager-next]')) sessionsOffset += limit
    else return
    loadSessions()
  })

  const sessionForm = $('[data-session-form]')
  function editSession(s) {
    if (!s) return
    const f = sessionForm.elements
    f.id.value = s.id
    f.check_in.value = toLocalInput(s.check_in)
    f.check_out.value = toLocalInput(s.check_out)
    sessionForm.dataset.hadCheckout = s.check_out ? '1' : ''
    $('[data-session-form-meta]').textContent = `${s.user_name || 'Unknown'} · ${s.check_out ? 'Done' : 'Active'}`
    $('[data-session-out-note]').textContent = s.check_out ? "A check-out time can be changed but not removed." : 'Leave empty to keep the session open.'
    openDrawer('session')
  }
  sessionForm.addEventListener('submit', async (e) => {
    e.preventDefault()
    const f = sessionForm.elements
    const error = $('[data-form-error]', sessionForm)
    const inAt = fromLocalInput(f.check_in.value)
    const outAt = f.check_out.value ? fromLocalInput(f.check_out.value) : null
    if (!inAt) { error.textContent = 'Enter a check-in time.'; return }
    if (sessionForm.dataset.hadCheckout && !f.check_out.value) { error.textContent = "A check-out time can't be removed."; return }
    if (f.check_out.value && !outAt) { error.textContent = 'Enter a valid check-out time.'; return }
    if (outAt && outAt < inAt) { error.textContent = 'Check-out must be after check-in.'; return }
    const body = { check_in: inAt.toISOString() }
    if (outAt) body.check_out = outAt.toISOString()
    await busy(sessionForm, async () => {
      try {
        await api(`/api/sessions/${f.id.value}`, { method: 'PUT', body })
        toast('Session saved')
        closeDrawer()
        loadSessions()
      } catch (err) { error.textContent = err.message }
    })
  })

  async function deleteSession(id, root) {
    await busy(root, async () => {
      try {
        await api(`/api/sessions/${id}`, { method: 'DELETE' })
        toast('Session deleted')
        if (root === sessionForm) closeDrawer()
      } catch (err) { toast(err.message) }
    })
    loadSessions()
  }
  sessionForm.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-action="session-delete-current"]')
    if (btn && confirmed(btn)) deleteSession(sessionForm.elements.id.value, sessionForm)
  })

  sessionsPanel.addEventListener('click', async (e) => {
    const btn = e.target.closest('button')
    if (!btn) return
    const d = btn.dataset
    if (d.action === 'sessions-refresh') loadSessions()
    else if (d.editSession) editSession(sessionsRows.find((s) => s.id === Number(d.editSession)))
    else if (d.deleteSession && confirmed(btn)) deleteSession(d.deleteSession, sessionsPanel)
    else if (d.action === 'sessions-delete-shown') {
      // The API deletes everything matching the filters, so require at least one.
      if (!qs(sessionFilters()) || !sessionsRows.length || !confirmed(btn)) return
      await busy(sessionsPanel, async () => {
        try {
          const r = await api('/api/sessions' + qs(sessionParams()), { method: 'DELETE' })
          toast(`Deleted ${plural(num(r && r.deleted), 'session')}`)
        } catch (err) { toast(err.message) }
      })
      reloadSessions()
    }
  })
  loaders.sessions = loadSessions

  // ---- scans ----

  const scansPanel = $('[data-panel="scans"]')
  async function loadScans() {
    const tbody = $('[data-scans]')
    let scans
    try { scans = list(await api('/api/rfid/scans')).slice().reverse() } catch (err) {
      tbody.innerHTML = emptyRow(5, err.message)
      return
    }
    $('[data-scans-meta]').textContent = scans.length ? plural(scans.length, 'scan') : ''
    $('[data-action="scans-clear"]').disabled = !scans.length
    if (!scans.length) { tbody.innerHTML = emptyRow(5, 'No scans since the server started.'); return }
    tbody.innerHTML = scans.map((s) => `<tr>
      <td class="tabular-nums">${esc(fmtDateTime(s.timestamp, true))}</td>
      <td class="font-mono text-xs">${esc(s.uid)}</td>
      <td class="${s.user_name ? 'font-medium' : 'text-muted-foreground'}">${esc(s.user_name || '—')}</td>
      <td>${state(s.known ? 'Known' : 'Unknown', s.known)}</td>
      <td>${esc(s.action || '—')}</td>
    </tr>`).join('')
  }
  scansPanel.addEventListener('click', async (e) => {
    const btn = e.target.closest('button')
    if (!btn) return
    if (btn.dataset.action === 'scans-refresh') loadScans()
    else if (btn.dataset.action === 'scans-clear' && confirmed(btn)) {
      await busy(scansPanel, async () => {
        try { await api('/api/rfid/scans', { method: 'DELETE' }); toast('Scan history cleared') } catch (err) { toast(err.message) }
      })
      loadScans()
    }
  })
  loaders.scans = loadScans

  // ---- reports ----

  const periodForm = $('[data-period-form]')
  const leaderboardForm = $('[data-leaderboard-form]')
  const memberSelect = $('[data-member-select]')
  let range = 'weekly'
  let period = null // { from, to, label, totals }
  let reportSeq = 0

  async function loadScheduled() {
    const boxes = $$('[data-scheduled]')
    const label = $('[data-scheduled-status]')
    const note = $('[data-scheduled-note]')
    try {
      const s = (await api('/api/reports/status')) || {}
      $('[data-scheduled="weekly"]').checked = !!s.weekly_enabled
      $('[data-scheduled="monthly"]').checked = !!s.monthly_enabled
      boxes.forEach((b) => { b.disabled = !s.available })
      note.hidden = !!s.available
      label.textContent = !s.available ? 'Unavailable' : s.enabled ? 'On' : 'Off'
      label.className = `kicker ${s.available && s.enabled ? 'text-primary' : 'text-muted-foreground'}`
    } catch (err) {
      boxes.forEach((b) => { b.disabled = true })
      label.textContent = 'Unavailable'
      toast(err.message)
    }
  }
  $$('[data-scheduled]').forEach((box) => box.addEventListener('change', async () => {
    const which = box.dataset.scheduled
    box.disabled = true
    try {
      await api(`/api/reports/toggle${qs({ period: which, enabled: box.checked ? 'true' : 'false' })}`, { method: 'POST' })
      toast(`${which === 'weekly' ? 'Weekly' : 'Monthly'} report ${box.checked ? 'on' : 'off'}`)
    } catch (err) { toast(err.message) }
    loadScheduled()
  }))

  const includeAuto = () => (periodForm.elements.include_auto.checked ? 'true' : '')

  async function loadPeriod() {
    const seq = ++reportSeq
    const inc = includeAuto()
    let report
    if (range === 'custom') {
      const from = periodForm.elements.from.value, to = periodForm.elements.to.value
      if (!from || !to) { toast('Choose both dates'); return }
      report = await api('/api/statistics/report' + qs({ from, to, limit: 10, include_auto_checkout: inc }))
      if (seq !== reportSeq) return
      period = { from, to, label: report.period || `${from} to ${to}` }
    } else {
      report = await api(`/api/statistics/${range}` + qs({ include_auto_checkout: inc }))
      if (seq !== reportSeq) return
      period = { from: ymd(report.start_date), to: ymd(report.end_date), label: report.period }
    }
    period.totals = report
    renderSummary(report)
    return seq
  }

  function renderSummary(r) {
    const set = (k, v) => { $(`[data-report="${k}"]`).textContent = v }
    $('[data-period-label]').textContent = period.label || ''
    set('hours', fixed(r.total_hours, 1))
    set('visits', count(r.total_visits))
    set('users', count(r.unique_users))
    set('days', count(r.active_days))
    set('avg', fixed(r.average_per_user, 2))
    const top = list(r.top_users)[0]
    set('top', top ? top.name : '—')
    set('top-hours', top ? `${fixed(top.total_hours, 1)} h` : '')
  }

  async function loadLeaderboard() {
    if (!period) return
    const f = leaderboardForm.elements
    const limit = Math.min(50, Math.max(1, Math.floor(Number(f.limit.value)) || 10))
    const tbody = $('[data-leaderboard]')
    $('[data-leaderboard-meta]').textContent = `By ${f.rank_by.value}`
    try {
      const data = await api('/api/statistics/leaderboard' + qs({ rank_by: f.rank_by.value, limit, from: period.from, to: period.to, include_auto_checkout: includeAuto() }))
      const users = list(data && data.users)
      tbody.innerHTML = users.length
        ? users.map((u, i) => `<tr>
            <td class="font-mono text-xs tabular-nums text-muted-foreground">${i + 1}</td>
            <td class="font-medium">${esc(u.name)}</td>
            <td class="text-right tabular-nums">${fixed(u.total_hours, 1)}</td>
            <td class="text-right tabular-nums">${count(u.visit_count)}</td>
            <td class="text-right tabular-nums">${count(u.active_days)}</td>
            <td class="text-right tabular-nums">${fixed(u.avg_duration, 2)}</td>
          </tr>`).join('')
        : emptyRow(6, 'No visits in this period.')
    } catch (err) { tbody.innerHTML = emptyRow(6, err.message) }
  }

  async function loadMemberOptions() {
    try {
      const users = list(await api('/api/users?limit=0&sort_by=name&order=asc'))
      const keep = memberSelect.value
      memberSelect.innerHTML = '<option value="">Choose a member</option>' + users.map((u) => `<option value="${u.id}">${esc(u.name)}</option>`).join('')
      if (keep && users.some((u) => String(u.id) === keep)) memberSelect.value = keep
    } catch (err) { toast(err.message) }
  }

  function clearMember() {
    $$('[data-member]').forEach((el) => { el.textContent = '—' })
    $('[data-member-meta]').textContent = ''
    $('[data-member-sessions]').innerHTML = emptyRow(4, 'Choose a member to see their sessions.')
  }

  async function loadMember() {
    const id = memberSelect.value
    if (!id) { clearMember(); return }
    if (!period) return
    const inc = includeAuto()
    const span = { from: period.from, to: period.to, include_auto_checkout: inc }
    const set = (k, v) => { $(`[data-member="${k}"]`).textContent = v }
    $('[data-member-meta]').textContent = period.label || ''
    try {
      const [stats, hours, visits] = await Promise.all([
        api(`/api/statistics/users/${id}` + qs(span)),
        api('/api/statistics/leaderboard' + qs({ rank_by: 'hours', limit: 100, ...span })),
        api('/api/statistics/leaderboard' + qs({ rank_by: 'visits', limit: 100, ...span })),
      ])
      const totals = period.totals || {}
      const rank = (rows) => rows.findIndex((u) => String(u.user_id) === id) + 1
      const hu = list(hours && hours.users), vu = list(visits && visits.users)
      const participants = Math.max(hu.length, vu.length)
      set('hours', fixed(stats.total_hours, 1))
      set('visits', count(stats.visit_count))
      set('days', count(stats.active_days))
      set('avg', fixed(stats.avg_duration, 2))
      set('hours-pct', `${fixed(totals.total_hours ? (num(stats.total_hours) / totals.total_hours) * 100 : 0, 1)}%`)
      set('visits-pct', `${fixed(totals.total_visits ? (num(stats.visit_count) / totals.total_visits) * 100 : 0, 1)}%`)
      set('hours-rank', rank(hu) ? `${rank(hu)}/${participants}` : '—')
      set('visits-rank', rank(vu) ? `${rank(vu)}/${participants}` : '—')
      set('first', fmtDate(stats.first_visit))
      set('last', fmtDate(stats.last_visit))
    } catch (err) { toast(err.message) }

    const tbody = $('[data-member-sessions]')
    try {
      const rows = list(await api('/api/sessions' + qs({ user_id: id, sort_by: 'check_in', order: 'desc', limit: 20 })))
      tbody.innerHTML = rows.length
        ? rows.map((s) => {
            const inAt = toDate(s.check_in), outAt = toDate(s.check_out)
            return `<tr>
              <td class="tabular-nums">${esc(fmtDateTime(inAt))}</td>
              <td class="tabular-nums">${esc(outAt ? fmtDateTime(outAt) : '—')}</td>
              <td>${esc(methodLabel(s.check_out_method, !outAt))}</td>
              <td class="text-right tabular-nums">${inAt && outAt ? fixed((outAt - inAt) / 36e5, 2) : '—'}</td>
            </tr>`
          }).join('')
        : emptyRow(4, 'No sessions yet.')
    } catch (err) { tbody.innerHTML = emptyRow(4, err.message) }
  }

  async function loadStatistics() {
    try {
      if (!(await loadPeriod())) return
    } catch (err) { toast(err.message); return }
    loadLeaderboard()
    loadMember()
  }

  function setRange(next) {
    range = next
    $$('[data-range]').forEach((b) => b.setAttribute('aria-pressed', b.dataset.range === range ? 'true' : 'false'))
    $('[data-custom-range]').hidden = range !== 'custom'
  }
  periodForm.addEventListener('click', (e) => {
    const b = e.target.closest('[data-range]')
    if (!b) return
    setRange(b.dataset.range)
    if (range === 'custom') periodForm.elements.from.focus()
    else loadStatistics()
  })
  periodForm.addEventListener('submit', (e) => { e.preventDefault(); loadStatistics() })
  periodForm.elements.include_auto.addEventListener('change', () => { if (range !== 'custom' || (periodForm.elements.from.value && periodForm.elements.to.value)) loadStatistics() })
  periodForm.addEventListener('reset', () => setTimeout(() => {
    setRange('weekly')
    leaderboardForm.reset()
    loadStatistics()
  }, 0))
  leaderboardForm.addEventListener('change', loadLeaderboard)
  leaderboardForm.addEventListener('input', debounce((e) => { if (e.target.name === 'limit') loadLeaderboard() }, 300))
  leaderboardForm.addEventListener('submit', (e) => { e.preventDefault(); loadLeaderboard() })
  memberSelect.addEventListener('change', loadMember)

  loaders.reports = async () => {
    loadScheduled()
    await loadMemberOptions()
    if (!memberSelect.value) clearMember()
    loadStatistics()
  }

  // ---- start ----

  showTab(location.hash.slice(1))
})()
