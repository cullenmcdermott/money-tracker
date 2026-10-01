// Formatting, escaping, icons and date helpers shared by the views.
export const $ = s => document.querySelector(s)
export const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c])
const nf0 = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 })
const nf2 = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' })
// Privacy mode masks every dollar amount (percentages, charts and bar lengths stay); main.js flips it.
export const privacy = { on: false }
const MASK = '$•••'
export const usd0 = c => privacy.on ? MASK : nf0.format(c / 100).replace('-', '−')
export const usd2 = c => privacy.on ? MASK : nf2.format(c / 100).replace('-', '−')
export const pct = x => x < 0.01 ? '<1%' : Math.round(x * 100) + '%'
export const f1 = n => +n.toFixed(1)
export const clip = (s, n) => s.length > n ? s.slice(0, n - 1).trimEnd() + '…' : s
// Axis label for a dollar amount: $500, $2.5k, $1.2M.
export const axis = v => privacy.on ? '' : v === 0 ? '$0' : Math.abs(v) >= 1e6 ? '$' + +(v / 1e6).toFixed(1) + 'M' : Math.abs(v) >= 1000 ? '$' + +(v / 1000).toFixed(1) + 'k' : '$' + v
// Smallest 1/2/2.5/5 x 10^n step that fits `range` in at most n ticks.
export function niceStep(range, n) {
  const raw = Math.max(range, 1) / n, p = 10 ** Math.floor(Math.log10(raw))
  return [1, 2, 2.5, 5, 10].map(m => m * p).find(s => s >= raw)
}

const P = {
  overview: '<path d="M3 6h5c5 0 5 12 10 12h3M3 18h5c2 0 3.2-1.6 4.2-3.5M12.2 9.5C13.2 7.6 14.5 6 18 6h3"/>',
  transactions: '<path d="M8 6h13M8 12h13M8 18h13M3.5 6h.01M3.5 12h.01M3.5 18h.01"/>',
  accounts: '<path d="M3 10l9-6 9 6M5 10v8M9.5 10v8M14.5 10v8M19 10v8M3 20h18"/>',
  spending: '<path d="M5 20V10M12 20V4M19 20v-7"/>',
  settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  chevL: '<path d="M15 5l-7 7 7 7"/>', chevR: '<path d="M9 5l7 7-7 7"/>', chevD: '<path d="M6 9l6 6 6-6"/>',
  sync: '<path d="M20 11a8 8 0 0 0-14-4M4 13a8 8 0 0 0 14 4M6 3v4h4M18 21v-4h-4"/>',
  alert: '<path d="M12 4l9 16H3z"/><path d="M12 10v4M12 17h.01"/>',
  search: '<circle cx="11" cy="11" r="6"/><path d="M20 20l-4.5-4.5"/>',
  plan: '<path d="M4 20V4M4 5h11l-2 4 2 4H4"/>',
  investments: '<path d="M3 17l6-6 4 4 8-8M15 7h6v6"/>',
  theme: '<circle cx="12" cy="12" r="8"/><path d="M12 4v16"/>',
  eye: '<path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  eyeOff: '<path d="M3 3l18 18M10.6 5.1A10.4 10.4 0 0 1 12 5c6.4 0 10 7 10 7a17 17 0 0 1-3.1 3.9M6.6 6.6C3.8 8.4 2 12 2 12s3.6 7 10 7c1.9 0 3.5-.6 4.9-1.4M9.9 9.9a3 3 0 0 0 4.2 4.2"/>',
  arrow: '<path d="M5 12h14M13 6l6 6-6 6"/>',
  check: '<path d="M5 12l5 5 9-10"/>',
  x: '<path d="M6 6l12 12M18 6L6 18"/>',
  edit: '<path d="M4 20h4L19 9l-4-4L4 16zM13.5 6.5l4 4"/>',
}
export const icon = (n, s = 20, cls = '') => `<svg class="ic${cls}" width="${s}" height="${s}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${P[n]}</svg>`
export const brandHTML = '<svg width="24" height="24" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 1 18 11 12 23 6 13Z" fill="currentColor"/><path d="M12 1 18 11 12 13Z" fill="#ED533D"/><path d="M12 23 6 13 12 11Z" fill="#E8EDF2"/></svg><span>Money Tracker</span>'

// Dates. Months are 'YYYY-MM', days 'YYYY-MM-DD'; formatted in UTC so they never shift a day.
const fmt = (s, o) => new Date(s + (s.length === 7 ? '-01' : '') + 'T12:00:00Z').toLocaleString('en-US', { ...o, timeZone: 'UTC' })
export const mName = m => fmt(m, { month: 'long', year: 'numeric' })
export const mShort = m => fmt(m, { month: 'short' })
export const dShort = d => fmt(d, { month: 'short', day: 'numeric' })
export const dMed = d => fmt(d, { month: 'short', day: 'numeric', year: 'numeric' })
export const dLong = d => fmt(d, { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' })
export function rel(iso) {
  if (!iso || isNaN(Date.parse(iso))) return 'never'
  const m = Math.max(0, Math.round((Date.now() - new Date(iso)) / 60000))
  if (m < 1) return 'just now'
  if (m < 60) return m + ' min ago'
  const h = Math.round(m / 60)
  if (h < 24) return h + (h === 1 ? ' hour ago' : ' hours ago')
  const d = Math.round(h / 24)
  return d + (d === 1 ? ' day ago' : ' days ago')
}
export const ym = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`
