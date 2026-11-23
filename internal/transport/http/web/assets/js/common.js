// Shared helpers for every office-tracker page: theme selector, API calls and toasts.
;(function () {
  'use strict'
  const root = document.documentElement
  const select = document.querySelector('[data-theme-select]')
  let stored = null
  try { stored = localStorage.getItem('payload-theme') } catch (e) {}
  if (select) {
    select.value = stored === 'light' || stored === 'dark' ? stored : 'auto'
    select.addEventListener('change', () => {
      const v = select.value
      try { v === 'auto' ? localStorage.removeItem('payload-theme') : localStorage.setItem('payload-theme', v) } catch (e) {}
      root.dataset.theme = v === 'light' || v === 'dark' ? v : matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
    })
  }

  // Header turns translucent on scroll, like the website.
  const header = document.querySelector('[data-site-header]')
  if (header) {
    const onScroll = () => {
      const scrolled = window.scrollY > 24
      header.classList.toggle('border-border', scrolled)
      header.classList.toggle('bg-background/80', scrolled)
      header.classList.toggle('backdrop-blur-md', scrolled)
      header.classList.toggle('border-transparent', !scrolled)
      header.classList.toggle('bg-background', !scrolled)
    }
    onScroll()
    addEventListener('scroll', onScroll, { passive: true })
  }

  // Close the user menu when clicking elsewhere.
  document.addEventListener('click', (e) => {
    document.querySelectorAll('[data-user-menu][open]').forEach((d) => { if (!d.contains(e.target)) d.removeAttribute('open') })
  })

  let toastTimer
  function toast(message) {
    const el = document.getElementById('toast')
    if (!el) return
    el.textContent = message
    el.style.opacity = '1'
    clearTimeout(toastTimer)
    toastTimer = setTimeout(() => { el.style.opacity = '0' }, 2800)
  }

  class ApiError extends Error {}

  // JSON API helper. Mutations carry the CSRF token; a 401 sends the person to sign in.
  async function api(path, options = {}) {
    const init = { method: options.method || 'GET', credentials: 'same-origin', headers: { Accept: 'application/json' } }
    if (options.body !== undefined) {
      init.headers['Content-Type'] = 'application/json'
      init.body = JSON.stringify(options.body)
    }
    if (init.method !== 'GET') init.headers['X-CSRF-Token'] = (window.OFFICE && window.OFFICE.csrf) || ''
    const res = await fetch(path, init)
    if (res.status === 401) {
      location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search)
      throw new ApiError('Signed out')
    }
    const type = res.headers.get('Content-Type') || ''
    const data = type.includes('application/json') ? await res.json() : await res.text()
    if (!res.ok) throw new ApiError((data && data.error) || (typeof data === 'string' && data.trim()) || 'Something went wrong. Try again.')
    return data
  }

  function escapeHTML(s) {
    return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c])
  }

  window.Office = { api, toast, escapeHTML, ApiError }
})()
