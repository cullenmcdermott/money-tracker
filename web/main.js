import './style.css'
import { $, esc, privacy, usd0, usd2, pct, f1, clip, axis, niceStep, icon, brandHTML, mName, mShort, dShort, dMed, dLong, rel, ym } from './util.js'

/* ---------- API ---------- */
async function api(path, options, withTotal) {
  const res = await fetch('/api' + path, options)
  let body = null
  try { body = await res.json() } catch { /* non-JSON error page */ }
  if (res.status === 401) { location.assign('/auth/login?return=' + encodeURIComponent(location.pathname + location.search + location.hash)); await new Promise(() => {}) } // auth: session missing or expired
  if (!res.ok) throw Object.assign(new Error(body?.error || `Request failed (${res.status})`), { status: res.status })
  return withTotal ? { data: body, total: +res.headers.get('X-Total-Count') || 0 } : body
}
const send = (method, path, body) => api(path, { method, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) })

/* ---------- state ---------- */
const D = { monthly: [], networth: [], accounts: [], items: [], cats: [], config: {}, rules: null, recurring: null, alerts: null }
const cache = new Map() // 'c:<period>' categories, 'i:<period>' income sources, 'u:<period>' uncategorized count
const inflight = new Set()
const acctTx = new Map()
let MONTHS = [], YEARS = [], CUR = ym(new Date()), acct = {}
const NOW_M = CUR
const blankF = () => ({ month: 'all', cat: 'all', acct: 'all', q: '', flow: '' })
const state = {
  view: 'overview', mode: 'month', month: CUR, year: +CUR.slice(0, 4), f: blankF(), editing: null, txMsg: null,
  open: null, sel: null, animated: false, theme: 'system', lastPT: 'mouse', loaded: false, err: '', perr: {},
  sync: {}, ruleDel: null, rulesErr: '',
}
try { state.theme = localStorage.getItem('theme') || 'system' } catch { /* storage blocked */ }
try { privacy.on = localStorage.getItem('privacy') === '1' } catch { /* storage blocked */ }
const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches
const PAGE = 120
let cur = null

/* ---------- derived data ---------- */
const catName = raw => raw === '' ? 'Uncategorized' : /^[A-Z][A-Z0-9_]+$/.test(raw) ? raw.toLowerCase().replaceAll('_', ' ').replace(/^./, c => c.toUpperCase()) : raw
const KNOWN = new Set(['housing', 'groceries', 'dining', 'insurance', 'utilities', 'transportation', 'shopping', 'subscriptions', 'health', 'travel', 'uncategorized', 'income', 'transfer', 'other'])
const catColor = raw => {
  const n = raw === '' ? 'uncategorized' : raw.toLowerCase()
  return KNOWN.has(n) ? `var(--cat-${n})` : `var(--cat-x${[...n].reduce((h, c) => h + c.charCodeAt(0), 0) % 4})`
}
const monthsBetween = (a, b) => {
  const out = []
  for (let [y, m] = [+a.slice(0, 4), +a.slice(5)]; `${y}-${String(m).padStart(2, '0')}` <= b; m > 11 ? (y++, m = 1) : m++) out.push(`${y}-${String(m).padStart(2, '0')}`)
  return out
}
function derive() {
  CUR = [NOW_M, D.monthly.at(-1)?.month].filter(Boolean).sort().at(-1) // server clock may run ahead of the browser's
  const first = [D.monthly[0]?.month, CUR].filter(Boolean).sort()[0], last = CUR
  MONTHS = monthsBetween(first, last)
  YEARS = [...new Set(MONTHS.map(m => m.slice(0, 4)))].map(Number)
  acct = Object.fromEntries(D.accounts.map(a => [a.id, a]))
  if (!MONTHS.includes(state.month)) state.month = CUR
  if (!YEARS.includes(state.year)) state.year = +CUR.slice(0, 4)
}
const totals = ms => D.monthly.filter(r => ms.includes(r.month)).reduce((t, r) => ({ income: t.income + r.income, expense: t.expense + r.expense, invested: t.invested + (r.invested || 0) }), { income: 0, expense: 0, invested: 0 })
// mode 'range' is a zoom: the months dragged across on a chart (state.range), kept as the key YYYY-MM..YYYY-MM.
const periodKey = () => state.mode === 'range' ? state.range.join('..') : state.mode === 'month' ? state.month : String(state.year)
const periodMonths = () => state.mode === 'range' ? monthsBetween(...state.range) : state.mode === 'month' ? [state.month] : MONTHS.filter(m => m.startsWith(String(state.year)))
// The overview's charts all show the 12 months ending where the selected period ends, or just the months zoomed to.
const windowMonths = () => { const e = periodMonths().at(-1); return state.mode === 'range' ? periodMonths() : monthsBetween(ym(new Date(+e.slice(0, 4), +e.slice(5) - 12, 1)), e) }
function zoom(a, b) {
  b = [b, CUR].sort()[0]; if (b < MONTHS[0]) return; a = [a, MONTHS[0]].sort().at(-1)
  if (state.mode !== 'range') state.zoomFrom = { mode: state.mode, month: state.month, year: state.year }
  if (a === b) Object.assign(state, { mode: 'month', month: a })
  else Object.assign(state, { mode: 'range', range: [a, b], month: b })
  state.year = +state.month.slice(0, 4); state.sel = null; render()
}
const rangeLabel = ms => ms.length === 1 ? mName(ms[0]) : `${mShort(ms[0])} ${ms[0].slice(0, 4)} to ${mShort(ms.at(-1))} ${ms.at(-1).slice(0, 4)}`
// Net worth points inside that window; the last one is the value at the end of the selected period.
const nwPoints = () => { const ms = windowMonths(), a = ms[0] + '-01', b = ms.at(-1) + '-31'; return D.networth.filter(p => p.date >= a && p.date <= b) }
const monthOf = m => D.monthly.find(r => r.month === m) || { income: 0, expense: 0 }
const errItems = () => D.items.filter(i => i.last_error)
// The first day or two of a month has nothing posted yet (SimpleFIN runs about a day behind), so the overview opens on last month.
const curEmpty = () => !monthOf(CUR).income && !monthOf(CUR).expense

async function loadCore() {
  const [monthly, networth, accounts, items, cats] = await Promise.all([api('/summary/monthly'), api('/networth'), api('/accounts'), api('/items'), api('/categories')])
  Object.assign(D, { monthly, networth, accounts, items, cats })
  cache.clear(); acctTx.clear(); state.perr = {}; D.recurring = null; D.alerts = null
  derive()
}
async function reload() {
  try { const first = !state.loaded; await loadCore(); state.err = ''; state.loaded = true; if (first) { state.month = curEmpty() && MONTHS.length > 1 ? MONTHS.at(-2) : CUR; state.year = +state.month.slice(0, 4) } } catch (e) { state.err = e.message }
  render()
}
const load = (k, path, withTotal) => cache.has(k) ? 0 : api(path, null, withTotal).then(v => cache.set(k, v))
const ensurePeriod = key => Promise.all([load('c:' + key, '/summary/categories?month=' + key), load('i:' + key, '/summary/income?month=' + key), load('u:' + key, `/transactions?uncategorized=1&month=${key}&limit=1`, true)])
const periodReady = key => ['c:', 'i:', 'u:'].every(p => cache.has(p + key))

/* ---------- flow model ---------- */
function buildModel(key) {
  const t = totals(periodMonths()), total = Math.max(t.income, t.expense)
  const cats = cache.get('c:' + key).map(r => ({ raw: r.category, name: catName(r.category), amt: r.amount }))
  const inc = cache.get('i:' + key), top = inc.slice(0, 3), rest = inc.slice(3).reduce((s, r) => s + r.amount, 0)
  const src = top.map((r, i) => ({ id: 'src:' + i, name: r.source, amt: r.amount, kind: 'src', color: 'var(--income)', f: { flow: 'in', q: r.source } }))
  if (rest > 0) src.push({ id: 'src:other', name: 'Other', amt: rest, kind: 'src', color: 'var(--income)', f: { flow: 'in' } })
  if (t.expense > t.income) src.push({ id: 'src:savings', name: 'From savings', amt: t.expense - t.income, kind: 'src', color: 'var(--ink2)' })
  let out = cats.map((c, i) => ({ id: 'cat:' + i, name: c.name, amt: c.amt, kind: 'cat', color: catColor(c.raw), f: { cat: c.raw === '' ? '__none' : c.raw } }))
  // Categories under 3% of spending fold into one flow (the top 6 always show) until it is clicked open.
  const small = out.slice(6).filter(o => o.amt < t.expense * 0.03)
  if (!state.flowAll && small.length > 1) {
    out = out.filter(o => !small.includes(o))
    out.push({ id: 'cat:small', name: `${small.length} more`, amt: small.reduce((s, o) => s + o.amt, 0), kind: 'cat', color: 'var(--cat-other)', group: true })
  }
  if (t.income > t.expense) out.push({ id: 'kept', name: 'Kept', amt: t.income - t.expense, kind: 'kept', color: 'var(--kept)' })
  return { key, a: t, total, src, out, uncat: cache.get('u:' + key).total }
}
const canDrill = o => !!o.f

/* ---------- period navigator ---------- */
function navHTML(withToggle) {
  const isM = state.mode === 'month' || !withToggle
  const i = MONTHS.indexOf(state.month)
  const prevOff = isM ? i <= 0 : state.year <= YEARS[0], nextOff = isM ? i >= MONTHS.length - 1 : state.year >= YEARS.at(-1)
  const title = isM ? mName(state.month) : String(state.year)
  if (withToggle && state.mode === 'range') return `<div class="pnav"><div class="stepper"><h1 id="ptitle" aria-live="polite">${esc(rangeLabel(periodMonths()))}</h1></div>
    <div class="frow" style="margin:0"><button class="btn ghost sm" data-act="unzoom">Reset zoom</button><div class="seg" role="group" aria-label="Period length"><button data-act="mode" data-v="month" aria-pressed="false">Month</button><button data-act="mode" data-v="year" aria-pressed="false">Year</button></div></div></div>`
  return `<div class="pnav"><div class="stepper">
    <button class="step" data-act="step" data-d="-1" ${prevOff ? 'disabled' : ''} aria-label="Previous ${isM ? 'month' : 'year'}">${icon('chevL')}</button>
    <h1 id="ptitle" aria-live="polite">${title}</h1>
    <button class="step" data-act="step" data-d="1" ${nextOff ? 'disabled' : ''} aria-label="Next ${isM ? 'month' : 'year'}">${icon('chevR')}</button></div>
    ${withToggle ? `<div class="seg" role="group" aria-label="Period length"><button data-act="mode" data-v="month" aria-pressed="${state.mode === 'month'}">Month</button><button data-act="mode" data-v="year" aria-pressed="${state.mode === 'year'}">Year</button></div>` : ''}</div>`
}
function step(d) {
  if (state.view === 'report' || state.mode === 'month') {
    state.month = MONTHS[Math.min(MONTHS.length - 1, Math.max(0, MONTHS.indexOf(state.month) + d))]
    state.year = +state.month.slice(0, 4)
  } else state.year = YEARS[Math.min(YEARS.length - 1, Math.max(0, YEARS.indexOf(state.year) + d))]
  state.sel = null
  render()
}
const syncLine = () => {
  const last = D.items.map(c => c.last_synced_at).filter(Boolean).sort().pop()
  return last ? `Synced ${rel(last)}` : 'Not synced yet'
}
const notice = (text, extra = '') => `<div class="notice" role="status">${icon('alert', 18)}<span>${esc(text)}</span>${extra}</div>`
const retry = act => `<button class="btn ghost sm" data-act="${act}">Retry</button>`
const banner = () => state.err ? notice(state.err, retry('reload')) : ''
const today = () => { const d = new Date(); return `${ym(d)}-${String(d.getDate()).padStart(2, '0')}` }

/* ---------- Overview ---------- */
function overview() {
  if (!D.items.length) return firstRun()
  const key = periodKey(), isM = state.mode === 'month', now = new Date()
  let sub
  if (state.mode === 'range') sub = `${periodMonths().length} months, zoomed in${periodMonths().includes(CUR) ? ` (${mShort(CUR)} is month to date)` : ''}`
  else if (isM) {
    sub = state.month === CUR ? `Month to date${CUR === NOW_M ? `, through ${dShort(today())}` : ''}` : 'Full month'
    if (state.month === MONTHS.at(-2) && curEmpty()) sub += ` <span aria-hidden="true">·</span> ${mName(CUR)} has nothing posted yet. <button class="linkbtn" data-act="step" data-d="1">See ${mName(CUR)}</button>`
  } else {
    const y = state.year, ms = MONTHS.filter(m => m.startsWith(String(y))), from = mShort(ms[0]), to = mShort(ms.at(-1))
    const partial = ms[0] !== `${y}-01`
    sub = y === now.getFullYear() ? `Year to date, ${from} to ${to}${partial ? ', where the history starts' : ''}` : partial ? `${from} to ${to} only, where the history starts` : 'Full year'
  }
  const notices = errItems().map(c => `<div class="notice" role="status">${icon('alert', 18)}<span>${esc(c.last_error)}${c.last_synced_at ? ` Last synced ${rel(c.last_synced_at)}.` : ''}</span><a href="#settings">Open Settings</a></div>`).join('')
  let body, stats
  const nwTotal = nwPoints().length ? usd0(nwPoints().at(-1).total) : '—'
  const err = state.perr[key]
  if (periodReady(key)) {
    cur = buildModel(key)
    const kept = cur.a.income - cur.a.expense
    const share = c => privacy.on && cur.a.income ? `${pct(c / cur.a.income)}<small class="muted" style="font-weight:400;font-size:12px"> of income</small>` : usd0(c) // privacy: shares instead of masks
    stats = `<div><span class="lab"><i style="background:var(--income)"></i>In</span><b>${usd0(cur.a.income)}</b></div><div><span class="lab"><i style="background:var(--spend)"></i>Out</span><b>${share(cur.a.expense)}</b></div><div><span class="lab"><i style="background:${kept >= 0 ? 'var(--kept)' : 'var(--ink2)'}"></i>${kept >= 0 ? 'Kept' : 'Over by'}</span><b>${share(Math.abs(kept))}</b></div>${cur.a.invested ? `<div><span class="lab"><i style="background:var(--cat-x3)"></i>${cur.a.invested > 0 ? 'Invested' : 'Taken from investments'}</span><b>${share(Math.abs(cur.a.invested))}</b></div>` : ''}`
    body = cur.total ? `<div id="readout" aria-live="polite"></div><div id="flowbox"></div><p class="cap">${state.flowAll ? '<button class="linkbtn" data-act="flowless">Group small categories</button> ' : ''}Line thickness is proportional to amount. Counts checking, savings and credit cards; excludes transfers, investment and loan accounts, and pending transactions.<span id="mincap"></span></p>`
      : isM && key === CUR && MONTHS.length > 1 ? `<div class="empty"><p><strong>${mName(key)} just started</strong></p><p>Transactions usually show up a day or two after they're made.</p><p><button class="btn ghost" data-act="step" data-d="-1">See ${mName(MONTHS.at(-2))}</button></p></div>`
      : `<div class="empty"><p><strong>Nothing recorded for ${esc(isM ? mName(key) : key)}</strong></p><p>No income or spending in this period. Try another one.</p></div>`
  } else {
    cur = null
    stats = ''
    body = err ? `<div class="empty"><p><strong>Could not load this period</strong></p><p>${esc(err)}</p><p><button class="btn ghost" data-act="retryperiod">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'
    if (!err) startPeriod(key)
  }
  const uc = cur?.uncat || 0
  loadAlerts()
  return `${banner()}${navHTML(true)}
  <p class="sub">${sub} <span aria-hidden="true">·</span> ${syncLine()}</p>
  ${notices}
  <div class="grid">
   <section class="panel" aria-labelledby="fh"><div class="phead"><h2 id="fh" class="sr">Money flow, ${esc(rangeLabel(periodMonths()))}</h2><div class="fstats">${stats}<div class="nw"><span class="lab">Net worth at period end</span><a href="#accounts"><b>${nwTotal}</b></a></div></div></div>
     ${body}</section>
   <section class="panel" aria-labelledby="nh"><h2 id="nh">Net worth</h2><p class="prange">${esc(rangeLabel(windowMonths()))}</p>${nwHTML()}</section>
  </div>
  <section class="panel cfp" aria-labelledby="ch"><div class="phead"><div><h2 id="ch">Cash flow</h2><p class="prange">${esc(rangeLabel(windowMonths()))}</p></div>
    <div class="legend"><span><i style="background:var(--income)"></i>Income</span><span><i style="background:var(--spend)"></i>Spending</span></div></div>
    <div class="cfwrap" id="cfwrap"></div>
    <p class="cap">Counts checking, savings and credit cards; excludes transfers, investment and loan accounts, and pending transactions. Highlighted bars are the selected period.${windowMonths().includes(CUR) ? ` ${esc(mName(CUR).replace(/ \d+$/, ''))} is month to date.` : ''} Select a bar to open its transactions, or drag across bars to zoom in.</p></section>
  ${alertLine()}
  ${(nt => nt ? `<p class="need">${nt} account${nt === 1 ? '' : 's'} may have the wrong type. <button class="linkbtn" data-act="go" data-v="accounts">Check account types ${icon('arrow', 14)}</button></p>` : '')(D.accounts.filter(a => !a.type_confident).length)}
  ${uc ? `<p class="need">${uc}${uc >= 1000 ? '+' : ''} transaction${uc === 1 ? '' : 's'} need${uc === 1 ? 's' : ''} a category in this period. <button class="linkbtn" data-act="needcat">Review by merchant ${icon('arrow', 14)}</button></p>` : ''}`
}
function startPeriod(key) {
  if (inflight.has(key)) return
  inflight.add(key)
  ensurePeriod(key).catch(e => { state.perr[key] = e.message }).finally(() => { inflight.delete(key); if (state.view === 'overview' && periodKey() === key) render() })
}
function firstRun() {
  return `${banner()}<section class="panel first"><h1>Nothing connected yet</h1>
    <p>Money Tracker shows income, spending and net worth once it has accounts to read.</p>
    <p>Configure SimpleFIN (see Settings) and the first sync brings in about three months of transactions. Net worth history starts from that first sync.</p>
    <a class="btn" href="#settings">Open Settings ${icon('arrow', 16)}</a></section>`
}
const nwSplit = p => `Assets ${usd0(p.assets)} <span aria-hidden="true">·</span> Debts ${usd0(p.debts)}`
function nwHTML() {
  const pts = nwPoints()
  if (!pts.length) return `<div class="nwbig">—</div><div class="nwd">No balance history for these months.</div><p class="cap">Balances are recorded at each sync. <button class="linkbtn" data-act="go" data-v="settings">Settings ${icon('arrow', 14)}</button></p>`
  const ms = periodMonths(), first = ms[0] + '-01', last = ms.at(-1) + '-31'
  const inP = pts.filter(p => p.date >= first && p.date <= last)
  let d
  if (inP.length > 1) { const a = inP[0], b = inP.at(-1), df = b.total - a.total; d = `${df >= 0 ? '+' : '−'}${usd0(Math.abs(df))}${state.mode === 'range' ? ' over these months' : ` in ${state.mode === 'month' ? mName(state.month) : state.year} (shaded)`}` }
  else d = 'No balance history for this period.'
  return `<div class="nwbig" id="nwval">${usd0(pts.at(-1).total)}</div><div class="nwd" id="nwlbl">${d}</div><div class="nwad" id="nwad">${nwSplit(pts.at(-1))}</div>
   ${pts.length > 1 ? `<button class="nwc" data-act="go" data-v="accounts" aria-label="Net worth trend since ${dShort(pts[0].date)}. Open accounts." id="nwc"></button>` : ''}
   <p class="cap">Balance at the end of the selected period${state.mode === 'range' ? '' : ', with the 12 months before it'}. Drag across the chart to zoom in.${pts.length < 2 ? ' One day of history so far, so there is no trend yet.' : ''} <button class="linkbtn" data-act="go" data-v="accounts">Accounts ${icon('arrow', 14)}</button></p>`
}

/* flow diagram */
const rib = (x1, a1, b1, x2, a2, b2) => { const xm = (x1 + x2) / 2; return `M${f1(x1)},${f1(a1)}C${f1(xm)},${f1(a1)} ${f1(xm)},${f1(a2)} ${f1(x2)},${f1(a2)}L${f1(x2)},${f1(b2)}C${f1(xm)},${f1(b2)} ${f1(xm)},${f1(b1)} ${f1(x1)},${f1(b1)}Z` }
const vrib = (y1, a1, b1, y2, a2, b2) => { const ym = (y1 + y2) / 2; return `M${f1(a1)},${f1(y1)}C${f1(a1)},${f1(ym)} ${f1(a2)},${f1(ym)} ${f1(a2)},${f1(y2)}L${f1(b2)},${f1(y2)}C${f1(b2)},${f1(ym)} ${f1(b1)},${f1(ym)} ${f1(b1)},${f1(y1)}Z` }
function flowSVG(m, W, anim) {
  const mob = W < 640, gap = 4, minR = 24, MIN = 1.5
  const HB = mob ? 220 : 230, k = HB / m.total
  let y = 0
  const R = m.out.map(o => { const t = Math.max(o.amt * k, MIN), s = Math.max(t, minR), it = { ...o, t, s, top: y }; y += s + gap; return it })
  const sumR = y - gap, thR = R.reduce((a, o) => a + o.t, 0)
  let L = [], sumL = 0, thL = 0, H, barTop, rTop, lTop, xL, xH, xHr, xR, Hb
  const HW = 12
  const clamped = [...m.out, ...m.src].some(o => o.amt * k < MIN)
  if (mob) {
    const Wp = 64, pipeX = 4; xR = pipeX + Wp + 40
    L = m.src.map(o => ({ ...o, t: Math.max(o.amt / m.total * Wp, MIN) })); barTop = rTop = 118; Hb = thR; H = 118 + Math.max(sumR, Hb) + 8; xH = pipeX; xHr = pipeX + Wp
  } else {
    let yl = 0; L = m.src.map(o => { const t = Math.max(o.amt * k, MIN), s = Math.max(t, 44), it = { ...o, t, s, top: yl }; yl += s + 14; return it })
    sumL = yl - 14; thL = L.reduce((a, o) => a + o.t, 0); Hb = Math.max(thL, thR)
    const Hc = Math.max(sumR, sumL, Hb); H = Hc + 44; const T0 = 30
    barTop = T0 + (Hc - Hb) / 2; rTop = T0 + (Hc - sumR) / 2; lTop = T0 + (Hc - sumL) / 2
    xL = Math.round(Math.min(178, W * .22)); xR = W - 216; xH = (xL + 10 + xR) / 2 - HW / 2; xHr = xH + HW
  }
  const amtX = W - 58, shX = W - 4
  let ribs = '', lbls = '', nodes = ''
  const aria = (o, dr, share) => `${esc(o.name)}, ${usd0(o.amt)}, ${pct(share)} of the total${dr ? '. Open transactions' : ''}`
  let cum = 0
  for (const o of R) {
    const ya = barTop + cum; cum += o.t; const yc = rTop + o.top + o.s / 2, cl = o.color, dr = canDrill(o), share = o.amt / m.total
    ribs += `<path class="rib${dr ? ' click' : ''}" data-id="${o.id}" style="fill:${cl}" d="${rib(xHr, ya, ya + o.t, xR, yc - o.t / 2, yc + o.t / 2)}"/>`
    lbls += `<g class="flow${dr ? ' click' : ''}" data-id="${o.id}" tabindex="0" role="button" aria-label="${aria(o, dr, share)}">
      <rect class="hit" x="${xR - 4}" y="${f1(rTop + o.top - 3)}" width="${W - xR + 4}" height="${f1(o.s + 6)}"/>
      <rect x="${xR}" y="${f1(yc - o.t / 2)}" width="8" height="${f1(o.t)}" rx="1.5" style="fill:${cl}"/>
      <text class="nm" x="${xR + 16}" y="${f1(yc + 4.5)}">${esc(clip(o.name, 16))}</text>
      <text class="am" x="${amtX}" y="${f1(yc + 4.5)}" text-anchor="end">${usd0(o.amt)}</text>
      <text class="sh" x="${shX}" y="${f1(yc + 4.5)}" text-anchor="end">${pct(share)}</text></g>`
  }
  cum = 0
  L.forEach((o, i) => {
    const cl = o.color, dr = canDrill(o), share = o.amt / m.total
    if (mob) {
      const cx = W * (i + .5) / L.length, x0 = xH + cum; cum += o.t
      ribs += `<path class="rib${dr ? ' click' : ''}" data-id="${o.id}" style="fill:${cl}" d="${vrib(52, cx - o.t / 2, cx + o.t / 2, barTop, x0, x0 + o.t)}"/>`
      lbls += `<g class="flow${dr ? ' click' : ''}" data-id="${o.id}" tabindex="0" role="button" aria-label="${aria(o, dr, share)}">
        <rect class="hit" x="${f1(cx - W / L.length / 2 + 2)}" y="0" width="${f1(W / L.length - 4)}" height="60"/>
        <rect x="${f1(cx - Math.max(o.t, 4) / 2)}" y="46" width="${f1(Math.max(o.t, 4))}" height="6" rx="1.5" style="fill:${cl}"/>
        <text class="nm" x="${f1(cx)}" y="16" text-anchor="middle">${esc(clip(o.name, Math.max(6, Math.floor(W / L.length / 8))))}</text>
        <text class="am" x="${f1(cx)}" y="33" text-anchor="middle">${usd0(o.amt)}</text></g>`
    } else {
      const ya = barTop + cum; cum += o.t; const yc = lTop + o.top + o.s / 2
      ribs += `<path class="rib${dr ? ' click' : ''}" data-id="${o.id}" style="fill:${cl}" d="${rib(xL + 10, yc - o.t / 2, yc + o.t / 2, xH, ya, ya + o.t)}"/>`
      lbls += `<g class="flow${dr ? ' click' : ''}" data-id="${o.id}" tabindex="0" role="button" aria-label="${aria(o, dr, share)}">
        <rect class="hit" x="0" y="${f1(lTop + o.top - 3)}" width="${xL + 14}" height="${f1(o.s + 6)}"/>
        <rect x="${xL}" y="${f1(yc - o.t / 2)}" width="10" height="${f1(o.t)}" rx="1.5" style="fill:${cl}"/>
        <text class="nm" x="${xL - 10}" y="${f1(yc - 2)}" text-anchor="end">${esc(clip(o.name, 20))}</text>
        <text class="am" x="${xL - 10}" y="${f1(yc + 14)}" text-anchor="end">${usd0(o.amt)} <tspan class="sh">${pct(share)}</tspan></text></g>`
    }
  })
  if (mob) nodes = `<rect x="${xH}" y="${barTop}" width="64" height="${f1(Hb)}" rx="3" style="fill:var(--hh)"/><text class="hh-in" x="${xH + 32}" y="${barTop + 20}" text-anchor="middle">Household</text><text class="hh-in" x="${xH + 32}" y="${barTop + 37}" text-anchor="middle" style="font-weight:400">${usd0(m.total)}</text>`
  else nodes = `<rect x="${f1(xH)}" y="${f1(barTop)}" width="${HW}" height="${f1(Hb)}" rx="2" style="fill:var(--hh)"/><text class="hh-t" x="${f1(xH + HW / 2)}" y="${f1(barTop - 22)}" text-anchor="middle">Household</text><text class="hh-t2" x="${f1(xH + HW / 2)}" y="${f1(barTop - 8)}" text-anchor="middle">${usd0(m.total)}</text>`
  const clipR = mob ? `<rect id="rvr" x="0" y="0" width="${W}" height="${anim ? 0 : H}"/>` : `<rect id="rvr" x="0" y="0" width="${anim ? 0 : W}" height="${H}"/>`
  m.mob = mob; m.W = W; m.H = H; m.clamped = clamped
  return `<svg id="flowsvg" width="${W}" height="${f1(H)}" viewBox="0 0 ${W} ${f1(H)}" role="group" aria-label="Money flow diagram: income into Household, out to spending categories and Kept">
    <defs><clipPath id="rv">${clipR}</clipPath></defs>
    <g clip-path="url(#rv)">${ribs}${nodes}</g>${lbls}</svg>`
}
function readout(id) {
  if (!id) return 'Hover or tap a flow to trace it from source to spend.'
  const o = [...cur.src, ...cur.out].find(x => x.id === id); if (!o) return ''
  const m = cur, share = pct(o.amt / m.total)
  let txt
  if (o.kind === 'src') txt = o.id === 'src:savings' ? `Spending was ${usd0(o.amt)} more than income, so it came from savings.` : `${share} of money in.`
  else if (o.kind === 'kept') txt = `${share} of income, not spent.`
  else txt = `${share} of ${m.a.income >= m.a.expense ? 'income' : 'the total'}, ${pct(o.amt / m.a.expense)} of spending.`
  const hint = o.group ? ' <span class="muted">Click to show each one.</span>' : canDrill(o) ? (state.sel === id ? ` <button class="linkbtn" data-act="drill" data-id="${id}">Open transactions ${icon('arrow', 14)}</button>` : ' <span class="muted">Click to open transactions.</span>') : ''
  return `<span class="sw" style="background:${o.color}"></span><strong>${esc(o.name)} ${usd0(o.amt)}</strong><span>${txt}</span>${hint}`
}
function hl(id) {
  const box = $('#flowbox'); if (!box || !box.firstElementChild) return
  const svg = box.firstElementChild; svg.classList.toggle('hl', !!id)
  const isOut = id && !id.startsWith('src:')
  svg.querySelectorAll('[data-id]').forEach(e => {
    const d = e.dataset.id, on = d === id, rl = !!id && !on && (isOut ? d.startsWith('src:') : !d.startsWith('src:'))
    e.classList.toggle('on', on); e.classList.toggle('rel', rl)
  })
  $('#readout').innerHTML = readout(id)
}
function drill(id) {
  const o = [...cur.src, ...cur.out].find(x => x.id === id); if (!o?.f) return
  state.f = { ...blankF(), month: periodKey(), ...o.f }
  go('transactions')
}
function drawFlow() {
  const box = $('#flowbox'); if (!box || !cur) return
  const w = Math.floor(box.clientWidth) || 600
  const anim = !state.animated && !reduce; state.animated = true
  box.innerHTML = flowSVG(cur, Math.max(300, w), anim)
  $('#mincap').textContent = cur.clamped ? ' Flows too small to see are drawn at a minimum thickness.' : ''
  if (state.sel && !$(`[data-id="${state.sel}"]`)) state.sel = null
  hl(state.sel)
  if (anim) {
    const r = $('#rvr'), end = cur.mob ? cur.H : cur.W, t0 = performance.now(), dur = 1100
    const tick = t => { const p = Math.min(1, (t - t0) / dur), e = 1 - Math.pow(1 - p, 3); r.setAttribute(cur.mob ? 'height' : 'width', end * e); if (p < 1) requestAnimationFrame(tick) }
    requestAnimationFrame(tick)
  }
}
function flowEvents(box) {
  box.addEventListener('pointerdown', e => { state.lastPT = e.pointerType })
  box.addEventListener('pointerover', e => { if (e.pointerType === 'touch') return; const el = e.target.closest('[data-id]'); hl(el ? el.dataset.id : state.sel) })
  box.addEventListener('pointerleave', e => { if (e.pointerType !== 'touch') hl(state.sel) })
  box.addEventListener('click', e => {
    const el = e.target.closest('[data-id]'); if (!el) return; const id = el.dataset.id, o = [...cur.src, ...cur.out].find(x => x.id === id)
    if (o?.group) { state.flowAll = true; state.sel = null; render(); return }
    if (state.lastPT === 'touch' && state.sel !== id) { state.sel = id; hl(id); return }
    if (o && canDrill(o)) drill(id); else { state.sel = id; hl(id) }
  })
  box.addEventListener('focusin', e => { const el = e.target.closest('[data-id]'); if (el) hl(el.dataset.id) })
  box.addEventListener('focusout', () => hl(state.sel))
  box.addEventListener('keydown', e => {
    if (e.key !== 'Enter' && e.key !== ' ') return; const el = e.target.closest('[data-id]'); if (!el) return; e.preventDefault()
    const o = [...cur.src, ...cur.out].find(x => x.id === el.dataset.id)
    if (o?.group) { state.flowAll = true; state.sel = null; render(); return }
    o && canDrill(o) ? drill(o.id) : (state.sel = el.dataset.id, hl(state.sel))
  })
}

// Drag across a chart to zoom in: idx maps an x in the svg's own units to a data index, pick(i, j) gets the
// span. The click that ends a drag is swallowed, so a plain click still does what it did before.
function brush(svg, idx, pick) {
  let s = null, r = null
  const at = e => { const b = svg.getBoundingClientRect(); return (e.clientX - b.left) * svg.viewBox.baseVal.width / b.width }
  svg.addEventListener('pointerdown', e => { if (!e.button) { s = at(e); r = null } })
  svg.addEventListener('pointermove', e => {
    if (s == null || (!r && Math.abs(at(e) - s) < 8)) return
    if (!r) { r = document.createElementNS('http://www.w3.org/2000/svg', 'rect'); r.setAttribute('class', 'brush'); r.setAttribute('height', svg.viewBox.baseVal.height); svg.append(r); svg.setPointerCapture(e.pointerId) }
    r.setAttribute('x', Math.min(s, at(e))); r.setAttribute('width', Math.abs(at(e) - s))
  })
  svg.addEventListener('pointerup', e => {
    if (r) {
      const i = idx(s), j = idx(at(e)), stop = ev => ev.stopPropagation()
      addEventListener('click', stop, true); setTimeout(() => { removeEventListener('click', stop, true); pick(Math.min(i, j), Math.max(i, j)) })
    }
    s = null
  })
  svg.addEventListener('pointercancel', () => { r?.remove(); s = r = null })
}

/* net worth chart */
function drawNW() {
  const btn = $('#nwc'); if (!btn) return
  const w = Math.max(200, Math.floor(btn.clientWidth) || 300), pts = nwPoints()
  const H = 176, Lm = 48, Rm = 8, T = 8, B = 24, pw = w - Lm - Rm, ph = H - T - B
  const vals = pts.map(p => p.total / 100); let lo = Math.min(...vals), hi = Math.max(...vals); const pad = (hi - lo) * .12 || 1; lo -= pad; hi += pad
  const stepv = niceStep(hi - lo, 4)
  const x = i => Lm + pw * i / (pts.length - 1), y = v => T + ph - (v - lo) / (hi - lo) * ph
  let g = ''; for (let v = Math.ceil(lo / stepv) * stepv; v <= hi; v += stepv) g += `<line class="gridline" x1="${Lm}" x2="${w - Rm}" y1="${f1(y(v))}" y2="${f1(y(v))}"/><text x="${Lm - 8}" y="${f1(y(v) + 4)}" text-anchor="end">${axis(v)}</text>`
  // Label the first point and each month start, thinned so labels never collide.
  let xl = '', lastX = -99
  pts.forEach((p, i) => { if ((i === 0 || p.date.endsWith('-01')) && x(i) - lastX > 48) { lastX = x(i); xl += `<text x="${f1(x(i))}" y="${H - 6}" text-anchor="${i === 0 ? 'start' : 'middle'}">${dShort(p.date)}</text>` } })
  const ms = periodMonths(), fst = ms[0] + '-01', lst = ms.at(-1) + '-31'
  const idx = pts.map((p, i) => p.date >= fst && p.date <= lst ? i : -1).filter(i => i >= 0)
  const shade = idx.length > 1 && state.mode !== 'range' ? `<rect x="${f1(x(idx[0]))}" y="${T}" width="${f1(x(idx.at(-1)) - x(idx[0]))}" height="${ph}" style="fill:var(--accent);opacity:.09"/>` : ''
  const d = 'M' + pts.map((p, i) => `${f1(x(i))},${f1(y(p.total / 100))}`).join('L')
  btn.innerHTML = `<svg id="nwsvg" width="${w}" height="${H}" viewBox="0 0 ${w} ${H}" role="img" aria-label="Net worth from ${dShort(pts[0].date)} to ${dShort(pts.at(-1).date)}">${g}${shade}<path d="${d}" fill="none" style="stroke:var(--income)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/><circle cx="${f1(x(0))}" cy="${f1(y(pts[0].total / 100))}" r="3" style="fill:var(--panel);stroke:var(--income)" stroke-width="2"/>${xl}<g id="nwx" style="display:none"><line y1="${T}" y2="${T + ph}" style="stroke:var(--muted)" stroke-dasharray="3 3"/><circle r="4" style="fill:var(--income);stroke:var(--panel)" stroke-width="2"/></g></svg>`
  const svg = btn.firstElementChild, cx = $('#nwx')
  brush(svg, v => Math.max(0, Math.min(pts.length - 1, Math.round((v - Lm) / pw * (pts.length - 1)))), (i, j) => zoom(pts[i].date.slice(0, 7), pts[j].date.slice(0, 7)))
  const def = { v: $('#nwval').textContent, l: $('#nwlbl').textContent, s: $('#nwad').innerHTML }
  btn.onpointermove = e => {
    const r = svg.getBoundingClientRect(), i = Math.max(0, Math.min(pts.length - 1, Math.round((e.clientX - r.left - Lm) / pw * (pts.length - 1))))
    cx.style.display = ''; const ln = cx.querySelector('line'); ln.setAttribute('x1', x(i)); ln.setAttribute('x2', x(i))
    const c = cx.querySelector('circle'); c.setAttribute('cx', x(i)); c.setAttribute('cy', y(pts[i].total / 100))
    $('#nwval').textContent = usd0(pts[i].total); $('#nwlbl').textContent = dMed(pts[i].date); $('#nwad').innerHTML = nwSplit(pts[i])
  }
  btn.onpointerleave = () => { cx.style.display = 'none'; $('#nwval').textContent = def.v; $('#nwlbl').textContent = def.l; $('#nwad').innerHTML = def.s }
}

/* cash flow chart */
function drawCF() {
  const wrap = $('#cfwrap'); if (!wrap) return
  const w = Math.max(240, Math.floor(wrap.clientWidth) || 600)
  const ser = windowMonths().map(m => ({ m, ...monthOf(m) }))
  const H = 250, Lm = 50, Rm = 4, T = 10, B = 44, pw = w - Lm - Rm, ph = H - T - B
  const max = Math.max(...ser.flatMap(s => [s.income, s.expense]), 0) / 100
  const stepv = niceStep(max || 1000, 5), top = Math.ceil((max || 1000) / stepv) * stepv
  const y = v => T + ph - (v / 100 / top) * ph, gw = pw / ser.length, bw = Math.max(5, Math.min(16, gw * .3)), sel = new Set(periodMonths()), narrow = w < 520
  let g = ''; for (let v = 0; v <= top; v += stepv) g += `<line class="gridline" x1="${Lm}" x2="${w - Rm}" y1="${f1(y(v * 100))}" y2="${f1(y(v * 100))}"/><text x="${Lm - 8}" y="${f1(y(v * 100) + 4)}" text-anchor="end">${axis(v)}</text>`
  const bar = (x0, wd, h, yy, cls) => { if (h <= 0) return ''; const r = Math.min(4, wd / 2, h); return `<path class="bar ${cls}" d="M${f1(x0)},${f1(yy + h)}V${f1(yy + r)}Q${f1(x0)},${f1(yy)} ${f1(x0 + r)},${f1(yy)}H${f1(x0 + wd - r)}Q${f1(x0 + wd)},${f1(yy)} ${f1(x0 + wd)},${f1(yy + r)}V${f1(yy + h)}Z"/>` }
  let mo = ''
  ser.forEach((s, i) => {
    const cx = Lm + gw * (i + .5), xi = cx - bw - 1, xo = cx + 1, yi = y(s.income), yo = y(s.expense), base = y(0)
    const lab = narrow ? mShort(s.m)[0] : mShort(s.m), yr = (i === 0 || s.m.endsWith('-01')) ? `<text x="${f1(cx)}" y="${H - 6}" text-anchor="middle">${s.m.slice(0, 4)}</text>` : ''
    mo += `<g class="mo${sel.has(s.m) ? ' sel' : ''}" data-m="${s.m}" tabindex="0" role="button" aria-label="${mName(s.m)}: income ${usd0(s.income)}, spending ${usd0(s.expense)}. Open transactions">
      <rect class="band" x="${f1(cx - gw / 2 + 1)}" y="${T}" width="${f1(gw - 2)}" height="${ph + B - 8}" rx="6"/>
      ${bar(xi, bw, base - yi, yi, 'i')}${bar(xo, bw, base - yo, yo, 'o')}
      <text x="${f1(cx)}" y="${T + ph + 18}" text-anchor="middle">${lab}</text>${yr}
      <rect class="hit" x="${f1(cx - gw / 2)}" y="${T}" width="${f1(gw)}" height="${H - T}"/></g>`
  })
  wrap.innerHTML = `<svg class="cfsvg hasSel" width="${w}" height="${H}" viewBox="0 0 ${w} ${H}" role="group" aria-label="Monthly income and spending, ${ser.length} months">${g}${mo}</svg><div class="tip" id="tip"></div>`
  brush(wrap.firstElementChild, v => Math.max(0, Math.min(ser.length - 1, Math.floor((v - Lm) / gw))), (i, j) => zoom(ser[i].m, ser[j].m))
  const tip = $('#tip')
  const show = e => {
    const gEl = e.target.closest('.mo'); if (!gEl) { tip.classList.remove('on'); return }
    const s = ser.find(z => z.m === gEl.dataset.m), net = s.income - s.expense
    tip.innerHTML = `<b>${mName(s.m)}${s.m === CUR ? ' (so far)' : ''}</b><br>In ${usd0(s.income)}<br>Out ${usd0(s.expense)}<br>Net ${net < 0 ? '−' : '+'}${usd0(Math.abs(net))}`
    const r = gEl.getBoundingClientRect(), wr = wrap.getBoundingClientRect(); let l = r.left - wr.left + r.width / 2 - tip.offsetWidth / 2; l = Math.max(0, Math.min(wr.width - tip.offsetWidth, l))
    tip.style.left = l + 'px'; tip.style.top = Math.max(0, r.top - wr.top + 8) + 'px'; tip.classList.add('on')
  }
  wrap.addEventListener('pointerover', e => { if (e.pointerType !== 'touch') show(e) })
  wrap.addEventListener('pointerleave', () => tip.classList.remove('on'))
  wrap.addEventListener('focusin', show); wrap.addEventListener('focusout', () => tip.classList.remove('on'))
}

/* ---------- Transactions ---------- */
const T = { rows: [], total: 0, busy: false, err: '', seq: 0 }
function txQuery() {
  const F = state.f, p = new URLSearchParams()
  if (F.month !== 'all') p.set('month', F.month)
  if (F.cat !== 'all') p.set('category', F.cat === '__none' ? '' : F.cat)
  if (F.acct !== 'all') p.set('account_id', F.acct)
  if (F.q.trim()) p.set('q', F.q.trim())
  if (F.flow) p.set('flow', F.flow)
  return p
}
// mode: 'reset' (new filters), 'more' (append the next page) or 'keep' (reload what is shown after an edit)
async function fetchTx(mode) {
  const seq = ++T.seq, keep = mode === 'keep'
  T.busy = true; T.err = ''
  if (mode === 'reset') { T.rows = []; T.total = 0 }
  paintTx()
  const p = txQuery(); p.set('limit', keep ? Math.min(1000, Math.max(T.rows.length, PAGE)) : PAGE); p.set('offset', mode === 'more' ? T.rows.length : 0)
  try {
    const r = await api('/transactions?' + p, null, true)
    if (seq !== T.seq) return
    T.rows = mode === 'more' ? T.rows.concat(r.data) : r.data; T.total = r.total
  } catch (e) { if (seq !== T.seq) return; T.err = e.message }
  T.busy = false; paintTx()
}
const filtersActive = () => { const F = state.f; return F.month !== 'all' || F.cat !== 'all' || F.acct !== 'all' || F.q || F.flow }
function paintTx() {
  const list = $('#txlist'); if (!list) return
  list.innerHTML = txListHTML()
  $('#txcount').textContent = T.busy && !T.rows.length ? '' : T.err ? '' : `${T.total} transaction${T.total === 1 ? '' : 's'}`
  $('#clrslot').innerHTML = clearHTML()
  const m = $('#txmsg'); m.textContent = state.txMsg?.t || ''; m.className = 'cap' + (state.txMsg?.err ? ' warn' : ''); m.hidden = !state.txMsg
}
const clearHTML = () => filtersActive() ? `<button class="btn ghost clr" data-act="txclear">${icon('x', 16)}Clear</button>` : ''
const cashAcct = t => t.account_type === 'depository' || t.account_type === 'credit'
function catCell(t) {
  if (t.transfer) return '<span class="dot" style="background:var(--cat-transfer)"></span>Transfer'
  if (!cashAcct(t)) return '<span class="dot" style="background:var(--cat-transfer)"></span>Not in cash flow'
  if (state.editing === t.id) return editorHTML(t)
  const label = catName(t.category)
  if (t.category === '') return `<button class="uncat" data-act="pick" data-id="${esc(t.id)}" aria-label="Uncategorized, choose a category for ${esc(t.merchant || t.name)}">Uncategorized ${icon('chevD', 14)}</button>`
  return `<button class="catbtn" data-act="pick" data-id="${esc(t.id)}" aria-label="${esc(label)}, change category for ${esc(t.merchant || t.name)}"><span class="dot" style="background:${catColor(t.category)}"></span>${esc(label)}</button>`
}
function editorHTML(t) {
  const names = D.cats.includes(t.category) || t.category === '' ? D.cats : [...D.cats, t.category]
  const pattern = (t.merchant || t.name).trim().slice(0, 60)
  if (state.editNew) return `<div class="editor"><input class="fsel newcat" id="newcat" maxlength="60" placeholder="New category name" autocomplete="off" aria-label="New category name for ${esc(t.merchant || t.name)}" data-id="${esc(t.id)}">
    <button class="btn sm" data-act="newsave" data-id="${esc(t.id)}">Save</button>
    <label class="always"><input type="checkbox" id="always"> Always use this for ${esc(pattern)}</label>
    <button class="linkbtn" data-act="pickcancel">Cancel</button></div>`
  return `<div class="editor"><select class="pick" data-id="${esc(t.id)}" aria-label="Choose a category for ${esc(t.merchant || t.name)}">
    ${t.category === '' ? '<option value="" selected>Choose category</option>' : ''}${t.manual ? '<option value="__auto">Automatic (clear my choice)</option>' : ''}
    ${names.map(c => `<option value="${esc(c)}" ${c === t.category ? 'selected' : ''}>${esc(catName(c))}</option>`).join('')}<option value="__new">New category…</option></select>
    <label class="always"><input type="checkbox" id="always"> Always use this for ${esc(pattern)}</label>
    <button class="linkbtn" data-act="pickcancel">Cancel</button></div>`
}
function txRow(t) {
  const a = acct[t.account_id], where = (t.institution || t.account) + (a?.mask ? ' ••' + a.mask : '')
  const tags = (t.pending ? '<span class="tag">Pending</span>' : '') + (t.transfer ? '<span class="tag">Transfer</span>' : '') + (!t.transfer && !cashAcct(t) ? `<span class="tag">${t.account_type === 'loan' ? 'Loan' : 'Investment'} account</span>` : '')
  const amt = (t.amount > 0 ? '+' : '') + usd2(t.amount)
  return `<div class="row${t.transfer || !cashAcct(t) ? ' tr' : ''}${t.pending ? ' pe' : ''}"><div class="m"><span class="t">${esc(t.merchant || t.name)}</span>${tags}</div>
    <div class="sub2"><span class="a">${esc(where)}</span><span class="c">${catCell(t)}</span></div>
    <div class="amt${t.amount > 0 && !t.transfer && cashAcct(t) ? ' in' : ''}">${amt}</div></div>`
}
function txListHTML() {
  if (T.err) return `<div class="empty"><p><strong>Could not load transactions</strong></p><p>${esc(T.err)}</p><p><button class="btn ghost" data-act="txretry">Retry</button></p></div>`
  if (!T.rows.length) {
    if (T.busy) return '<div class="empty" role="status"><p class="muted">Loading…</p></div>'
    if (!filtersActive()) return `<div class="empty"><p><strong>No transactions yet</strong></p><p>They appear after the first sync. Configure SimpleFIN in Settings.</p></div>`
    return `<div class="empty"><p><strong>No transactions match</strong></p><p>Try a different month, category or search.</p><p><button class="btn ghost" data-act="txclear">Clear filters</button></p></div>`
  }
  let out = '', day = ''
  for (const t of T.rows) { if (t.date !== day) { day = t.date; out += `<div class="day">${dLong(day)}</div>` } out += txRow(t) }
  if (T.rows.length < T.total) out += `<div class="morebar"><button class="btn ghost" data-act="more" ${T.busy ? 'disabled' : ''}>Show ${Math.min(PAGE, T.total - T.rows.length)} more</button> <span class="muted" style="margin-left:8px">${T.rows.length} of ${T.total}</span></div>`
  return out
}
function transactions() {
  const F = state.f
  const catOpts = [...D.cats]; if (F.cat !== 'all' && F.cat !== '__none' && !catOpts.includes(F.cat)) catOpts.push(F.cat)
  const flowTag = F.flow ? `<button class="btn ghost sm flowtag" data-act="unflow">${F.flow === 'in' ? 'Money in only' : 'Money out only'} ${icon('x', 14)}</button>` : ''
  return `${banner()}<div class="pagehead"><h1>Transactions</h1><span class="muted" id="txcount"></span><a class="btn ghost sm" href="#alerts" style="margin-left:auto">Alerts${openAl().length ? ` (${openAl().length})` : ''}</a><a class="btn ghost sm" href="#review">Review by merchant ${icon('arrow', 14)}</a></div>
  <div class="filters">
   <select data-f="month" aria-label="Month"><option value="all">All months</option>${F.month.includes('..') ? `<option value="${F.month}" selected>${esc(rangeLabel(monthsBetween(...F.month.split('..'))))}</option>` : ''}${YEARS.map(y => `<option value="${y}" ${F.month === String(y) ? 'selected' : ''}>All of ${y}</option>`).join('')}${[...MONTHS].reverse().map(m => `<option value="${m}" ${F.month === m ? 'selected' : ''}>${mName(m)}</option>`).join('')}</select>
   <select data-f="cat" aria-label="Category"><option value="all">All categories</option><option value="__none" ${F.cat === '__none' ? 'selected' : ''}>Uncategorized</option>${catOpts.map(c => `<option value="${esc(c)}" ${F.cat === c ? 'selected' : ''}>${esc(catName(c))}</option>`).join('')}</select>
   <select data-f="acct" aria-label="Account"><option value="all">All accounts</option>${D.accounts.map(a => `<option value="${esc(a.id)}" ${F.acct === a.id ? 'selected' : ''}>${esc(a.institution)} ${esc(a.name)}</option>`).join('')}</select>
   <div class="sbox">${icon('search', 18)}<input type="search" id="q" placeholder="Search merchants" aria-label="Search transactions" value="${esc(F.q)}"></div>
   <span id="clrslot"></span>
  </div>
  <p class="notecap">Transfers and activity inside investment and loan accounts are muted and never count as income or spending. Pending items are marked and excluded from cash flow. Filtering by category or money in/out leaves both out.${flowTag}</p>
  ${F.cat === '__none' ? `<p class="notecap rvhint">Faster: <a href="#review">review these by merchant</a> and categorize a whole merchant at once.</p>` : ''}
  <p id="txmsg" role="status" hidden></p>
  <div class="list" id="txlist"></div>`
}
async function applyCat(id, val, always) {
  const t = T.rows.find(r => r.id === id); if (!t) return
  const pattern = (t.merchant || t.name).trim().slice(0, 60)
  state.txMsg = null
  try {
    await send('PATCH', '/transactions/' + encodeURIComponent(id), { category: val === '__auto' ? '' : val })
  } catch (e) { state.txMsg = { t: `Could not save the category: ${e.message}`, err: true }; paintTx(); return }
  if (always && val !== '__auto') {
    try { await send('POST', '/rules', { pattern, category: val }); D.rules = null; state.txMsg = { t: `Saved. New “${pattern}” transactions will be ${catName(val)}.` } }
    catch (e) { state.txMsg = { t: e.status === 409 ? `Saved for this transaction. A rule for “${pattern}” already exists; change it under Settings, Rules.` : `Saved for this transaction, but the rule failed: ${e.message}`, err: true } }
  }
  state.editing = null
  cache.clear(); state.perr = {}
  api('/categories').then(c => { D.cats = c }).catch(() => {})
  fetchTx('keep')
}

/* ---------- Accounts ---------- */
const TYPE_GROUPS = { depository: 'Cash', credit: 'Credit', investment: 'Investments', property: 'Property', loan: 'Loans', other: 'Other accounts' }
const GROUP_ORDER = ['Cash', 'Credit', 'Investments', 'Property', 'Loans', 'Other accounts']
// SimpleFIN never says what an account is: the server guesses (type_source 'guess') and the user can override from this list.
const TYPE_LABELS = { depository: 'Cash', credit: 'Credit card', investment: 'Investment', property: 'Property', loan: 'Loan', other: 'Other' }
const groupOf = a => TYPE_GROUPS[a.type] || 'Other accounts'
const typeOptions = a => `<option value="" ${a.type_source === 'user' ? '' : 'selected'}>Automatic (${TYPE_LABELS[a.type] || 'Other'})</option>` + Object.entries(TYPE_LABELS).map(([v, l]) => `<option value="${v}" ${a.type_source === 'user' && a.type === v ? 'selected' : ''}>${l}</option>`).join('')
function accounts() {
  const total = D.accounts.reduce((s, a) => s + (a.current ?? 0), 0)
  const errIds = new Set(errItems().map(i => i.id))
  let html = `${banner()}<div class="pagehead"><h1>Accounts</h1></div>
  <section class="panel"><h2>Net worth</h2><div class="nwbig">${D.accounts.length ? usd0(total) : '—'}</div><p class="cap">The sum of every balance. Loans and credit cards count as negative. Balances are as of the last sync.</p></section>`
  if (!D.accounts.length) html += '<section class="panel acctgroup"><div class="empty"><p><strong>No accounts yet</strong></p><p>Configure SimpleFIN in Settings and they will appear here.</p></div></section>'
  for (const label of GROUP_ORDER) {
    const list = D.accounts.filter(a => groupOf(a) === label); if (!list.length) continue
    const groupSum = list.reduce((s, a) => s + (a.current ?? 0), 0) // privacy mode shows each account's share of it
    html += `<section class="panel acctgroup" style="padding-left:0;padding-right:0"><div class="gh"><h2>${label}</h2><b>${usd0(list.reduce((s, a) => s + (a.current ?? 0), 0))}</b></div>`
    for (const a of list) {
      const open = state.open === a.id, det = acctTx.get(a.id)
      html += `<div class="arow${open ? ' open' : ''}"><button data-act="acct" data-id="${esc(a.id)}" aria-expanded="${open}"><span><span class="n">${esc(a.institution)} ${esc(a.name)}</span><br><span class="s">${a.mask ? '••' + esc(a.mask) : ''}${a.type_confident ? '' : ' <span class="tag">Check type</span>'}${errIds.has(a.item_id) ? ` <span class="warn">${icon('alert', 14)}Needs attention</span>` : ''}</span></span><span class="b">${a.current == null ? '—' : privacy.on && groupSum ? `${pct(Math.abs(a.current / groupSum))}<small class="muted" style="font-weight:400;font-size:12px"> of ${label.toLowerCase()}</small>` : usd0(a.current)}</span><span class="ch">${icon('chevR', 16)}</span></button>`
      if (open) {
        const rows = !det ? '<p class="muted" style="padding:10px 0" role="status">Loading…</p>' : det.err ? `<p class="warn" style="padding:10px 0">${esc(det.err)}</p>` : det.rows.length ? det.rows.map(t => `<div class="row${t.transfer ? ' tr' : ''}${t.pending ? ' pe' : ''}"><div class="m"><span class="t">${esc(t.merchant || t.name)}</span>${t.pending ? '<span class="tag">Pending</span>' : ''}${t.transfer ? '<span class="tag">Transfer</span>' : ''}</div><div class="amt${t.amount > 0 && !t.transfer ? ' in' : ''}">${t.amount > 0 ? '+' : ''}${usd2(t.amount)}</div></div>`).join('') : '<p class="muted" style="padding:10px 0">No transactions for this account.</p>'
        html += `<div class="adet"><label style="display:flex;align-items:center;gap:10px;font-size:13px;padding:10px 0 2px">Account type <select class="typepick field" style="width:auto" data-id="${esc(a.id)}">${typeOptions(a)}</select></label>${state.typeErr && state.typeErr.id === a.id ? `<p class="warn" role="alert">${esc(state.typeErr.msg)}</p>` : ''}<p class="muted" style="font-size:13px;padding:10px 0 2px">Recent transactions</p>${rows}
        <div class="foot"><button class="linkbtn" data-act="acctTx" data-id="${esc(a.id)}">All transactions for this account ${icon('arrow', 14)}</button></div></div>`
      }
      html += '</div>'
    }
    html += '</section>'
  }
  return html
}
async function setAccountType(id, type) {
  state.typeErr = null
  try { await send('PATCH', '/accounts/' + encodeURIComponent(id), { type }); D.accounts = await api('/accounts'); derive() } catch (e) { state.typeErr = { id, msg: e.message } }
  render()
}
async function openAccount(id) {
  state.open = state.open === id ? null : id
  render()
  if (state.open === id && !acctTx.has(id)) {
    try { acctTx.set(id, { rows: (await api(`/transactions?account_id=${encodeURIComponent(id)}&limit=6`)) }) } catch (e) { acctTx.set(id, { err: e.message }) }
    if (state.view === 'accounts') render()
  }
}

/* ---------- Spending ---------- */
// By category or by merchant ('c:' / 'm:' cache keys), for the selected month, year or zoomed range. A single month
// also loads the 6 months before it for the average tick.
const spendPrior = () => state.mode === 'month' ? MONTHS.filter(k => k < state.month).slice(-6) : []
function spending() {
  const key = periodKey(), byM = state.spendBy === 'merchant', p = byM ? 'm:' : 'c:', ek = 's' + p + key, err = state.perr[ek]
  const prior = state.mode === 'month' ? MONTHS.filter(k => k < state.month).slice(-6) : [], head = `${banner()}${navHTML(true)}`
  if (![key, ...prior].every(k => cache.has(p + k))) {
    if (!err && !inflight.has(ek)) {
      inflight.add(ek)
      Promise.all([key, ...prior].map(k => load(p + k, `/summary/categories?month=${k}${byM ? '&by=merchant' : ''}`)))
        .catch(e => { state.perr[ek] = e.message }).finally(() => { inflight.delete(ek); if (state.view === 'spending' && periodKey() === key) render() })
    }
    return `${head}<section class="panel" style="margin-top:16px">${err ? `<div class="empty"><p><strong>Could not load spending</strong></p><p>${esc(err)}</p><p><button class="btn ghost" data-act="retryspend">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'}</section>`
  }
  const field = byM ? 'merchant' : 'category', ms = periodMonths(), n = ms.length
  let rows = cache.get(p + key).map(r => ({ raw: r[field], name: byM ? r.merchant : catName(r.category), v: r.amount, color: byM ? 'var(--spend)' : catColor(r.category), f: byM ? { flow: 'out', q: r.merchant } : { cat: r.category === '' ? '__none' : r.category } }))
  if (byM && rows.length > 31) rows = [...rows.slice(0, 30), { name: `${rows.length - 30} more merchants`, v: rows.slice(30).reduce((s, r) => s + r.v, 0), color: 'var(--cat-other)', f: { flow: 'out' } }]
  spendRows = rows
  const total = rows.reduce((s, r) => s + r.v, 0)
  const pa = prior.map(k => Object.fromEntries(cache.get(p + k).map(r => [r[field], r.amount])))
  const avg = r => pa.length && r.raw != null ? pa.reduce((s, x) => s + (x[r.raw] || 0), 0) / pa.length : null
  const mx = Math.max(...rows.map(r => r.v), ...rows.map(r => avg(r) || 0), 1)
  const label = state.mode === 'month' ? mName(state.month) : state.mode === 'year' ? String(state.year) : rangeLabel(ms)
  const note = state.mode === 'month' ? `The tick on each bar marks the average of ${prior.length ? `the ${prior.length} prior month${prior.length === 1 ? '' : 's'}` : 'prior months (none yet)'}.` : `${n} month${n === 1 ? '' : 's'}; the right column is the monthly average.`
  return `${head}
  <p class="sub">${ms.includes(CUR) ? `${mShort(CUR)} is month to date${CUR === NOW_M ? `, through ${dShort(today())}` : ''}. Totals will grow. ` : ''}Counts checking, savings and credit cards; excludes transfers, investment and loan accounts, and pending transactions. ${note}</p>
  <div class="frow"><div class="seg" role="group" aria-label="Group spending by"><button data-act="spendby" data-v="category" aria-pressed="${!byM}">Category</button><button data-act="spendby" data-v="merchant" aria-pressed="${byM}">Merchant</button></div>
   <button class="btn ghost sm" data-act="spendcsv" style="margin-left:auto" ${rows.length ? '' : 'disabled'}>Export CSV</button></div>
  <section class="panel" style="padding-left:0;padding-right:0"><div class="gh" style="padding-top:0"><h2>Spent in ${esc(label)}</h2><b>${usd0(total)}</b></div>
   ${rows.length ? rows.map((r, i) => {
    const av = avg(r), d = av == null ? null : r.v - av
    return `<button class="srow" data-act="spendcat" data-i="${i}"><span class="nm"><span class="dot" style="background:${r.color}"></span>${esc(r.name)}</span>
      <span class="track"><span class="fill" style="width:${f1(r.v / mx * 100)}%;background:${r.color}"></span>${av != null ? `<span class="avg" style="left:calc(${f1(av / mx * 100)}% - 1px)"></span>` : ''}</span>
      <span class="v">${usd0(r.v)}</span><span class="p">${pct(r.v / total)}</span>
      <span class="d">${n > 1 ? `${usd0(Math.round(r.v / n))} a month` : d == null ? (r.raw == null ? '' : 'No prior months') : `${usd0(Math.abs(d))} ${d >= 0 ? 'above' : 'below'} avg of ${usd0(av)}`}</span></button>`
  }).join('') : `<div class="empty">No spending in ${esc(label)}.</div>`}
  </section>
  <p class="need">Subscriptions and bills that repeat on a schedule. <button class="linkbtn" data-act="go" data-v="recurring">Recurring charges ${icon('arrow', 14)}</button></p>
  <p class="need">Income and spending by month, side by side, for the 12 months ending in ${mName(state.month)}. <button class="linkbtn" data-act="go" data-v="report">Income v Expense ${icon('arrow', 14)}</button></p>`
}
// Rows of cells (strings or cents) as a CSV download; cents become plain dollars so a spreadsheet can add them up.
function downloadCSV(name, rows) {
  const cell = v => { if (typeof v === 'number') return (v / 100).toFixed(2); if (/^[=+\-@]/.test(v)) v = "'" + v; return /[",\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v } // a leading ' keeps a merchant name from running as a spreadsheet formula
  const a = document.createElement('a')
  a.href = URL.createObjectURL(new Blob([rows.map(r => r.map(cell).join(',')).join('\n') + '\n'], { type: 'text/csv' }))
  a.download = name; a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 1000)
}
let spendRows = []

/* ---------- Income v Expense (#report): the 12 months ending at state.month, one row per income source and category ---------- */
let repCells = [], repCSV = []
function report() {
  const m = state.month, ms = MONTHS.filter(k => k <= m).slice(-12), key = 'r' + m, err = state.perr[key]
  const head = `${banner()}${navHTML(false)}`
  if (!ms.every(k => cache.has('c:' + k) && cache.has('i:' + k))) {
    if (!err && !inflight.has(key)) {
      inflight.add(key)
      Promise.all(ms.flatMap(k => [load('c:' + k, '/summary/categories?month=' + k), load('i:' + k, '/summary/income?month=' + k)]))
        .catch(e => { state.perr[key] = e.message }).finally(() => { inflight.delete(key); if (state.view === 'report' && state.month === m) render() })
    }
    return `${head}<section class="panel" style="margin-top:16px">${err ? `<div class="empty"><p><strong>Could not load the report</strong></p><p>${esc(err)}</p><p><button class="btn ghost" data-act="retryreport">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'}</section>`
  }
  // rows: { label, f (transactions filter, null for a total), v: amount per month in ms }
  const pivot = (p, name, f) => {
    const by = new Map()
    ms.forEach((k, i) => cache.get(p + k).forEach(r => { const n = name(r); if (!by.has(n)) by.set(n, { label: n, f: f(r), v: ms.map(() => 0) }); by.get(n).v[i] += r.amount }))
    return [...by.values()].sort((a, b) => sum(b.v) - sum(a.v))
  }
  const sum = v => v.reduce((s, x) => s + x, 0), add = rows => ms.map((_, i) => rows.reduce((s, r) => s + r.v[i], 0))
  let inc = pivot('i:', r => r.source, r => ({ flow: 'in', q: r.source }))
  // ponytail: refunds and one-off deposits show up as sources; fold everything past the top 10 into one row
  if (inc.length > 11) inc = [...inc.slice(0, 10), { label: `${inc.length - 10} other sources`, f: { flow: 'in' }, v: add(inc.slice(10)) }]
  const exp = pivot('c:', r => catName(r.category), r => ({ cat: r.category === '' ? '__none' : r.category }))
  const incT = add(inc), expT = add(exp), net = incT.map((x, i) => x - expT[i])
  repCells = []
  const cell = (v, f, month) => { if (!v || !f) return `<td>${v ? usd0(v) : '<span class="muted">—</span>'}</td>`; repCells.push({ ...f, month }); return `<td><button class="cellbtn" data-act="repcell" data-i="${repCells.length - 1}">${usd0(v)}</button></td>` }
  const row = (r, cls = '') => `<tr class="${cls}"><th scope="row">${esc(r.label)}</th>${r.v.map((v, i) => cell(v, r.f, ms[i])).join('')}<td>${usd0(Math.round(sum(r.v) / ms.length))}</td>${cell(sum(r.v), r.f, `${ms[0]}..${ms.at(-1)}`)}</tr>`
  const section = (title, rows, total) => `<tr class="rsec"><th scope="rowgroup" colspan="${ms.length + 3}">${title}</th></tr>${rows.map(r => row(r)).join('')}${row({ label: `Total ${title.toLowerCase()}`, v: total }, 'rtot')}`
  const line = r => [r.label, ...r.v, Math.round(sum(r.v) / ms.length), sum(r.v)]
  repCSV = [['', ...ms, 'Average', 'Total'], ['Income'], ...inc.map(line), line({ label: 'Total income', v: incT }), ['Expenses'], ...exp.map(line), line({ label: 'Total expenses', v: expT }), line({ label: 'Net income', v: net })]
  return `${head}
  <div class="frow"><button class="btn ghost sm" data-act="repcsv" style="margin-left:auto">Export CSV</button></div>
  <p class="sub">${esc(rangeLabel(ms))}${ms.includes(CUR) ? ` (${mShort(CUR)} is month to date)` : ''}. Same accounts as Spending: transfers, investment and loan accounts and pending transactions are left out. Click an amount to see its transactions. <a href="#spending">Back to Spending</a></p>
  <section class="panel"><div class="tblwrap"><table class="invtbl reptbl"><thead><tr><th></th>${ms.map(k => `<th scope="col">${mShort(k)}${k.endsWith('-01') || k === ms[0] ? ` <small>${k.slice(0, 4)}</small>` : ''}</th>`).join('')}<th scope="col">Average</th><th scope="col">Total</th></tr></thead>
   <tbody>${section('Income', inc, incT)}${section('Expenses', exp, expT)}</tbody>
   <tfoot>${row({ label: 'Net income', v: net }, 'rtot')}</tfoot></table></div></section>`
}

/* ---------- Recurring ---------- */
function recurring() {
  const head = `${banner()}<div class="pagehead"><h1>Recurring charges</h1></div>`, err = state.perr.rec
  if (D.recurring === null) {
    if (!err && !inflight.has('rec')) {
      inflight.add('rec')
      api('/recurring').then(v => { D.recurring = v }, e => { state.perr.rec = e.message }).finally(() => { inflight.delete('rec'); if (state.view === 'recurring') render() })
    }
    return `${head}<section class="panel">${err ? `<div class="empty"><p><strong>Could not load recurring charges</strong></p><p>${esc(err)}</p><p><button class="btn ghost" data-act="retryrec">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'}</section>`
  }
  const R = D.recurring, now = today(), days = d => Math.round((Date.parse(d) - Date.parse(now)) / 864e5)
  // A cancelled item that billed again goes back to its group, flagged, until it's marked again or cleared.
  const live = R.filter(r => r.active && (!r.mark || r.charged_after_cancel))
  const soon = live.filter(r => r.kind !== 'income' && days(r.next) <= 7).sort((a, b) => a.next.localeCompare(b.next))
  const hidden = R.filter(r => r.mark === 'hidden')
  const row = r => {
    const i = R.indexOf(r), d = days(r.next), inc = r.kind === 'income'
    const tags = [r.new && 'New', r.previous != null && `Up from ${usd2(r.previous)}`, r.cadence === 'yearly' && r.active && !r.mark && d >= 0 && d <= 30 && `Renews in ${d} day${d === 1 ? '' : 's'}`]
      .filter(Boolean).map(t => ` <span class="tag">${t}</span>`).join('') + (r.charged_after_cancel ? ' <span class="tag warn">Charged after you cancelled</span>' : '')
    const what = r.kind === 'bill' ? `usually ${usd2(r.typical)}${Math.round(r.high / 100) > Math.round(r.low / 100) ? `, ${usd0(r.low)} to ${usd0(r.high)} over the last year` : ''}` : inc && r.active ? `about ${usd0(r.monthly)} a month` : ''
    const acts = r.mark && !r.charged_after_cancel ? [['Undo', 'clear']] : [[r.charged_after_cancel ? 'Mark cancelled again' : inc ? 'Mark stopped' : 'Mark cancelled', 'cancelled'], r.charged_after_cancel ? ['Clear', 'clear'] : ['Not recurring', 'hidden']]
    return `<div class="arow"><button data-act="recq" data-i="${i}"><span><span class="n">${esc(r.name)}</span>${tags}<br>
    <span class="s">${r.cadence[0].toUpperCase() + r.cadence.slice(1)}${r.category ? ` · ${esc(catName(r.category))}` : ''} · ${r.active && r.mark !== 'cancelled' || r.charged_after_cancel ? `next around ${(r.cadence === 'yearly' ? dMed : dShort)(r.next)}` : `last ${dMed(r.last)}`}${what ? ` · ${what}` : ''}</span></span>
    <span class="b">${usd2(r.amount)}</span><span class="ch">${icon('chevR', 16)}</span></button>
    <div class="racts">${acts.map(([l, v]) => `<button class="linkbtn" data-act="recmark" data-i="${i}" data-v="${v}" aria-label="${l}: ${esc(r.name)}">${l}</button>`).join('')}</div></div>`
  }
  const total = rs => rs.reduce((s, r) => s + r.monthly, 0)
  const group = (title, rs, right = '', cap = '') => rs.length ? `<section class="panel acctgroup" style="padding-left:0;padding-right:0"><div class="gh" style="padding-top:0"><h2>${title}</h2><b>${right}</b></div>
   ${cap ? `<p class="cap" style="padding:0 20px 8px">${cap}</p>` : ''}${rs.map(row).join('')}</section>` : ''
  const kind = k => live.filter(r => r.kind === k), subs = kind('subscription'), bills = kind('bill'), pay = kind('income')
  const month = rs => `about ${usd0(total(rs))} a month`
  return `${head}
  <p class="sub">Found in your transaction history: the same merchant at a steady interval. Subscriptions also need a similar amount each time; bills (housing, utilities, insurance, transportation) can vary. Needs three charges (two for yearly), so short history finds fewer. <a href="#spending">Back to Spending</a></p>
  ${soon.length ? `<section class="panel acctgroup" style="padding-left:0;padding-right:0"><div class="gh" style="padding-top:0"><h2>Coming up</h2><b>${usd0(soon.reduce((s, r) => s + r.amount, 0))} in the next 7 days</b></div>
   ${soon.map(r => `<div class="arow"><button data-act="recq" data-i="${R.indexOf(r)}"><span><span class="n">${esc(r.name)}</span><br><span class="s">${days(r.next) <= 0 ? 'Due any day' : `Around ${dShort(r.next)}`}</span></span><span class="b">${usd2(r.amount)}</span><span class="ch">${icon('chevR', 16)}</span></button></div>`).join('')}</section>` : ''}
  ${live.length ? '' : '<section class="panel"><div class="empty">No recurring charges found yet.</div></section>'}
  ${group('Subscriptions', subs, month(subs))}${group('Bills', bills, month(bills))}${group('Income', pay, month(pay))}
  ${group('Not seen lately', R.filter(r => !r.active && !r.mark), '', 'Overdue by more than half a cycle. Probably cancelled.')}
  ${group('Cancelled', R.filter(r => r.mark === 'cancelled' && !r.charged_after_cancel), '', 'Left out of the totals. If one bills again, it moves back to its group.')}
  ${hidden.length ? `<p class="need"><button class="linkbtn" data-act="rechidden" aria-expanded="${!!state.recHidden}">${state.recHidden ? 'Hide' : 'Show'} ${hidden.length} marked not recurring</button></p>` : ''}
  ${state.recHidden ? group('Not recurring', hidden) : ''}`
}

/* ---------- Alerts (#alerts): charges worth a second look, found after each sync ---------- */
const AL_KIND = { new_merchant: 'New merchant', unusual_amount: 'Unusual amount', card_testing: 'Possible test charge', duplicate: 'Possible duplicate', away: 'Away from home' }
const AL_JEV = {
  suspicious: ['looks suspicious', 'Jev thinks this looks suspicious. Check it with your bank if you don’t recognize it.'],
  unusual: ['unusual but plausible', 'Jev: unusual for you, but plausible.'],
  normal: ['looked normal', 'Jev checked it and it looked normal.'],
}
function loadAlerts() {
  if (D.alerts != null || state.perr.al || inflight.has('al')) return
  inflight.add('al')
  api('/alerts').then(v => { D.alerts = v }, e => { state.perr.al = e.message }).finally(() => { inflight.delete('al'); if (state.view === 'alerts' || state.view === 'overview') render() })
}
const openAl = () => (D.alerts || []).filter(a => !a.checked)
const above = a => Math.round((a.so_far / a.usual_by_now - 1) * 100)
// One line for the Overview: the newest charge by name, or the categories running high.
function alertLine() {
  const open = openAl(); if (!open.length) return ''
  const top = open.find(a => a.tx), v = top && AL_JEV[top.jev]
  const text = top ? `${usd2(top.tx.amount)} at ${esc(top.tx.merchant)}${v ? ` (Jev says ${v[0]})` : ''}${open.length > 1 ? `, and ${open.length - 1} more, are worth a look.` : ' is worth a look.'}`
    : open.length > 1 ? `${open.length} spending categories are running high.` : `${esc(catName(open[0].category))} spending is running high.`
  return `<div class="callout">${icon('alert', 18)}<span>${text}</span><button class="linkbtn" data-act="go" data-v="alerts">Review alerts ${icon('arrow', 14)}</button></div>`
}
function alerts() {
  const head = `${banner()}<div class="pagehead"><h1>Alerts</h1></div>`, err = state.perr.al
  if (D.alerts == null) {
    loadAlerts()
    return `${head}<section class="panel">${err ? `<div class="empty"><p><strong>Could not load alerts</strong></p><p>${esc(err)}</p><p><button class="btn ghost" data-act="retryal">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'}</section>`
  }
  const A = D.alerts
  const card = a => {
    const i = A.indexOf(a)
    if (!a.tx) {
      const mx = Math.max(a.so_far, a.usual, 1), cat = esc(catName(a.category))
      return `<article class="acard"><div class="kicker"><span>Spending running high</span><span>${mName(a.month)}</span></div>
      <h2 class="ahead">${cat} is ${above(a)}% above usual</h2>
      <div class="figs"><div><b>${usd0(a.so_far)}</b><span>so far</span></div><div><b>${usd0(a.usual_by_now)}</b><span>usual by now</span></div><div><b>${usd0(a.usual)}</b><span>usual month</span></div></div>
      <div class="abar" aria-hidden="true"><i style="width:${f1(a.so_far / mx * 100)}%"></i><b style="left:calc(${f1(a.usual_by_now / mx * 100)}% - 1px)"></b></div>
      <div class="aacts"><button class="btn ghost sm" data-act="alfine" data-i="${i}">Fine this month</button><button class="btn ghost sm" data-act="alcat" data-i="${i}">See ${cat} spending</button></div></article>`
    }
    const t = a.tx, v = AL_JEV[a.jev]
    return `<article class="acard"><div class="kicker"><span>${AL_KIND[a.kind] || a.kind}</span><span>${dShort(t.date)} · ${esc(t.account)}</span></div>
    <h2 class="ahead">${usd2(t.amount)} at ${esc(t.merchant)}</h2>
    <ul class="why">${a.reasons.map(r => `<li>${esc(r)}</li>`).join('')}</ul>
    ${v ? `<p class="verdict v-${a.jev}">${v[1]}</p>` : ''}
    <div class="aacts"><button class="btn ghost sm" data-act="alfine" data-i="${i}">Looks fine</button><button class="btn ghost sm" data-act="altx" data-i="${i}">Open transaction</button></div></article>`
  }
  const open = openAl(), checked = A.filter(a => a.checked)
  return `${head}
  <p class="sub">Charges worth a second look, found after each sync from your own history. Mark the ones you recognize as fine; they won't come back. <a href="#transactions">Back to Transactions</a></p>
  ${open.length ? `<div class="agrid">${open.map(card).join('')}</div>` : '<section class="panel"><div class="empty">Nothing to look at. New alerts show up here after a sync.</div></section>'}
  ${checked.length ? `<p class="need"><button class="linkbtn" data-act="alchecked" aria-expanded="${!!state.alChecked}">${state.alChecked ? 'Hide' : 'Show'} ${checked.length} checked by Jev</button></p>` : ''}
  ${state.alChecked && checked.length ? `<h2 class="asec">Checked, looked normal</h2><div class="agrid">${checked.map(card).join('')}</div>` : ''}`
}

/* ---------- Investments (#investments): where each investment account's change came from ---------- */
// INV.data: last GET /investments for INV.period. Added = transfers in, payroll, conversions and share sales;
// market change = end - start - added - dividends (computed by the server).
const INV = { period: 'ytd', kind: 'all', chart: 'stack', hidden: new Set(), sort: 'end', dir: -1, data: null, err: '', open: new Set(), classing: null, form: [], formErr: '' }
const INV_COLORS = ['var(--income)', 'var(--cat-x2)', 'var(--cat-transportation)', 'var(--cat-housing)', 'var(--cat-x0)', 'var(--cat-x1)', 'var(--cat-x3)']
const invKind = a => /ira|401|403|roth|hsa|pension/i.test(a.name) ? 'Retirement' : /espp|rsu/i.test(a.name) ? 'Equity comp' : 'Taxable'
function loadInvestments() {
  if (inflight.has('inv')) return
  inflight.add('inv'); INV.err = ''
  api('/investments?period=' + INV.period).then(v => { INV.data = v }, e => { INV.err = e.message }).finally(() => { inflight.delete('inv'); if (state.view === 'investments') render() })
}
function investments() {
  const head = `${banner()}<div class="pagehead"><h1>Investments</h1>
    <div class="seg" role="group" aria-label="Period">${[['3m', '3 months'], ['ytd', 'This year'], ['1y', '12 months'], ['all', 'All']].map(([k, l]) => `<button data-act="invp" data-v="${k}" aria-pressed="${INV.period === k}">${l}</button>`).join('')}</div></div>`
  if (INV.err) return `${head}<section class="panel"><div class="empty"><p><strong>Could not load investments</strong></p><p>${esc(INV.err)}</p><p><button class="btn ghost" data-act="invp" data-v="${INV.period}">Retry</button></p></div></section>`
  if (!INV.data || INV.data.period !== INV.period) { loadInvestments(); return `${head}<div class="load" role="status">Loading…</div>` }
  const all = INV.data.accounts.map((a, i) => ({ ...a, kind: invKind(a), color: INV_COLORS[i % INV_COLORS.length], gain: a.market + a.dividends }))
  if (!all.length) return `${head}<section class="panel"><div class="empty"><p><strong>No investment accounts</strong></p><p>Set an account's type to Investment on the Accounts page.</p></div></section>`
  const shown = all.filter(a => INV.kind === 'all' || a.kind === INV.kind)
  const tot = k => shown.reduce((s, a) => s + a[k], 0), ret = (g, a) => (a.start + a.added / 2) >= 10000 ? g / (a.start + a.added / 2) : null // under $100 a percentage is noise
  const T = { start: tot('start'), added: tot('added'), dividends: tot('dividends'), market: tot('market'), end: tot('end'), gain: tot('gain') }
  const sgn = c => (c > 0 ? '+' : '') + usd0(c), pc = x => x == null ? '<span class="muted">—</span>' : `<span class="${x >= 0 ? 'up' : 'down'}">${x >= 0 ? '+' : '−'}${(Math.abs(x) * 100).toFixed(1)}%</span>`
  const rows = [...shown].sort((x, y) => INV.dir * (INV.sort === 'name' ? x.name.localeCompare(y.name) : INV.sort === 'ret' ? (ret(x.gain, x) ?? -Infinity) - (ret(y.gain, y) ?? -Infinity) : x[INV.sort] - y[INV.sort]))
  const th = (k, l) => `<th><button data-act="invs" data-v="${k}" ${INV.sort === k ? `aria-sort="${INV.dir < 0 ? 'descending' : 'ascending'}"` : ''}>${l}${INV.sort === k ? (INV.dir < 0 ? ' ↓' : ' ↑') : ''}</button></th>`
  return `${head}
  <p class="sub">${esc(dMed(INV.data.from))} to ${esc(dMed(INV.data.to))}${INV.period.includes('..') ? ', zoomed in. <button class="linkbtn" data-act="invunzoom">Reset zoom</button>' : ''}. Added is money you put in (transfers, payroll, conversions, vests once sold); market change is the rest of the growth.</p>
  <div class="invf"><div class="seg" role="group" aria-label="Account type">${['all', 'Taxable', 'Retirement', 'Equity comp'].map(k => `<button data-act="invk" data-v="${k}" aria-pressed="${INV.kind === k}">${k === 'all' ? 'All' : k}</button>`).join('')}</div></div>
  <section class="panel"><div class="fstats">
    <div><span class="lab"><i style="background:var(--income)"></i>Balance</span><b>${usd0(T.end)}</b></div>
    <div><span class="lab"><i style="background:var(--kept)"></i>You added</span><b>${sgn(T.added)}</b></div>
    <div><span class="lab"><i style="background:var(--cat-x2)"></i>Dividends</span><b>${sgn(T.dividends)}</b></div>
    <div><span class="lab"><i style="background:${T.market >= 0 ? 'var(--kept)' : 'var(--err)'}"></i>Market change</span><b>${sgn(T.market)}</b></div>
    <div><span class="lab">Return</span><b>${pc(ret(T.gain, T))}</b></div></div></section>
  <section class="panel" style="margin-top:16px"><div class="phead"><h2>Over time</h2><div class="seg" role="group" aria-label="Chart">${[['stack', 'Balance by account'], ['growth', 'Growth vs. added']].map(([k, l]) => `<button data-act="invc" data-v="${k}" aria-pressed="${INV.chart === k}">${l}</button>`).join('')}</div></div>
    ${INV.chart === 'stack' ? `<div class="invleg">${shown.map(a => `<button data-act="invh" data-v="${esc(a.id)}" aria-pressed="${!INV.hidden.has(a.id)}"><i style="background:${a.color}"></i>${esc(clip(a.name, 34))}</button>`).join('')}</div>`
      : '<div class="legend"><span><i style="background:var(--income)"></i>Balance</span><span><i style="background:var(--kept)"></i>Starting balance + what you added</span></div>'}
    ${invChart(INV.chart === 'stack' ? shown.filter(a => !INV.hidden.has(a.id)) : shown)}
    <p class="cap">${INV.chart === 'stack' ? 'Select an account in the legend to hide it.' : 'The gap between the lines is growth: dividends plus market change.'} Month-end balances. Drag across the chart to zoom in.</p></section>
  <section class="panel" style="margin-top:16px"><div class="tblwrap"><table class="invtbl"><thead><tr>${th('name', 'Account')}${th('start', 'Start')}${th('added', 'Added')}${th('dividends', 'Dividends')}${th('market', 'Market')}${th('ret', 'Return')}${th('end', 'Balance')}</tr></thead>
    <tbody>${rows.map(a => `<tr data-act="invrow" data-v="${esc(a.id)}" tabindex="0"><td><span class="invn"><i style="background:${a.color}"></i><span><b>${esc(a.name)}</b><br><small>${esc(a.institution)} · ${a.kind}</small></span></span></td>
      <td>${usd0(a.start)}</td><td>${sgn(a.added)}</td><td>${sgn(a.dividends)}</td><td class="${a.market >= 0 ? 'up' : 'down'}">${sgn(a.market)}</td><td>${pc(ret(a.gain, a))}</td><td><b>${usd0(a.end)}</b></td></tr>`).join('')}</tbody>
    <tfoot><tr><td>Total</td><td>${usd0(T.start)}</td><td>${sgn(T.added)}</td><td>${sgn(T.dividends)}</td><td class="${T.market >= 0 ? 'up' : 'down'}">${sgn(T.market)}</td><td>${pc(ret(T.gain, T))}</td><td>${usd0(T.end)}</td></tr></tfoot></table></div>
    <p class="cap">Return is dividends plus market change over the starting balance plus half of what you added: a simple estimate, not the time-weighted return your brokerage reports. Select a row to open the account.</p></section>
  ${holdingsHTML(shown)}`
}
// Holdings: the allocation of the accounts shown, then each account's latest positions with gains where the
// brokerage sent a cost, its unvested awards, and the owner's classification form for any symbol.
const CLASSES = [['us_stock', 'US stock', 'var(--income)'], ['intl_stock', 'International stock', 'var(--cat-x2)'], ['bonds', 'Bonds', 'var(--cat-housing)'], ['cash', 'Cash', 'var(--kept)'], ['other', 'Other', 'var(--cat-other)']]
const classLabel = c => {
  if (!c) return '<span class="muted">Unclassified</span>'
  const parts = c.split.map((bp, i) => [bp, CLASSES[i][1]]).filter(([bp]) => bp).sort((x, y) => y[0] - x[0])
  const what = parts.length === 1 ? parts[0][1] : parts.map(([bp, l]) => `${Math.round(bp / 100)}% ${l.replace('International', 'intl.').replace(/^[BCO]/, c => c.toLowerCase())}`).join(', ')
  const from = c.source === 'sec' ? `SEC filing ${dMed(c.as_of)}` : { owner: 'your split', stock: 'a company stock', money_market: 'a money market fund' }[c.source]
  return `${what}<br><small class="muted">From ${from}</small>`
}
function holdingsHTML(list) {
  const held = list.filter(a => a.positions?.length || a.unvested).map(a => ({ ...a, positions: a.positions || [] })) // none sent when an account holds only unvested awards
  if (!held.length) return ''
  const al = Object.fromEntries([...CLASSES.map(([k]) => k), 'unclassified'].map(k => [k, held.reduce((s, a) => s + a.allocation[k], 0)]))
  const total = Object.values(al).reduce((s, v) => s + v, 0) || 1
  const segs = [...CLASSES, ['unclassified', 'Unclassified', 'var(--line)']].filter(([k]) => al[k] > 0)
  const unvested = held.reduce((s, a) => s + a.unvested, 0)
  const table = a => {
    const withCost = a.positions.some(p => p.cost != null)
    const row = p => {
      const sym = esc(p.symbol), editing = INV.classing === p.symbol && INV.classAcct === a.id
      return `<tr><td><b>${sym || '—'}</b><br><small class="muted">${esc(clip(p.description, 48))}</small></td><td>${privacy.on ? '•••' : p.shares.toLocaleString('en-US', { maximumFractionDigits: 3 })}</td><td>${usd0(p.value)}</td>
        ${withCost ? (p.cost == null ? '<td class="muted">Unknown</td><td></td>' : `<td>${usd0(p.cost)}</td><td class="${p.value >= p.cost ? 'up' : 'down'}">${(p.value >= p.cost ? '+' : '−') + usd0(Math.abs(p.value - p.cost))}</td>`) : ''}
        <td>${classLabel(p.class)}${p.symbol && !editing ? ` <button class="linkbtn" data-act="classify" data-v="${sym}" data-a="${esc(a.id)}">${p.class?.source === 'owner' ? 'Change' : p.class ? 'Override' : 'Classify'}</button>` : ''}</td></tr>
        ${editing ? `<tr><td colspan="${withCost ? 6 : 4}">${classForm(p)}</td></tr>` : ''}`
    }
    return `<details class="hold" ${INV.open.has(a.id) ? 'open' : ''} data-id="${esc(a.id)}"><summary><b>${esc(a.name)}</b> <span class="muted">${esc(a.institution)}</span><span class="hv">${usd0(a.positions.reduce((s, p) => s + p.value, 0))}</span></summary>
      ${a.positions.length ? `<div class="tblwrap"><table class="invtbl htbl"><thead><tr><th>Holding</th><th>Shares</th><th>Value</th>${withCost ? '<th>Cost</th><th>Gain</th>' : ''}<th>Class</th></tr></thead><tbody>${a.positions.map(row).join('')}</tbody>
      ${withCost ? `<tfoot><tr><td>Known cost</td><td></td><td>${usd0(a.cost + a.unrealized)}</td><td>${usd0(a.cost)}</td><td class="${a.unrealized >= 0 ? 'up' : 'down'}">${(a.unrealized >= 0 ? '+' : '−') + usd0(Math.abs(a.unrealized))}</td><td></td></tr></tfoot>` : ''}</table></div>` : ''}
      <p class="cap">${withCost && a.no_cost ? `${usd0(a.no_cost)} has no known cost and is left out of the gain. ` : withCost ? '' : 'This brokerage sends no cost basis, so there are no gains to show. '}${a.unvested ? `<b>Unvested:</b> ${usd0(a.unvested)} in ${a.unvested_count} award${a.unvested_count === 1 ? '' : 's'}, an estimate at the price of the last sync, not in net worth, gains or allocation. ` : ''}As of ${rel(a.holdings_at)}.</p></details>`
  }
  return `<section class="panel" style="margin-top:16px"><div class="phead"><h2>Allocation</h2></div>
    <div class="allocbar" role="img" aria-label="${segs.map(([k, l]) => `${l} ${pct(al[k] / total)}`).join(', ')}">${segs.map(([k, l, c]) => `<i style="width:${f1(al[k] / total * 100)}%;background:${c}" title="${l}"></i>`).join('')}</div>
    <div class="legend alleg">${segs.map(([k, l, c]) => `<span><i style="background:${c}"></i>${l} <b>${pct(al[k] / total)}</b> <span class="muted">${usd0(al[k])}</span></span>`).join('')}</div>
    <p class="cap">Latest holdings of the accounts shown.${unvested ? ` Unvested awards (${usd0(unvested)}) are left out.` : ''}${al.unclassified ? ' Classify the unclassified holdings below to complete it.' : ''}${D.config.fund_lookups ? '' : ' Fund lookups are off, so funds need your own split (see Settings).'}</p></section>
  <section class="panel" style="margin-top:16px"><div class="phead"><h2>Holdings</h2></div>${held.map(table).join('')}
    <p class="cap">Gain is value minus what the brokerage reports you paid: unrealized, before tax, and only where a cost was sent.</p></section>`
}
function classForm(p) {
  const v = INV.form
  return `<form class="clsform" data-cls="${esc(p.symbol)}"><p><b>Split for ${esc(p.symbol)}</b>, in percent. It must total 100.</p>
    <div class="clsin">${CLASSES.map(([k, l], i) => `<label>${l}<input type="number" min="0" max="100" step="1" name="${k}" value="${v[i]}" inputmode="numeric"></label>`).join('')}</div>
    <p class="cap">A target-date fund moves toward bonds over time; a split you set here stays as you set it.</p>
    ${INV.formErr ? `<p class="warn" role="alert">${esc(INV.formErr)}</p>` : ''}
    <div class="frow"><button class="btn sm">Save</button><button type="button" class="btn ghost sm" data-act="classcancel">Cancel</button>${p.class?.source === 'owner' ? `<button type="button" class="btn ghost sm" data-act="classclear" data-v="${esc(p.symbol)}">Clear my split</button>` : ''}</div></form>`
}
function invChart(list) {
  if (!list.length) return '<div class="empty">No accounts shown. Turn one back on in the legend.</div>'
  const months = list[0].months, n = months.length, W = 760, H = 240, L = 58, B = 24, T = 10
  const x = k => L + (W - L - 12) * (n === 1 ? 0.5 : k / (n - 1))
  const bal = months.map((_, k) => list.reduce((s, a) => s + a.months[k].balance, 0) / 100)
  const put = months.map((_, k) => list.reduce((s, a) => s + a.start + a.months[k].added, 0) / 100)
  let lo = 0, hi
  if (INV.chart === 'stack') hi = Math.max(...months.map((_, k) => list.reduce((s, a) => s + Math.max(0, a.months[k].balance), 0) / 100), 1) * 1.04
  else { lo = Math.min(...bal, ...put) * 0.97; hi = Math.max(...bal, ...put) * 1.02 }
  const step = niceStep(hi - lo, 4), y = v => T + (H - T - B) * (1 - (v - lo) / (hi - lo))
  const path = arr => arr.map((v, k) => `${k ? 'L' : 'M'}${f1(x(k))},${f1(y(v))}`).join('')
  const back = arr => arr.map((_, k) => `L${f1(x(n - 1 - k))},${f1(y(arr[n - 1 - k]))}`).join('')
  let g = ''
  for (let v = Math.ceil(lo / step) * step; v <= hi; v += step) g += `<line class="gridline" x1="${L}" x2="${W}" y1="${f1(y(v))}" y2="${f1(y(v))}"/><text x="${L - 8}" y="${f1(y(v) + 4)}" text-anchor="end">${axis(v)}</text>`
  if (INV.chart === 'stack') {
    let base = months.map(() => 0)
    for (const a of list) {
      const upper = a.months.map((m, k) => base[k] + Math.max(0, m.balance) / 100)
      g += `<path d="${path(upper)}${back(base)}Z" style="fill:${a.color}" opacity=".85"><title>${esc(a.name)}: ${usd0(a.end)}</title></path>`
      base = upper
    }
  } else {
    g += `<path d="${path(bal)}${back(put)}Z" style="fill:var(--kept)" opacity=".14"/>`
    g += `<path d="${path(put)}" fill="none" style="stroke:var(--kept)" stroke-width="2" stroke-dasharray="5 4"/><path d="${path(bal)}" fill="none" style="stroke:var(--income)" stroke-width="2"/>`
    months.forEach((m, k) => { g += `<circle cx="${f1(x(k))}" cy="${f1(y(bal[k]))}" r="3" style="fill:var(--income)"><title>${mName(m.month)}: balance ${usd0(bal[k] * 100)}, added so far ${usd0(put[k] * 100)}</title></circle>` })
  }
  const every = Math.ceil(n / 8)
  months.forEach((m, k) => { if (k % every === 0 || k === n - 1) g += `<text x="${f1(x(k))}" y="${H - 6}" text-anchor="middle">${mShort(m.month)}</text>` })
  return `<svg class="invchart" viewBox="0 0 ${W} ${H}" role="img" aria-label="Investment balances over time">${g}</svg>`
}

/* ---------- Plan (#plan): a forecast in today's dollars from the owner's own numbers, what-ifs, goals ---------- */
// PL.data: GET /plan (doc = what's saved, baseline = what the data says, defaults, projection, data_projection).
// PL.vals: the doc being tried. Every change is a what-if (POST /plan/projection) until Keep saves it.
const PL = { data: null, err: '', vals: null, proj: null, goals: null, ev: null, goal: null, gErr: '', timer: 0, seq: 0, busy: false }
const blankEv = () => ({ kind: 'expense', name: '', year: '', amount: '', until: '' })
const clone = o => JSON.parse(JSON.stringify(o))
const isObj = v => v && typeof v === 'object' && !Array.isArray(v)
// Key order, unset keys and empty objects don't matter when comparing docs.
const canon = o => JSON.stringify(o, (k, v) => isObj(v) ? Object.fromEntries(Object.entries(v).filter(([, x]) => x != null && !(isObj(x) && !Object.keys(x).length)).sort()) : v)
const r10k = c => privacy.on ? '$•••' : c >= 99500000 ? '$' + (Math.round(c / 1e7) / 10).toFixed(1) + 'M' : '$' + Math.round(c / 1e6) * 10 + 'k'
function setPlan(p) { PL.data = p; PL.vals = p.doc ? clone(p.doc) : null; PL.proj = null; PL.ev = PL.ev || blankEv() }
function loadPlan() {
  if (inflight.has('plan')) return
  inflight.add('plan'); PL.err = ''
  Promise.all([api('/plan'), api('/goals')]).then(([p, g]) => { setPlan(p); PL.goals = g }, e => { PL.err = e.message }).finally(() => { inflight.delete('plan'); if (state.view === 'plan') render() })
}
const planDirty = () => PL.vals && canon(PL.vals) !== canon(PL.data.doc)
// The numbers anyone can change, with where their starting value comes from.
const KNOBS = [['retire_age', 'Retirement age', 'age'], ['income_monthly', 'Income a month', 'money'], ['spending_monthly', 'Spending a month', 'money'],
  ['retire_extra', 'Extra spending a year once retired', 'money'], ['retire_spend_pct', 'Retirement spending, % of today’s', 'pct'], ['ss_monthly', 'Social Security a month', 'money'],
  ['ss_age', 'Social Security from age', 'age'], ['inflation', 'Inflation', 'pct'], ['end_age', 'Plan until age', 'age']]
const knobBase = k => ({ income_monthly: PL.data.baseline.income_monthly, spending_monthly: PL.data.baseline.spending_monthly })[k] ?? PL.data.defaults[k]
const fromData = k => k === 'income_monthly' || k === 'spending_monthly'
const showVal = (kind, v) => kind === 'money' ? usd0(v) : kind === 'pct' ? `${v}%` : String(v)
// Set a what-if value; back at its starting value, the override goes away.
function tryVal(obj, k, v, base) { if (v === base || v === '' || v == null) delete obj[k]; else obj[k] = v; planChanged() }
function planChanged() {
  clearTimeout(PL.timer)
  if (!planDirty()) { PL.proj = null; PL.pending = false; PL.seq++; render(); return }
  PL.pending = true; render()
  const seq = ++PL.seq, body = clone(PL.vals)
  PL.timer = setTimeout(() => send('POST', '/plan/projection', body).then(r => { if (seq === PL.seq) { PL.proj = r; PL.err = '' } }, e => { if (seq === PL.seq) PL.err = e.message }).finally(() => { if (seq === PL.seq) { PL.pending = false; if (state.view === 'plan') render() } }), 250)
}
function plan() {
  const head = `${banner()}<div class="pagehead"><h1>Plan</h1></div>`
  if (!PL.data) {
    if (!PL.err) loadPlan()
    return `${head}<section class="panel">${PL.err ? `<div class="empty"><p><strong>Could not load the plan</strong></p><p>${esc(PL.err)}</p><p><button class="btn ghost" data-act="planretry">Retry</button></p></div>` : '<div class="load" role="status">Loading…</div>'}</section>`
  }
  const D = PL.data, v = PL.vals
  if (!v) return `${head}<section class="panel setup"><h2>One question first</h2>
    <p>The forecast is built from your own income, spending, balances and contributions. It only needs your birth year to know how many years are ahead.</p>
    <form class="frow" data-planform="setup"><label class="fl">Birth year<input id="birthyear" type="number" min="${new Date().getFullYear() - 110}" max="${new Date().getFullYear() - 18}" required inputmode="numeric"></label><button class="btn">Show my forecast</button></form>
    ${PL.err ? `<p class="warn" role="alert">${esc(PL.err)}</p>` : ''}
    <p class="cap">The rest starts from your data, or from defaults where there is none: retirement at ${D.defaults.retire_age}, Social Security of ${usd0(D.defaults.ss_monthly)} a month from ${D.defaults.ss_age}, a plan to age ${D.defaults.end_age}. You can change any of it.</p></section>`
  const dirty = planDirty(), P = dirty && PL.proj ? PL.proj.projection : D.projection, Dp = dirty && PL.proj ? PL.proj.data_projection : D.data_projection
  const changed = canon(v) !== canon({ birth_year: v.birth_year, events: v.events, vests: { off: v.vests?.off || undefined } })
  const ra = v.retire_age ?? D.defaults.retire_age, end = v.end_age ?? D.defaults.end_age, age = new Date().getFullYear() - v.birth_year
  const at = (p, age) => (p.years.find(y => y.age === age) || p.years.at(-1)).assets
  const lasts = p => p.run_out ? `Runs out at ${p.run_out}` : `Past ${p.years.at(-1).age}`
  const accts = D.baseline.accounts, acc = id => v.accounts?.[id] || {}
  const included = accts.filter(a => acc(a.id).include ?? a.include), nowV = included.reduce((s, a) => s + Math.max(0, a.balance), 0)
  const knob = ([k, label, kind]) => {
    const base = knobBase(k), cur = v[k] ?? base, ch = v[k] != null && v[k] !== base, saved = D.doc[k] != null && D.doc[k] === v[k]
    const src = fromData(k) ? `Average of your last ${D.baseline.months} month${D.baseline.months === 1 ? '' : 's'}` : 'Default'
    const note = ch ? `${saved ? 'Set by you.' : `Trying ${showVal(kind, cur)}.`} ${fromData(k) ? 'Your data' : 'Default'}: ${showVal(kind, base)}. <button class="linkbtn" data-act="knobreset" data-v="${k}">Use ${fromData(k) ? 'my data' : 'the default'}</button>` : src + '.'
    const input = privacy.on && kind === 'money' ? '<span class="muted">$•••</span>'
      : k === 'retire_age' ? `<input id="kn-${k}" type="range" min="${age}" max="${Math.max(80, age)}" step="1" value="${cur}" data-knob="${k}" data-kind="${kind}">`
      : `<input id="kn-${k}" type="number" step="${kind === 'money' ? 100 : kind === 'pct' ? 0.5 : 1}" value="${kind === 'money' ? Math.round(cur / 100) : cur}" data-knob="${k}" data-kind="${kind}" inputmode="decimal">`
    return `<div class="knob${k === 'retire_age' ? ' wide' : ''}${ch ? ' chg' : ''}"><label for="kn-${k}">${label}${k === 'retire_age' ? `: <b id="ra-v">${cur}</b>` : ''}</label>${input}<div class="src">${note}</div></div>`
  }
  const evs = [...(v.events || []).map((e, i) => ({ ...e, i })), ...[[ra, `Retire at ${ra}`], [v.ss_age ?? D.defaults.ss_age, `Social Security, ${usd0(v.ss_monthly ?? D.defaults.ss_monthly)} a month`], [end, `End of plan at ${end}`]].map(([age, text]) => ({ year: v.birth_year + age, text, fixed: true }))]
    .sort((a, b) => a.year - b.year)
  const evText = e => e.fixed ? e.text : `${esc(e.name)}: ${e.kind === 'expense' ? `one-time expense of ${usd0(e.amount)}` : e.kind === 'income' ? `one-time income of ${usd0(e.amount)}`
    : `${e.kind === 'spending' ? 'spending' : 'income'} ${e.amount < 0 ? 'down' : 'up'} ${usd0(Math.abs(e.amount))} a year${e.until ? ` until ${e.until}` : ''}`}`
  const vestOn = !v.vests?.off && D.baseline.unvested > 0
  const E = PL.ev
  return `${head}
  <p class="sub">In today's dollars. Every number starts from your last ${D.baseline.months} months, your balances and your contributions, and you can try others below.${PL.pending ? ' <span role="status">Updating…</span>' : ''}</p>
  ${PL.err ? notice(PL.err) : ''}
  <section class="panel${PL.pending ? ' stale' : ''}">
    <p class="lead">${changed ? 'With your changes, retiring' : 'Retiring'} at ${ra}, you’d have about ${r10k(at(P, ra))} then. <span class="${P.run_out ? 'down' : ''}">${P.run_out ? `The money runs out at ${P.run_out}.` : `It lasts past ${end}.`}</span></p>
    ${changed ? `<p class="was">From your data alone: about ${r10k(at(Dp, D.defaults.retire_age))} at ${D.defaults.retire_age}, and ${Dp.run_out ? `it runs out at ${Dp.run_out}` : `it lasts past ${Dp.years.at(-1).age}`}.</p>` : ''}
    <div class="pstats">
      <div><span>Included assets now</span><b>${r10k(nowV)}</b></div>
      <div><span>At retirement (${ra})</span><b>${r10k(at(P, ra))}</b>${changed ? `<small>Your data: ${r10k(at(Dp, D.defaults.retire_age))}</small>` : ''}</div>
      <div><span>At end of plan (${end})</span><b>${P.run_out ? usd0(0) : r10k(P.years.at(-1).assets)}</b>${changed ? `<small>Your data: ${Dp.run_out ? usd0(0) : r10k(Dp.years.at(-1).assets)}</small>` : ''}</div>
      <div><span>Money lasts</span><b>${lasts(P)}</b>${changed ? `<small>Your data: ${lasts(Dp)}</small>` : ''}</div>
    </div>
    ${planChart(P, changed ? Dp : null, ra, v)}
    <div class="legend"><span><i style="background:var(--income)"></i>${changed ? (dirty ? 'With your changes (not saved)' : 'Your plan') : 'From your data'}</span>${changed ? '<span><i class="dash"></i>From your data</span>' : ''}<span>Shaded: retirement</span><span style="color:var(--spend)">Events</span><span style="color:var(--cat-x2)">Goals</span></div>
    <p class="note">A direction, not a prediction: today's dollars, steady growth with no market swings, and flat tax rates (taxable 12%, pre-tax 20%, Roth 0%, plus 10% on pre-tax and Roth before 59½).</p>
  </section>
  <section class="panel" style="margin-top:16px"><div class="phead"><h2>Your numbers</h2>
    ${dirty ? `<div class="frow" style="margin:0"><button class="btn" data-act="plankeep" ${PL.busy ? 'disabled' : ''}>Keep these changes</button><button class="btn ghost" data-act="planundo">Undo changes</button></div>` : changed ? '<button class="linkbtn" data-act="planalldata">Use all my data again</button>' : ''}</div>
    <p class="cap" style="margin:0 0 6px">Filled in from your data, or a default where there is none. Change any to try a different plan; the chart compares it with your data. Nothing is saved until you keep the changes.${privacy.on ? ' Amounts are hidden; show them to change these.' : ''}</p>
    <div class="knobs">${KNOBS.map(knob).join('')}
      <div class="knob wide${v.surplus_account ? ' chg' : ''}"><label for="kn-surplus">Money left over each year goes to</label><select id="kn-surplus" data-surplus>${[['', 'Extra savings (grows 2% a year)'], ...included.map(a => [a.id, a.name])].map(([id, n]) => `<option value="${esc(id)}" ${(v.surplus_account || '') === id ? 'selected' : ''}>${esc(n)}</option>`).join('')}</select><div class="src">${v.surplus_account ? 'Set by you. Default: Extra savings.' : 'Default.'}</div></div></div>
  </section>
  ${goalsHTML(v)}
  <section class="panel" style="margin-top:16px">
    ${sourcesHTML(P, accts)}
    <details class="psec" ${PL.open?.accounts ? 'open' : ''} data-sec="accounts"><summary>Accounts <span class="muted">${included.length} included</span></summary>
      <div class="tblwrap"><table class="invtbl ptbl"><thead><tr><th>Account</th><th>In plan</th><th>Kind</th><th>Growth</th><th>Added a year</th><th>Balance</th></tr></thead><tbody>
      ${accts.map(a => { const o = acc(a.id), on = o.include ?? a.include, ch = ['include', 'bucket', 'growth', 'contribution'].some(f => o[f] != null)
        return `<tr class="${ch ? 'chg' : ''}"><td><b>${esc(a.name)}</b><br><small class="muted">${esc(a.institution)}${ch ? ` · changed <button class="linkbtn" data-act="acctreset" data-v="${esc(a.id)}">use my data</button>` : ''}</small></td>
          <td><input type="checkbox" id="pa-${esc(a.id)}-include" data-acct="${esc(a.id)}" data-f="include" ${on ? 'checked' : ''} aria-label="Include ${esc(a.name)}"></td>
          <td><select id="pa-${esc(a.id)}-bucket" data-acct="${esc(a.id)}" data-f="bucket" aria-label="Kind of ${esc(a.name)}">${[['cash', 'Cash'], ['taxable', 'Taxable'], ['pretax', 'Pre-tax'], ['roth', 'Roth']].map(([b, l]) => `<option value="${b}" ${(o.bucket ?? a.bucket) === b ? 'selected' : ''}>${l}</option>`).join('')}</select></td>
          <td><input type="number" step="0.5" class="pnum" id="pa-${esc(a.id)}-growth" data-acct="${esc(a.id)}" data-f="growth" value="${o.growth ?? a.growth}" aria-label="Growth of ${esc(a.name)}, % a year">%${a.interest ? `<br><small class="muted">paid ${usd0(a.interest)} a year</small>` : ''}</td>
          <td>${privacy.on ? '$•••' : `<input type="number" step="500" class="pnum" id="pa-${esc(a.id)}-contribution" data-acct="${esc(a.id)}" data-f="contribution" value="${Math.round((o.contribution ?? a.contribution) / 100)}" aria-label="Added to ${esc(a.name)} a year, dollars">`}${addedNote(a)}</td>
          <td>${usd0(a.balance)}</td></tr>` }).join('')}</tbody></table></div>
      <p class="cap">Added a year starts from money that came from outside your accounts in the last ${D.baseline.months} months: payroll deposits and ESPP sales. RSU sales are left out (future vests come from your unvested stock), and so are moves between your own accounts, which are already part of your surplus. A cash account that paid interest grows at the rate it paid. Growth is before inflation. Cards, loans and property are left out unless you include them.</p></details>
    <details class="psec" ${PL.open?.events ? 'open' : ''} data-sec="events"><summary>Events <span class="muted">${evs.length + (vestOn ? 1 : 0)} in the plan</span></summary>
      ${evs.map(e => `<div class="pev"><span class="y">${e.year}</span><span class="d">${evText(e)}</span>${e.fixed ? '<span class="muted" style="font-size:12px">Set in your numbers</span>' : `<button class="linkbtn" data-act="evedit" data-i="${e.i}">Edit</button><button class="linkbtn" data-act="evdel" data-i="${e.i}">Remove</button>`}</div>`).join('')}
      ${D.baseline.unvested > 0 ? `<div class="pev"><span class="y">${new Date().getFullYear()}</span><span class="d">${vestOn ? vestText(P.vests) : '<span class="muted">Unvested stock is left out</span>'}</span><button class="linkbtn" data-act="vests">${vestOn ? 'Leave out' : 'Add back'}</button></div>` : ''}
      <form class="evform" data-planform="event">
        <label>Kind<select name="kind">${[['expense', 'One-time expense'], ['income', 'One-time income'], ['spending', 'Spending change a year'], ['earning', 'Income change a year']].map(([k, l]) => `<option value="${k}" ${E.kind === k ? 'selected' : ''}>${l}</option>`).join('')}</select></label>
        <label>Name<input name="name" value="${esc(E.name)}" placeholder="New roof" maxlength="80" required></label>
        <label>Year<input name="year" type="number" value="${E.year}" min="${new Date().getFullYear()}" required></label>
        <label>Amount, $<input name="amount" type="number" step="100" value="${E.amount}" required></label>
        <label>Until (optional)<input name="until" type="number" value="${E.until}"></label>
        <button class="btn ghost">${E.editing != null ? 'Save event' : 'Add event'}</button></form>
      <p class="cap">A yearly change can be negative, like a mortgage paid off. Events are part of the what-if too until you keep the changes.</p></details>
    <details class="psec" ${PL.open?.years ? 'open' : ''} data-sec="years"><summary>Year by year <span class="muted">${P.years.length} years</span></summary>
      <div class="tblwrap"><table class="invtbl ptbl"><thead><tr><th>Year</th><th>Age</th><th>Income</th><th>Spending</th><th>Withdrawn</th><th>Tax</th><th>Assets</th></tr></thead><tbody>
      ${P.years.map(y => `<tr class="${y.shortfall ? 'short' : ''}"><td>${y.year}</td><td>${y.age}</td><td>${usd0(y.income)}</td><td>${usd0(y.spending)}</td><td>${y.withdrawn ? usd0(y.withdrawn) : '—'}</td><td>${y.taxes ? usd0(y.taxes) : '—'}</td><td>${y.shortfall ? `<span class="down">${usd0(-y.shortfall)}</span>` : usd0(y.assets)}</td></tr>`).join('')}</tbody></table></div></details>
  </section>`
}
// What an account's Added a year is made of, and what was left out of it.
function addedNote(a) {
  const inn = [a.deposits && `${usd0(a.deposits)} deposits`, a.espp && `${usd0(a.espp)} ESPP`].filter(Boolean)
  const out = [a.rsu && `${usd0(a.rsu)} RSU sales`, a.unclear && `${usd0(a.unclear)} unclear stock sales`, a.internal && `${usd0(a.internal)} from your own accounts`].filter(Boolean)
  return inn.length || out.length ? `<br><small class="muted">${inn.join(' + ')}${out.length ? `${inn.length ? '; ' : ''}left out: ${out.join(', ')}` : ''}</small>` : ''
}
function vestText(vs) {
  const D = PL.data, m = D.baseline.months, pace = D.baseline.accounts.reduce((s, a) => s + a.rsu, 0)
  const how = vs.source === 'pace' ? `at the pace of your RSU sales (${usd0(pace)} a year over the last ${m} months)`
    : vs.source === 'owner' ? '(set by you)' : `(assumed: no RSU sales in your last ${m} months to set the pace)`
  return `Unvested stock, ${usd0(vs.total)} at the last sync: ${usd0(vs.total * vs.after_tax_pct / 100)} after tax (${vs.after_tax_pct}%, assumed), vesting over ${vs.years} year${vs.years === 1 ? '' : 's'} ${how}. Only while you're working; what hasn't vested at retirement is forfeited.`
}
// Where the forecast's starting numbers come from, what was left out so nothing counts twice, and what is
// assumed rather than read from the data.
function sourcesHTML(P, accts) {
  const D = PL.data, B = D.baseline, m = B.months, name = id => accts.find(a => a.id === id)?.name || id
  const inPlan = accts.filter(a => PL.vals.accounts?.[a.id]?.include ?? a.include), sum = (list, f) => list.reduce((s, a) => s + (a[f] || 0), 0)
  const rsu = sum(accts, 'rsu'), internal = sum(accts, 'internal'), deposits = sum(inPlan, 'deposits'), espp = sum(inPlan, 'espp')
  const item = (head, body) => `<div class="pev"><span class="d"><b>${head}</b><br><span class="muted">${body}</span></span></div>`
  const srcs = B.income_sources.map(s => `<button class="linkbtn" data-act="plansrc" data-v="${esc(s.name)}">${esc(s.name)}</button> ${usd0(s.amount)} a year (${s.count})`).join(' · ')
  const left = []
  if (rsu) left.push(`${usd0(rsu)} a year of RSU sales: ${B.unvested > 0 ? 'future vests come from your unvested stock instead, so they count once' : 'your brokerage doesn’t report unvested stock, so no future vests are in the plan. Add an income event if you expect more'}.`)
  if (internal) left.push(`${usd0(internal)} a year moved between your own accounts (journals, Roth conversions, withdrawals): not new money.`)
  if (B.interest_monthly) left.push(`${usd0(B.interest_monthly * 12)} a year of interest: it’s those accounts’ growth, so it isn’t income too.`)
  const unclear = B.unclear_sales.map(u => `${u.date} ${esc(name(u.account_id))}: ${usd0(u.amount)}`).join(' · ')
  const yr = P.years[0]?.year
  return `<details class="psec" ${PL.open?.sources ?? true ? 'open' : ''} data-sec="sources"><summary>Where the numbers come from <span class="muted">so nothing counts twice</span></summary>
    ${item(`Income: ${usd0(B.income_monthly)} a month`, `The average of your last ${m} months of deposits into checking, savings and cards. Largest: ${srcs || 'none'}. If one is money from your own accounts, like stock sale proceeds from a brokerage that isn’t connected, set its category to Transfer and it leaves the forecast.`)}
    ${item(`Added to investments: ${usd0(deposits + espp)} a year`, `${usd0(deposits)} of payroll and other deposits from outside your accounts${espp ? ` and ${usd0(espp)} of ESPP sales` : ''}, until you retire.`)}
    ${left.length ? item('Left out', left.join('<br>')) : ''}
    ${unclear ? item('Stock sales we couldn’t place, left out', `${unclear}. None follows an RSU vest or ESPP purchase in the same account. If they’re ESPP, add them to that account’s Added a year under Accounts; if RSUs, your unvested stock already covers them.`) : ''}
    ${yr && P.elapsed > 0 ? item(`${yr}: the rest of the year`, `Only the ${pct(1 - P.elapsed)} of ${yr} still ahead is projected; what’s past is already in your balances.`) : ''}
    ${item('Assumed, not from your data', `Investment growth, inflation, Social Security, retirement spending, the after-tax share of vests and the flat tax rates. Change any of them in Your numbers and Accounts.`)}
  </details>`
}
// Included assets by age: the plan as a line and area, the data-only plan dashed, retirement shaded,
// events (orange) and dated goals (purple) as markers.
function planChart(P, Dp, ra, v) {
  const W = 760, H = 270, L = 56, R = 752, T = 18, B = 240, y0 = P.years[0].age, y1 = P.years.at(-1).age
  const hi = Math.max(1, ...P.years.map(y => y.assets), ...(Dp ? Dp.years.map(y => y.assets) : [])) / 100 * 1.08, step = niceStep(hi, 4), top = Math.ceil(hi / step) * step
  const x = age => L + (R - L) * (age - y0) / Math.max(1, y1 - y0), y = c => B - (B - T) * (c / 100) / top
  const path = p => 'M' + p.years.map(r => `${f1(x(r.age))},${f1(y(r.assets))}`).join('L')
  let g = ''
  for (let val = 0; val <= top; val += step) g += `<line class="gridline" x1="${L}" x2="${R}" y1="${f1(y(val * 100))}" y2="${f1(y(val * 100))}"/><text x="${L - 8}" y="${f1(y(val * 100) + 4)}" text-anchor="end">${axis(val)}</text>`
  for (let a = Math.ceil(y0 / 5) * 5; a <= y1; a += 5) g += `<text x="${f1(x(a))}" y="${H - 8}" text-anchor="middle">${a}</text>`
  if (ra <= y1) g += `<rect x="${f1(x(Math.max(ra, y0)))}" y="${T - 8}" width="${f1(R - x(Math.max(ra, y0)))}" height="${B - T + 8}" style="fill:var(--panel2)"/>`
  g += `<path d="${path(P)}L${f1(x(y1))},${B}L${f1(x(y0))},${B}Z" style="fill:var(--income)" opacity=".14"/>`
  if (Dp) g += `<path d="${path(Dp)}" fill="none" style="stroke:var(--muted)" stroke-width="2" stroke-dasharray="6 5"/>`
  g += `<path d="${path(P)}" fill="none" style="stroke:var(--income)" stroke-width="2.5"/>`
  const marks = [...(v.events || []).map(e => [e.year - v.birth_year, e.name, 'var(--spend)']), ...(PL.goals || []).filter(gl => gl.target_date).map(gl => [+gl.target_date.slice(0, 4) - v.birth_year, gl.name, 'var(--cat-x2)'])]
    .filter(([age]) => age >= y0 && age <= y1)
  marks.forEach(([age, name, color], i) => { g += `<line x1="${f1(x(age))}" x2="${f1(x(age))}" y1="${T}" y2="${B}" style="stroke:${color}" stroke-dasharray="3 3"/><text x="${f1(x(age) + 4)}" y="${T + 12 + (i % 4) * 14}" style="fill:${color}">${esc(clip(name, 22))}</text>` })
  if (P.run_out) g += `<circle cx="${f1(x(P.run_out))}" cy="${B}" r="5" style="fill:var(--err)"><title>Runs out at ${P.run_out}</title></circle>`
  P.years.forEach(r => { g += `<rect x="${f1(x(r.age) - (R - L) / (y1 - y0 || 1) / 2)}" y="${T}" width="${f1((R - L) / (y1 - y0 || 1))}" height="${B - T}" fill="transparent"><title>${r.year}, age ${r.age}: ${usd0(r.assets)}${r.shortfall ? `, ${usd0(r.shortfall)} short` : ''}</title></rect>` })
  return `<svg class="pchart" viewBox="0 0 ${W} ${H}" role="img" aria-label="Included assets by age, ${P.run_out ? `running out at ${P.run_out}` : `lasting past ${y1}`}">${g}</svg>`
}
function goalsHTML(v) {
  const G = PL.goals || [], D = PL.data, f = PL.goal
  const row = gl => {
    const pace = gl.reached ? 'Reached' : gl.needed_monthly == null ? `${pct(gl.current / gl.target_amount)} there · no date set`
      : `${gl.pace_monthly >= gl.needed_monthly ? 'On pace' : 'Behind'}: ${usd0(gl.pace_monthly)} a month over the last 6 months, ${usd0(gl.needed_monthly)} needed`
    const cls = gl.reached || gl.needed_monthly == null ? '' : gl.pace_monthly >= gl.needed_monthly ? 'up' : 'behind'
    return `<div class="goal"><div><b>${esc(gl.name)}</b><br><small class="muted">${gl.target_months ? `${gl.target_months} months of expenses` : 'Target'}${gl.target_date ? ` · by ${mName(gl.target_date)}` : ''} · ${gl.accounts.map(l => `${esc(l.name)}${l.pct < 100 ? ` ${l.pct}%` : ''}`).join(', ') || 'no accounts linked'}</small></div>
      <div class="r"><b>${usd0(gl.current)}</b> <small class="muted">of ${usd0(gl.target_amount)}</small></div>
      <div class="gtrack"><i style="width:${f1(Math.min(1, gl.current / gl.target_amount) * 100)}%"></i></div>
      <div class="gpace ${cls}">${pace}</div><div class="r"><button class="linkbtn" data-act="goaledit" data-v="${gl.id}">Edit</button></div></div>`
  }
  const form = () => {
    const links = Object.fromEntries((f.accounts || []).map(l => [l.account_id, l.pct]))
    return `<form class="gform" data-planform="goal"><div class="frow" style="flex-wrap:wrap;align-items:end">
      <label class="fl">Name<input name="name" value="${esc(f.name || '')}" maxlength="80" required></label>
      <label class="fl">Target<select name="ttype"><option value="amount" ${f.target_months ? '' : 'selected'}>An amount</option><option value="months" ${f.target_months ? 'selected' : ''}>Months of expenses</option></select></label>
      <label class="fl">Amount, $ or months<input name="tval" type="number" min="1" value="${f.target_months ?? (f.target ? f.target / 100 : '')}" required></label>
      <label class="fl">By (optional)<input name="tdate" type="month" value="${f.target_date || ''}"></label></div>
      <p class="cap">Linked accounts and the share of each that counts toward this goal:</p>
      <div class="glinks">${D.baseline.accounts.filter(a => a.type === 'depository' || a.type === 'investment').map(a => `<label>${esc(a.name)}<span><input type="number" name="acct:${esc(a.id)}" min="0" max="100" value="${links[a.id] || ''}" placeholder="0">%</span></label>`).join('')}</div>
      ${PL.gErr ? `<p class="warn" role="alert">${esc(PL.gErr)}</p>` : ''}
      <div class="frow"><button class="btn">${f.id ? 'Save goal' : 'Add goal'}</button><button type="button" class="btn ghost" data-act="goalcancel">Cancel</button>${f.id ? `<button type="button" class="btn ghost" data-act="goaldel" data-v="${f.id}">Delete goal</button>` : ''}</div></form>`
  }
  return `<section class="panel" style="margin-top:16px"><div class="phead"><h2>Goals</h2>${f ? '' : '<button class="btn ghost sm" data-act="goalnew">Add a goal</button>'}</div>
    ${G.length ? G.map(row).join('') : f ? '' : '<p class="muted">No goals yet. A goal tracks money set aside for something, like an emergency fund or a down payment, against the balances of the accounts you link to it.</p>'}
    ${f ? form() : ''}
    <p class="cap">Goals don't change the forecast; dated ones are marked on the chart. A target in months of expenses follows the plan's spending.</p></section>`
}

/* ---------- Settings ---------- */
function settings() {
  const { sync } = state, conns = D.items
  const rules = D.rules
  return `${banner()}<div class="pagehead"><h1>Settings</h1></div>
  <section class="panel"><div class="phead"><h2>Connections</h2>
    <div class="frow" style="margin:0">${sync.busy ? `<button class="btn" disabled>${icon('sync', 18, ' spin')}Syncing</button><span id="syncstatus" class="muted" role="status">${syncStatus()}</span>` : `<button class="btn" data-act="sync">${icon('sync', 18)}Sync now</button>`}${sync.msg ? `<span class="ok" role="status">${icon('check', 16)}${esc(sync.msg)}</span>` : ''}${sync.err ? `<span class="warn" role="status">${icon('alert', 16)}${esc(sync.err)}</span>` : ''}</div></div>
   ${conns.length ? conns.map(c => `<div class="conn"><div class="l1"><span class="nm">SimpleFIN</span></div>
      <div class="ins">${c.institutions.length ? c.institutions.map(esc).join(', ') : 'No accounts yet'}</div>
      <div class="meta">Configured via SIMPLEFIN_ACCESS_URL · ${c.accounts} account${c.accounts === 1 ? '' : 's'} · Last sync ${rel(c.last_synced_at)} · ${c.requests_24h} of ${c.request_limit} requests used in the last 24 hours</div>
      ${c.last_error ? `<div class="cerr">${icon('alert', 18)}<div>${esc(c.last_error)}</div></div>` : ''}</div>`).join('') : D.config.simplefin_configured ? '<p class="muted" style="padding:10px 0">SimpleFIN is configured. Click Sync now to bring in accounts.</p>' : '<p class="muted" style="padding:10px 0">SimpleFIN is not configured. Set the <code>SIMPLEFIN_ACCESS_URL</code> environment variable and restart. The README explains how to get the access URL from SimpleFIN Bridge.</p>'}</section>
  <section class="panel sec"><h2>Rules</h2><p class="muted" style="font-size:13.5px">When a merchant or description contains the pattern, the transaction gets the category. Your own choices on a transaction always win.</p>
   ${state.rulesErr ? `<p class="warn" style="padding:8px 0">${esc(state.rulesErr)} <button class="linkbtn" data-act="rulesretry">Retry</button></p>` : !rules ? '<p class="muted" style="padding:10px 0" role="status">Loading…</p>' : rules.length ? rules.map(r => `<div class="rule"><span class="pat">${esc(r.pattern)}</span><span class="go">${icon('arrow', 14)}</span><span class="rc"><span class="dot" style="background:${catColor(r.category)}"></span>${esc(catName(r.category))}</span>
      <span class="end">${state.ruleDel === r.id ? `<span>Delete this rule?</span><button class="btn ghost sm" data-act="ruleCancel">Cancel</button><button class="btn danger sm" data-act="ruleOk" data-id="${r.id}">Yes, delete</button>` : `<button class="btn ghost sm" data-act="rule1" data-id="${r.id}">Delete</button>`}</span></div>`).join('') : '<p class="muted" style="padding:10px 0">No rules yet. Tick “Always use this” when you categorize a transaction to add one.</p>'}</section>
  ${D.me?.auth ? `<section class="panel sec"><h2>Account</h2><div class="frow" style="margin:0"><span>Signed in as <strong>${esc(D.me.email)}</strong></span><button class="btn ghost" data-act="signout">Sign out</button></div></section>` : ''}
  ${backupsSection()}
  <section class="panel sec"><h2>Merchants</h2><p class="muted" style="font-size:13.5px">Rename merchants and merge duplicates, like a store that shows up under several names.</p><div class="frow"><a class="btn ghost" href="#merchants">Manage merchants ${icon('arrow', 16)}</a></div></section>
  <section class="panel sec"><h2>Smart suggestions (Jev)</h2><p class="muted" style="font-size:13.5px"><strong>${D.config.jev_enabled ? 'On' : 'Off'}</strong>${D.config.jev_enabled ? ` · ${D.config.jev_merchants} merchant${D.config.jev_merchants === 1 ? '' : 's'} scored and saved` : ''}. ${jevUsageLine()}${D.config.jev_enabled ? 'Merchants and accounts the local rules cannot place are sent to Jev: name, bank descriptions, typical amount, and income or spending; for accounts, institution, name, and balance sign; for alerts, the charge’s description, amount and reasons, card or bank account, your usual state and top categories.' : 'Set <code>JEV_API_KEY</code> and restart to turn it on. Nothing is sent to Jev while it is off.'}</p></section>
  <section class="panel sec"><h2>Fund lookups</h2><p class="muted" style="font-size:13.5px"><strong>${D.config.fund_lookups ? 'On' : 'Off'}</strong>. ${D.config.fund_lookups ? 'After each sync, held fund tickers with no recent split are looked up in the SEC’s public fund filings to split them into stocks, bonds and cash. Only the tickers are sent.' : 'Set <code>SEC_USER_AGENT</code> (your name and email, which the SEC requires) and restart to classify funds from their SEC filings. Until then, set a split for each fund on the Investments page.'}</p></section>
  <section class="panel sec"><h2>Appearance</h2><div class="seg themeseg" role="group" aria-label="Theme">${['system', 'light', 'dark'].map(t => `<button data-act="themeset" data-v="${t}" aria-pressed="${state.theme === t}">${t[0].toUpperCase() + t.slice(1)}</button>`).join('')}</div></section>`
}
// Jev usage in Settings: requests and tokens for the last 30 days and all time, with the estimated cost (never masked by privacy mode).
function jevUsageLine() {
  const u = D.config.jev_usage; if (!u || !u.all_time.requests) return ''
  const n = x => x.toLocaleString('en-US'), cost = c => c == null ? '' : `, about $${c < 0.01 ? c.toFixed(4) : c.toFixed(2)}`
  const part = (label, p) => `${label}: ${n(p.requests)} request${p.requests === 1 ? '' : 's'}, ${n(p.input_tokens)} input + ${n(p.output_tokens)} output tokens${cost(p.cost)}`
  return `<br>${part('Last 30 days', u.last_30_days)}. ${part('All time', u.all_time)}. `
}
async function loadRules() {
  state.rulesErr = ''
  try { D.rules = await api('/rules') } catch (e) { state.rulesErr = e.message }
  if (state.view === 'settings') render()
}
// syncStatus: the running sync's step from GET /sync, e.g. "Saving accounts 5 of 14 (36%)", with a bar and elapsed time.
function syncStatus() {
  const p = state.sync.progress
  if (!p?.running) return 'Starting…'
  const secs = Math.max(0, Math.round((Date.now() - Date.parse(p.started_at)) / 1000))
  const count = p.total ? ` ${p.done} of ${p.total} (${Math.round(100 * p.done / p.total)}%)` : '…'
  return `${esc(p.stage)}${count} <progress ${p.total ? `value="${p.done}" max="${p.total}"` : ''} style="vertical-align:middle"></progress> ${secs}s`
}
async function doSync() {
  state.sync = { busy: true }; render()
  const poll = setInterval(async () => {
    try { state.sync.progress = await api('/sync') } catch { /* keep the last step shown */ }
    const el = $('#syncstatus')
    if (el && state.sync.busy) el.innerHTML = syncStatus()
  }, 500)
  try { await send('POST', '/sync'); state.sync = { msg: 'Synced just now' } } catch (e) { state.sync = { err: e.message } }
  clearInterval(poll)
  await reload()
}

/* ---------- Settings: Backups ---------- */
// bk.data: last GET /backups. bk.confirm: {name} or {file} while the typed confirmation is open. bk.phase: '' | 'backup' | 'restore'.
const bk = { data: null, err: '', phase: '', msg: '', fail: '', confirm: null }
let bkFile = null
const bkSize = n => n >= 1048576 ? (n / 1048576).toFixed(1) + ' MB' : Math.max(1, Math.round(n / 1024)) + ' KB'
const bkWhen = iso => new Date(iso).toLocaleString('en-US', { month: 'short', day: 'numeric', year: 'numeric', hour: 'numeric', minute: '2-digit' })
function backupsSection() {
  const d = bk.data, busy = !!bk.phase, c = bk.confirm
  const warns = d ? [
    !d.writable && `The backup folder is not writable (${d.write_error}). New backups will fail.`,
    d.stale && (d.last_backup ? `The last successful backup was ${rel(d.last_backup)}. Backups should run every night.` : 'No backup has completed yet, though the server has been running for over a day.'),
    d.last_error && `The last backup failed: ${d.last_error}`,
  ].filter(Boolean) : []
  const rows = !d ? (bk.err ? `<p class="warn" style="padding:8px 0">${esc(bk.err)} <button class="linkbtn" data-act="bkload">Retry</button></p>` : '<p class="muted" style="padding:10px 0" role="status">Loading…</p>')
    : d.backups.length ? d.backups.map(b => `<div class="bkrow"><span class="when">${esc(bkWhen(b.created_at))}</span><span class="meta">${bkSize(b.size)}${b.name.includes('-pre-restore') ? ' · before a restore' : ''}</span>
      <button class="btn ghost sm" data-act="bkask" data-name="${esc(b.name)}" ${busy ? 'disabled' : ''}>Restore</button></div>`).join('')
    : '<p class="muted" style="padding:10px 0">No backups yet</p>'
  const what = c && (c.file ? `the uploaded file “${esc(c.file.name)}”` : `the backup from ${esc(bkWhen(d.backups.find(b => b.name === c.name)?.created_at))}`)
  const confirm = c ? `<div class="bkconf" role="group" aria-label="Confirm restore"><p><strong>This replaces all current data with ${what}.</strong> A safety backup of the current data is taken first. If anything fails, nothing changes.</p>
    <div class="frow" style="margin:0"><label class="sr" for="bkconfirm">Type restore to confirm</label><input class="field" id="bkconfirm" placeholder="Type restore to confirm" autocomplete="off" style="max-width:240px">
    <button class="btn danger" id="bkgo" data-act="bkgo" disabled>Restore</button><button class="btn ghost" data-act="bkcancel">Cancel</button></div></div>` : ''
  const status = bk.phase === 'restore' ? `<span class="ok" role="status">${icon('sync', 16, ' spin')}Restoring. Keep this page open.</span>`
    : bk.msg ? `<span class="ok" role="status">${icon('check', 16)}${esc(bk.msg)}</span>` : bk.fail ? `<span class="warn" role="status">${icon('alert', 16)}${esc(bk.fail)}</span>` : ''
  return `<section class="panel sec"><div class="phead"><h2>Backups</h2>
    <div class="frow" style="margin:0"><button class="btn" data-act="bknow" ${busy ? 'disabled' : ''}>${bk.phase === 'backup' ? `${icon('sync', 18, ' spin')}Backing up` : 'Back up now'}</button>
    <button class="btn ghost" data-act="bkpick" ${busy ? 'disabled' : ''}>Upload and restore</button><input class="sr" id="bkfile" type="file" accept=".gz,.tgz,application/gzip" tabindex="-1" aria-hidden="true"></div></div>
   <p class="muted" style="font-size:13.5px">${d ? `Last backup ${d.last_backup ? rel(d.last_backup) : 'never'}.${d.next_run ? ` Next one ${esc(bkWhen(d.next_run))}.` : ''}` : ''}</p>
   ${warns.map(w => `<div class="cerr" role="status">${icon('alert', 18)}<div>${esc(w)}</div></div>`).join('')}
   <div class="frow" style="margin:10px 0 0">${status}</div>${confirm}${rows}</section>`
}
async function loadBackups() {
  bk.err = ''
  try { bk.data = await api('/backups') } catch (e) { bk.err = e.message }
  if (state.view === 'settings') render()
}
async function backupNow() {
  bk.phase = 'backup'; bk.msg = bk.fail = ''; render()
  try { await send('POST', '/backups'); bk.msg = 'Backup saved.' } catch (e) { bk.fail = e.message }
  bk.phase = ''
  await loadBackups()
}
async function restoreNow() {
  const c = bk.confirm
  bk.phase = 'restore'; bk.msg = bk.fail = ''; bk.confirm = null; render()
  try {
    if (c.file) await api('/backups/restore', { method: 'POST', body: c.file, headers: { 'Content-Type': 'application/gzip', 'X-Requested-With': 'money-tracker' } })
    else await send('POST', `/backups/${encodeURIComponent(c.name)}/restore`)
    bk.msg = 'Restored. A safety backup of the previous data was saved.'
    cache.clear(); D.rules = null; state.perr = {}; state.loaded = false
    await reload()
  } catch (e) { bk.fail = `Restore failed: ${e.message}. Nothing was changed.` }
  bk.phase = ''; bk.data = null
  await loadBackups()
}
document.addEventListener('click', e => {
  const el = e.target.closest('[data-act]'), a = el?.dataset.act
  if (a === 'bknow') backupNow()
  else if (a === 'bkload') { bk.data = null; loadBackups() }
  else if (a === 'bkask') { bk.confirm = { name: el.dataset.name }; bk.msg = bk.fail = ''; render(); $('#bkconfirm')?.focus() }
  else if (a === 'bkcancel') { bk.confirm = null; render() }
  else if (a === 'bkgo') restoreNow()
  else if (a === 'bkpick') $('#bkfile')?.click()
})
document.addEventListener('change', e => {
  if (e.target.id !== 'bkfile' || !e.target.files[0]) return
  bk.confirm = { file: e.target.files[0] }; bk.msg = bk.fail = ''; render(); $('#bkconfirm')?.focus()
})
document.addEventListener('input', e => { if (e.target.id === 'kn-retire_age') $('#ra-v').textContent = e.target.value })
document.addEventListener('toggle', e => { const sec = e.target.matches?.('details.psec') && e.target.dataset.sec; if (sec) (PL.open ||= {})[sec] = e.target.open }, true)
document.addEventListener('input', e => { if (e.target.id === 'bkconfirm') $('#bkgo').disabled = e.target.value.trim().toLowerCase() !== 'restore' })

/* ---------- Toast (shared, lives outside #main so re-renders never remove it) ---------- */
const toastEl = document.createElement('div')
toastEl.className = 'toast'; toastEl.setAttribute('role', 'status'); toastEl.hidden = true
document.body.append(toastEl)
let toastTimer, toastUndo = null
function hideToast() { clearTimeout(toastTimer); toastEl.hidden = true; toastUndo = null }
function toast(text, { undo, err } = {}) {
  clearTimeout(toastTimer); toastUndo = undo || null
  toastEl.className = 'toast' + (err ? ' err' : '')
  toastEl.innerHTML = `<span>${esc(text)}</span>${undo ? '<button class="btn ghost sm" data-act="toastundo">Undo</button>' : ''}<button class="tclose" data-act="toastx" aria-label="Dismiss">${icon('x', 14)}</button>`
  toastEl.hidden = false
  toastTimer = setTimeout(hideToast, undo ? 12000 : 6000)
}
toastEl.addEventListener('pointerenter', () => clearTimeout(toastTimer))
toastEl.addEventListener('focusin', () => clearTimeout(toastTimer))

/* ---------- Review inbox (#review): uncategorized spending grouped by merchant ---------- */
const R = { loaded: false, busy: false, err: '', sugErr: false, groups: [], done: 0, seq: 0 }
const rvConf = c => c >= 0.75 ? 'Likely' : 'Maybe'
const rvAmt = c => (c > 0 ? '+' : '') + usd2(c)
const rvGroup = id => R.groups.find(g => g.id === id)
async function loadReview() {
  const seq = ++R.seq
  if (!R.loaded) { R.busy = true; R.err = ''; paintReview() }
  try {
    const rows = []
    for (let total = 1; rows.length < total;) {
      const r = await api(`/transactions?uncategorized=1&limit=1000&offset=${rows.length}`, null, true)
      if (!r.data.length) break
      rows.push(...r.data); total = r.total
    }
    const [sugs, mers] = await Promise.all([api('/suggestions').catch(() => null), api('/merchants').catch(() => [])])
    if (seq !== R.seq) return
    const sugBy = new Map((sugs || []).map(s => [s.display_name, s])), keyBy = new Map(mers.filter(m => !m.merged_into).map(m => [m.display_name, m.key]))
    const by = new Map()
    for (const t of rows) {
      const name = t.merchant || t.name
      if (!by.has(name)) by.set(name, { name, rows: [], total: 0 })
      const g = by.get(name); g.rows.push(t); g.total += t.amount
    }
    const old = new Map(R.groups.map(g => [g.name, g]))
    R.groups = [...by.values()].sort((a, b) => Math.abs(b.total) - Math.abs(a.total)).map((g, i) => {
      const sug = sugBy.get(g.name) || null, prev = old.get(g.name)
      return { ...g, id: i, sug, key: sug?.merchant || keyBy.get(g.name) || '', cat: prev?.cat ?? sug?.category ?? '', remember: prev?.remember ?? true, open: prev?.open ?? false, newing: false, busy: false, err: '' }
    })
    R.sugErr = sugs === null; R.err = ''; R.loaded = true
    api('/config').then(c => { D.config = c }).catch(() => {}) // Jev's saved-merchant count on Settings
  } catch (e) { if (seq !== R.seq) return; R.err = e.message }
  R.busy = false
  paintReview()
}
function review() {
  return `${banner()}<div class="pagehead"><h1>Review</h1><span class="muted" id="rvcount"></span>${D.config.jev_enabled ? `<button class="btn ghost sm" data-act="rvrejev" style="margin-left:auto" title="Jev's answers are saved per merchant. This forgets them and asks again, e.g. after you add categories.">Ask Jev again</button>` : ''}</div>
  <p class="notecap">Uncategorized spending across all months, grouped by merchant, largest first. Accepting a merchant categorizes every one of its uncategorized transactions. Transfers and pending items are left out.</p>
  <div id="rvroot"></div>`
}
function rvCounts() {
  const n = R.groups.reduce((s, g) => s + g.rows.length, 0)
  return `${R.groups.length} merchant${R.groups.length === 1 ? '' : 's'} · ${n} transaction${n === 1 ? '' : 's'}`
}
function paintReview() {
  const root = $('#rvroot'); if (!root) return
  const head = $('#rvcount'); if (head) head.textContent = R.loaded && R.groups.length ? rvCounts() : ''
  if (R.err) { root.innerHTML = `<div class="list"><div class="empty"><p><strong>Could not load the inbox</strong></p><p>${esc(R.err)}</p><p><button class="btn ghost" data-act="rvretry">Retry</button></p></div></div>`; return }
  if (!R.loaded) { root.innerHTML = '<div class="load" role="status">Loading…</div>'; return }
  if (!R.groups.length) {
    root.innerHTML = `<div class="list"><div class="empty caught"><h2 id="rvdone" tabindex="-1">All caught up</h2><p>${R.done ? `${R.done} transaction${R.done === 1 ? '' : 's'} categorized this session.` : 'Every transaction has a category.'}</p>
      <p class="rvlinks"><a class="btn ghost" href="#transactions">Open Transactions</a><a class="btn ghost" href="#merchants">Manage merchants</a></p></div></div>`
    return
  }
  root.innerHTML = `${R.sugErr ? '<p class="notecap warn2">Suggestions are unavailable right now. Pick a category for each merchant.</p>' : ''}<ul class="glist" aria-label="Merchants to review">${R.groups.map(groupHTML).join('')}</ul>`
}
function rvOptions(g) {
  const names = [...new Set([...D.cats, g.sug?.category, g.cat].filter(Boolean))]
  return `<option value="" selected>${g.cat ? 'Change category' : 'Choose category'}</option>${names.map(c => `<option value="${esc(c)}">${esc(catName(c))}</option>`).join('')}<option value="__new">New category…</option>`
}
function groupHTML(g) {
  const n = g.rows.length, sug = g.sug
  const chosen = g.cat, changed = sug && chosen && chosen !== sug.category
  const chip = chosen ? `<span class="chip"><span class="dot" style="background:${catColor(chosen)}"></span>${esc(catName(chosen))}</span>` : '<span class="chip none">No category yet</span>'
  let why
  if (chosen && sug && !changed) why = `<b class="conf ${sug.confidence >= 0.75 ? 'hi' : 'lo'}">${rvConf(sug.confidence)}</b><span class="reason">${esc(sug.reason)}</span>`
  else if (changed) why = `<span class="reason">Your choice. The suggestion was ${esc(catName(sug.category))}.</span>`
  else if (chosen) why = '<span class="reason">Your choice.</span>'
  else why = `<span class="reason">${sug ? '' : 'No suggestion for this merchant.'}</span>`
  const picker = g.newing
    ? `<input class="fsel" id="rvnew${g.id}" maxlength="60" placeholder="New category name" autocomplete="off" aria-label="New category name for ${esc(g.name)}" data-rvnew="${g.id}"><button class="linkbtn" data-act="rvnewcancel" data-g="${g.id}">Cancel</button>`
    : `<select class="fsel" data-rvpick="${g.id}" aria-label="Category for ${esc(g.name)}">${rvOptions(g)}</select>`
  const label = sug && !changed ? 'Accept' : 'Apply'
  const tx = g.open ? `<div class="gtx">${g.rows.map(t => { const a = acct[t.account_id]; return `<div class="gt"><span class="d">${dShort(t.date)}</span><span class="w">${esc((t.institution || t.account) + (a?.mask ? ' ••' + a.mask : ''))}</span><span class="amt">${rvAmt(t.amount)}</span></div>` }).join('')}</div>` : ''
  return `<li class="gwrap" data-g="${g.id}"><div class="gin"><article class="grp" tabindex="0" data-g="${g.id}" aria-label="${esc(g.name)}, ${n} transaction${n === 1 ? '' : 's'}, ${rvAmt(g.total)}${chosen ? `, category ${esc(catName(chosen))}. Press Enter to ${label.toLowerCase()}` : ''}">
    <div class="gmain"><h3 class="gname">${esc(g.name)}</h3><p class="gmeta">${n} transaction${n === 1 ? '' : 's'} · ${rvAmt(g.total)}</p>
      <button class="linkbtn gexp" data-act="rvexp" data-g="${g.id}" aria-expanded="${g.open}">${g.open ? 'Hide' : 'Show'} transactions ${icon(g.open ? 'chevD' : 'chevR', 14)}</button></div>
    <div class="gsug">${chip}<div class="gwhy">${why}</div></div>
    <div class="gact"><div class="gpick">${picker}<button class="btn" data-act="rvaccept" data-g="${g.id}" ${g.busy ? 'disabled' : ''}>${g.busy ? 'Saving' : label}</button></div>
      <label class="always"><input type="checkbox" data-rvrem="${g.id}" ${g.remember ? 'checked' : ''}> Remember for future ${esc(g.name)} transactions</label>
      ${g.err ? `<p class="rowmsg" role="alert">${esc(g.err)}</p>` : ''}</div>
    ${tx}</article></div></li>`
}
function repaintGroup(g, focus) {
  const li = $(`.gwrap[data-g="${g.id}"]`); if (!li) return
  li.outerHTML = groupHTML(g)
  if (focus) $(`.gwrap[data-g="${g.id}"] ${focus}`)?.focus()
}
async function rvAccept(id) {
  const g = rvGroup(id); if (!g || g.busy) return
  const cat = (g.newing ? $(`#rvnew${id}`)?.value : g.cat)?.trim()
  if (!cat) { g.err = 'Choose a category first.'; repaintGroup(g, '.fsel'); return }
  g.cat = cat; g.newing = false; g.busy = true; g.err = ''; repaintGroup(g, '.grp')
  const remember = g.remember && !!g.key
  try {
    if (g.key) await send('POST', '/suggestions/accept', { merchant: g.key, category: cat, remember: g.remember })
    else for (const t of g.rows) await send('PATCH', '/transactions/' + encodeURIComponent(t.id), { category: cat })
  } catch (e) { g.busy = false; g.err = e.message; repaintGroup(g, '.grp'); return }
  R.done += g.rows.length
  const u = { key: g.key, remember, ids: g.rows.map(t => t.id) }
  const li = $(`.gwrap[data-g="${id}"]`), next = li?.nextElementSibling || li?.previousElementSibling
  R.groups = R.groups.filter(x => x !== g)
  li?.classList.add('leaving')
  setTimeout(() => { li?.remove(); if (!R.groups.length) paintReview(); const h = $('#rvcount'); if (h) h.textContent = R.groups.length ? rvCounts() : ''; ($('#rvdone') || $('.grp'))?.focus() }, reduce ? 0 : 240)
  if (next) next.querySelector('.grp')?.focus({ preventScroll: false })
  const h = $('#rvcount'); if (h) h.textContent = R.groups.length ? rvCounts() : ''
  cache.clear(); state.perr = {}; T.rows = []
  api('/categories').then(c => { D.cats = c }).catch(() => {})
  const n = u.ids.length
  toast(`${n} ${g.name} transaction${n === 1 ? '' : 's'} set to ${catName(cat)}${remember ? '. Future ones too.' : '.'}`, { undo: () => rvUndo(u) })
}
async function rvUndo(u) {
  try {
    if (u.remember) await send('POST', '/suggestions/accept', { merchant: u.key, category: '', remember: true })
    else for (const id of u.ids) await send('PATCH', '/transactions/' + encodeURIComponent(id), { category: '' })
  } catch (e) { toast(`Could not undo: ${e.message}`, { err: true }); return }
  R.done = Math.max(0, R.done - u.ids.length); cache.clear(); state.perr = {}
  toast('Put back in the inbox.')
  if (state.view === 'review') loadReview()
}

/* ---------- Merchants (#merchants): search, rename, merge ---------- */
const M = { loaded: false, err: '', list: [], q: '', sort: 'count', sel: new Set(), editing: null, editErr: '', mg: null, seq: 0 }
async function loadMerchants() {
  const seq = ++M.seq
  try { const l = await api('/merchants'); if (seq !== M.seq) return; M.list = l; M.loaded = true; M.err = '' } catch (e) { if (seq !== M.seq) return; M.err = e.message }
  paintMerchants()
}
function merchants() {
  return `${banner()}<div class="pagehead"><h1>Merchants</h1><span class="muted" id="mcount"></span></div>
  <p class="notecap">A merchant is one place you spend. Rename it, or merge duplicates so they count and categorize together. <a href="#settings">Back to Settings</a></p>
  <div class="mtools"><div class="sbox">${icon('search', 18)}<input type="search" id="mq" placeholder="Search merchants" aria-label="Search merchants" value="${esc(M.q)}" class="fsel wide"></div>
   <div class="seg" role="group" aria-label="Sort">${[['count', 'Most transactions'], ['name', 'A to Z']].map(([v, l]) => `<button data-act="msort" data-v="${v}" aria-pressed="${M.sort === v}">${l}</button>`).join('')}</div></div>
  <div id="msel"></div><div id="mroot"></div><dialog id="mdlg" aria-labelledby="mdt"></dialog>`
}
const mRoots = () => M.list.filter(m => !m.merged_into)
const mKids = key => M.list.filter(m => m.merged_into === key)
function mRow(m) {
  const kids = mKids(m.key), n = m.transactions, ed = M.editing === m.key
  const name = ed
    ? `<form class="mren" data-mren="${esc(m.key)}"><input class="fsel" id="mrn" maxlength="60" value="${esc(m.display_name)}" aria-label="New name for ${esc(m.display_name)}"><button class="btn sm" type="submit">Save</button><button class="linkbtn" type="button" data-act="mrencancel">Cancel</button>${M.editErr ? `<span class="warn" role="alert">${esc(M.editErr)}</span>` : ''}</form>`
    : `<span class="mn">${esc(m.display_name)}</span>${m.category ? `<span class="chip sm"><span class="dot" style="background:${catColor(m.category)}"></span>${esc(catName(m.category))}</span>` : ''}`
  return `<li class="mrow${M.sel.has(m.key) ? ' picked' : ''}" data-mk="${esc(m.key)}"><div class="mline">
    <input type="checkbox" class="mchk" data-mchk="${esc(m.key)}" ${M.sel.has(m.key) ? 'checked' : ''} aria-label="Select ${esc(m.display_name)} for merging">
    <div class="mname">${name}</div><span class="mct">${n} transaction${n === 1 ? '' : 's'}</span>
    ${ed ? '' : `<button class="btn ghost sm" data-act="mren" data-k="${esc(m.key)}" aria-label="Rename ${esc(m.display_name)}">${icon('edit', 14)}Rename</button>`}</div>
    ${kids.length ? `<ul class="mkids" aria-label="Merged into ${esc(m.display_name)}">${kids.map(k => `<li><span class="tag">Merged</span>${esc(k.display_name)}</li>`).join('')}</ul>` : ''}</li>`
}
function paintMerchants() {
  const root = $('#mroot'); if (!root) return
  if (M.err) { root.innerHTML = `<div class="list"><div class="empty"><p><strong>Could not load merchants</strong></p><p>${esc(M.err)}</p><p><button class="btn ghost" data-act="mretry">Retry</button></p></div></div>`; return }
  if (!M.loaded) { root.innerHTML = '<div class="load" role="status">Loading…</div>'; return }
  const q = M.q.trim().toLowerCase()
  let rows = mRoots().filter(m => !q || m.display_name.toLowerCase().includes(q) || m.key.includes(q) || mKids(m.key).some(k => k.display_name.toLowerCase().includes(q)))
  rows = rows.sort((a, b) => M.sort === 'name' ? a.display_name.localeCompare(b.display_name) : b.transactions - a.transactions || a.display_name.localeCompare(b.display_name))
  const c = $('#mcount'); if (c) c.textContent = `${rows.length}${q ? ' of ' + mRoots().length : ''} merchant${rows.length === 1 ? '' : 's'}`
  root.innerHTML = rows.length ? `<ul class="mlist">${rows.map(mRow).join('')}</ul>` : `<div class="list"><div class="empty"><p><strong>${M.list.length ? 'No merchants match' : 'No merchants yet'}</strong></p><p>${M.list.length ? 'Try a different search.' : 'They appear after the first sync.'}</p></div></div>`
  paintMSel()
}
function paintMSel() {
  const box = $('#msel'); if (!box) return
  const n = M.sel.size
  box.innerHTML = n ? `<div class="selbar" role="region" aria-label="Selection"><span><strong>${n}</strong> selected</span>${n > 1 ? `<button class="btn" data-act="mmerge">Merge into…</button>` : '<span class="muted">Select one more to merge</span>'}<button class="linkbtn" data-act="mclear">Clear</button></div>` : ''
}
function mgTarget() { return M.mg && mRoots().find(m => m.key === M.mg.target) }
function paintMerge() {
  const dlg = $('#mdlg'); if (!dlg || !M.mg) return
  const sel = mRoots().filter(m => M.sel.has(m.key)), t = mgTarget(), others = sel.filter(m => m !== t)
  const cnt = sel.reduce((s, m) => s + m.transactions, 0), list = a => a.length < 3 ? a.map(m => `“${esc(m.display_name)}”`).join(' and ') : `${a.slice(0, -1).map(m => `“${esc(m.display_name)}”`).join(', ')} and “${esc(a.at(-1).display_name)}”`
  dlg.innerHTML = `<form method="dialog" class="mdform"><h2 id="mdt">Merge merchants</h2>
    <fieldset><legend>Keep this name</legend>${sel.map(m => `<label class="mopt"><input type="radio" name="mtarget" value="${esc(m.key)}" ${m === t ? 'checked' : ''}><span>${esc(m.display_name)}</span><span class="muted">${m.transactions} transaction${m.transactions === 1 ? '' : 's'}</span></label>`).join('')}</fieldset>
    <p class="mgsum">${list(others)} will be merged into “${esc(t.display_name)}”. All ${cnt} transactions will show as “${esc(t.display_name)}” everywhere, including past ones, and be categorized together.</p>
    <p class="muted mgnote">There is no unmerge, but you can rename the merchant afterwards.</p>
    ${M.mg.err ? `<p class="warn" role="alert">${esc(M.mg.err)}</p>` : ''}
    <div class="mact"><button class="btn ghost" type="button" data-act="mgcancel">Cancel</button><button class="btn" type="button" data-act="mgok" ${M.mg.busy ? 'disabled' : ''}>${M.mg.busy ? 'Merging' : `Merge into ${esc(clip(t.display_name, 24))}`}</button></div></form>`
}
function openMerge() {
  const sel = mRoots().filter(m => M.sel.has(m.key)).sort((a, b) => b.transactions - a.transactions)
  if (sel.length < 2) return
  M.mg = { target: sel[0].key, err: '', busy: false }
  paintMerge(); const d = $('#mdlg'); d.addEventListener('close', () => { M.mg = null }, { once: true }); d.showModal()
}
async function doMerge() {
  const t = mgTarget(); if (!t || M.mg.busy) return
  const sources = mRoots().filter(m => M.sel.has(m.key) && m !== t).map(m => m.key)
  M.mg.busy = true; M.mg.err = ''; paintMerge()
  try { await send('POST', '/merchants/merge', { target: t.key, sources }) } catch (e) { M.mg.busy = false; M.mg.err = e.message; paintMerge(); return }
  $('#mdlg').close()
  M.sel.clear(); cache.clear(); state.perr = {}; R.loaded = false
  toast(`Merged ${sources.length} into “${t.display_name}”.`)
  await loadMerchants()
}
async function renameMerchant(key, name) {
  name = name.trim()
  if (!name) { M.editErr = 'Enter a name.'; paintMerchants(); $('#mrn')?.focus(); return }
  try { await send('PATCH', '/merchants/' + encodeURIComponent(key), { display_name: name }) } catch (e) { M.editErr = e.message; paintMerchants(); $('#mrn')?.focus(); return }
  M.list.find(m => m.key === key).display_name = name; M.editing = null; M.editErr = ''; cache.clear(); R.loaded = false
  paintMerchants(); $(`.mrow[data-mk="${CSS.escape(key)}"] [data-act="mren"]`)?.focus()
  toast(`Renamed to “${name}”.`)
}

/* Listeners for the review and merchants views (kept apart from the shared handlers above). */
document.addEventListener('click', e => {
  const el = e.target.closest('[data-act]'); if (!el) return
  const a = el.dataset.act, id = +el.dataset.g
  if (a === 'toastundo') { const u = toastUndo; hideToast(); u?.() }
  else if (a === 'toastx') hideToast()
  else if (a === 'rvaccept') rvAccept(id)
  else if (a === 'rvexp') { const g = rvGroup(id); g.open = !g.open; repaintGroup(g, '.gexp') }
  else if (a === 'rvnewcancel') { const g = rvGroup(id); g.newing = false; repaintGroup(g, '.fsel') }
  else if (a === 'rvretry') { R.err = ''; loadReview() }
  else if (a === 'rvrejev') { el.disabled = true; send('DELETE', '/suggestions/jev').then(() => api('/config')).then(c => { D.config = c; R.loaded = false; loadReview() }, err => { R.err = err.message; paintReview() }) }
  else if (a === 'newsave') { const v = $('#newcat')?.value.trim(); if (v) applyCat(el.dataset.id, v, $('#always')?.checked); else $('#newcat')?.focus() }
  else if (a === 'msort') { M.sort = el.dataset.v; document.querySelectorAll('[data-act="msort"]').forEach(b => b.setAttribute('aria-pressed', b === el)); paintMerchants() }
  else if (a === 'mren') { M.editing = el.dataset.k; M.editErr = ''; paintMerchants(); const i = $('#mrn'); i?.focus(); i?.select() }
  else if (a === 'mrencancel') { const k = M.editing; M.editing = null; M.editErr = ''; paintMerchants(); $(`.mrow[data-mk="${CSS.escape(k)}"] [data-act="mren"]`)?.focus() }
  else if (a === 'mclear') { M.sel.clear(); paintMerchants() }
  else if (a === 'mmerge') openMerge()
  else if (a === 'mgcancel') $('#mdlg').close()
  else if (a === 'mgok') doMerge()
  else if (a === 'mretry') { M.err = ''; loadMerchants() }
})
document.addEventListener('change', e => {
  const t = e.target
  if (t.dataset.rvpick !== undefined) {
    const g = rvGroup(+t.dataset.rvpick)
    if (t.value === '__new') { g.newing = true; repaintGroup(g, '.fsel') } else if (t.value) { g.cat = t.value; g.err = ''; repaintGroup(g, '.fsel') }
  } else if (t.dataset.rvrem !== undefined) rvGroup(+t.dataset.rvrem).remember = t.checked
  else if (t.dataset.mchk !== undefined) { t.checked ? M.sel.add(t.dataset.mchk) : M.sel.delete(t.dataset.mchk); t.closest('.mrow').classList.toggle('picked', t.checked); paintMSel() }
  else if (t.name === 'mtarget') { M.mg.target = t.value; paintMerge(); $('#mdlg input:checked')?.focus() }
})
document.addEventListener('input', e => { if (e.target.id === 'mq') { M.q = e.target.value; paintMerchants() } })
document.addEventListener('toggle', e => { const id = e.target.matches?.('details.hold') && e.target.dataset.id; if (id) e.target.open ? INV.open.add(id) : INV.open.delete(id) }, true)
document.addEventListener('submit', e => {
  const f = e.target
  const pf = f.dataset.planform
  if (pf) {
    e.preventDefault()
    const el = n => f.elements[n]?.value ?? ''
    if (pf === 'setup') send('PUT', '/plan', { birth_year: +$('#birthyear').value }).then(p => { setPlan(p); PL.err = ''; return api('/goals').then(g => { PL.goals = g }) }, er => { PL.err = er.message }).finally(render)
    else if (pf === 'event') {
      const ev = { kind: el('kind'), name: el('name').trim(), year: +el('year'), amount: Math.round(+el('amount') * 100) }
      if (el('until') && ev.kind !== 'expense' && ev.kind !== 'income') ev.until = +el('until')
      const list = PL.vals.events ||= []
      if (PL.ev.editing != null) list[PL.ev.editing] = ev; else list.push(ev)
      list.sort((x, y) => x.year - y.year)
      PL.ev = blankEv(); planChanged()
    } else if (pf === 'goal') {
      const months = el('ttype') === 'months', body = { name: el('name').trim(), target_date: el('tdate'), accounts: [] }
      if (months) body.target_months = +el('tval'); else body.target = Math.round(+el('tval') * 100)
      for (const inp of f.querySelectorAll('[name^="acct:"]')) if (+inp.value > 0) body.accounts.push({ account_id: inp.name.slice(5), pct: +inp.value })
      send(PL.goal.id ? 'PUT' : 'POST', PL.goal.id ? '/goals/' + PL.goal.id : '/goals', body).then(() => api('/goals')).then(g => { PL.goals = g; PL.goal = null; PL.gErr = '' }, er => { PL.gErr = er.message }).finally(render)
    }
    return
  }
  if (f.dataset.cls !== undefined) {
    e.preventDefault()
    INV.form = CLASSES.map(([k]) => +f.elements[k].value || 0)
    if (INV.form.reduce((s, v) => s + v, 0) !== 100 || INV.form.some(v => v < 0 || !Number.isInteger(v))) { INV.formErr = 'Make the percentages whole numbers that total 100.'; render(); return }
    send('PUT', '/fund-classes/' + encodeURIComponent(f.dataset.cls), { split: INV.form }).then(() => { INV.classing = null; INV.data = null }, e => { INV.formErr = e.message }).finally(render)
    return
  }
  if (e.target.dataset.mren === undefined) return
  e.preventDefault(); renameMerchant(e.target.dataset.mren, $('#mrn').value)
})
document.addEventListener('keydown', e => {
  const t = e.target
  if (t.id === 'newcat' && e.key === 'Enter') { e.preventDefault(); $('[data-act="newsave"]')?.click() }
  else if (t.dataset?.rvnew !== undefined && e.key === 'Enter') { e.preventDefault(); rvAccept(+t.dataset.rvnew) }
  else if (t.id === 'mrn' && e.key === 'Escape') { e.preventDefault(); $('[data-act="mrencancel"]').click() }
  else if (t.classList?.contains('grp')) {
    if (e.key === 'Enter') { e.preventDefault(); rvAccept(+t.dataset.g) }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') { e.preventDefault(); const li = t.closest('.gwrap'), n = e.key === 'ArrowDown' ? li.nextElementSibling : li.previousElementSibling; n?.querySelector('.grp').focus() }
  } else if (e.key === 'Escape' && !toastEl.hidden && !t.closest?.('input,select,textarea,dialog')) hideToast()
})

/* ---------- shell ---------- */
const VIEWS = [['overview', 'Overview'], ['transactions', 'Transactions'], ['accounts', 'Accounts'], ['investments', 'Investments'], ['spending', 'Spending'], ['plan', 'Plan'], ['settings', 'Settings']]
// Routes with no nav tab of their own; the tab of the view they belong to stays highlighted.
const SUBVIEWS = { review: ['transactions', 'Review'], merchants: ['settings', 'Merchants'], recurring: ['spending', 'Recurring'], report: ['spending', 'Income v Expense'], alerts: ['transactions', 'Alerts'] }
const validView = v => VIEWS.some(x => x[0] === v) || v in SUBVIEWS
function paintNav() {
  const link = ([v, l]) => `<a href="#${v}" ${(SUBVIEWS[state.view]?.[0] || state.view) === v ? 'aria-current="page"' : ''}>${icon(v, 20)}<span>${l}</span></a>`
  $('#navSide').innerHTML = $('#tabs').innerHTML = VIEWS.map(link).join('')
  $('#brandSide').innerHTML = $('#brandTop').innerHTML = brandHTML
  $('#themeSide').innerHTML = icon('theme', 18) + `Theme: ${{ system: 'System', light: 'Light', dark: 'Dark' }[state.theme]}`
  $('#themeTop').innerHTML = icon('theme', 18)
  for (const id of ['#privSide', '#privTop']) {
    const b = $(id); b.setAttribute('aria-pressed', privacy.on); b.title = privacy.on ? 'Show dollar amounts' : 'Hide dollar amounts (show percentages only)'
    b.innerHTML = icon(privacy.on ? 'eyeOff' : 'eye', 18) + (id === '#privSide' ? `Amounts: ${privacy.on ? 'Hidden' : 'Shown'}` : '')
  }
  $('#privTop').setAttribute('aria-label', $('#privTop').title)
}
function render(top) {
  const v = state.view, main = $('#main'), fid = v === 'plan' && document.activeElement?.id
  if (!state.loaded) main.innerHTML = state.err ? `<div class="panel first"><h1>Could not load</h1><p>${esc(state.err)}</p><button class="btn" data-act="reload">Retry</button></div>` : '<div class="load" role="status">Loading…</div>'
  else main.innerHTML = { overview, transactions, accounts, investments, spending, settings, review, merchants, recurring, alerts, plan, report }[v]()
  paintNav()
  document.title = (VIEWS.find(x => x[0] === v)?.[1] || SUBVIEWS[v]?.[1] || 'Overview') + ' · Money Tracker'
  if (state.loaded && v === 'overview') { drawFlow(); drawNW(); drawCF(); const box = $('#flowbox'); if (box) flowEvents(box) }
  if (state.loaded && v === 'investments' && $('.invchart')) {
    const svg = $('.invchart'), months = INV.data.accounts[0].months, n = months.length
    brush(svg, v => Math.max(0, Math.min(n - 1, Math.round((v - 58) / (760 - 58 - 12) * (n - 1)))), (i, j) => {
      if (!INV.period.includes('..')) INV.zoomFrom = INV.period
      INV.period = `${months[i].month}..${months[j].month}`; INV.data = null; render()
    })
  }
  if (state.loaded && v === 'transactions') { paintTx(); fetchTx('reset'); loadAlerts() }
  if (state.loaded && v === 'settings' && D.rules === null && !state.rulesErr) loadRules()
  if (state.loaded && v === 'settings' && bk.data === null && !bk.err && !bk.phase) loadBackups()
  if (state.loaded && v === 'review') loadReview()
  if (state.loaded && v === 'merchants') loadMerchants()
  if (top) window.scrollTo(0, 0)
  if (fid) document.getElementById(fid)?.focus({ preventScroll: true })
}
function go(v) { if (location.hash === '#' + v) render(true); else location.hash = v }
addEventListener('hashchange', () => { const v = location.hash.slice(1); state.view = validView(v) ? v : 'overview'; state.sel = null; state.editing = null; state.txMsg = null; render(true) })
// Charts are drawn at a measured pixel width; redraw whenever the content column's width settles or changes
// (web fonts can resize it after the first render).
let lastW = 0
new ResizeObserver(([e]) => { const w = Math.round(e.contentRect.width); if (w === lastW) return; lastW = w; if (state.loaded && state.view === 'overview') requestAnimationFrame(() => { drawFlow(); drawNW(); drawCF() }) }).observe($('#main'))

function setTheme(t) {
  state.theme = t
  if (t === 'system') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t
  try { localStorage.setItem('theme', t) } catch { /* storage blocked */ }
  paintNav(); if (state.view === 'settings' || state.view === 'overview') render()
}
const openMonth = m => { state.f = { ...blankF(), month: m }; go('transactions') }

document.addEventListener('click', e => {
  const el = e.target.closest('[data-act]'), m = e.target.closest('.mo')
  if (m && !el) { openMonth(m.dataset.m); return }
  if (!el) return
  const a = el.dataset.act, d = el.dataset
  if (a === 'step') step(+d.d)
  else if (a === 'unzoom') { Object.assign(state, state.zoomFrom || { mode: 'month' }); state.zoomFrom = null; state.sel = null; render() }
  else if (a === 'mode') { state.zoomFrom = null; state.mode = d.v; if (d.v === 'year') state.year = +state.month.slice(0, 4); state.sel = null; render() }
  else if (a === 'go') go(d.v)
  else if (a === 'drill') drill(d.id)
  else if (a === 'needcat') go('review')
  else if (a === 'invp') { INV.period = d.v; INV.zoomFrom = null; INV.data = null; render() }
  else if (a === 'invunzoom') { INV.period = INV.zoomFrom || 'ytd'; INV.zoomFrom = null; INV.data = null; render() }
  else if (a === 'invk') { INV.kind = d.v; render() }
  else if (a === 'invc') { INV.chart = d.v; render() }
  else if (a === 'invh') { INV.hidden.has(d.v) ? INV.hidden.delete(d.v) : INV.hidden.add(d.v); render() }
  else if (a === 'invs') { if (INV.sort === d.v) INV.dir *= -1; else { INV.sort = d.v; INV.dir = d.v === 'name' ? 1 : -1 } render() }
  else if (a === 'invrow') { go('accounts'); openAccount(d.v) }
  else if (a === 'classify') {
    const p = INV.data.accounts.find(x => x.id === d.a).positions.find(x => x.symbol === d.v)
    Object.assign(INV, { classing: d.v, classAcct: d.a, formErr: '', form: p.class ? p.class.split.map(bp => Math.round(bp / 100)) : [0, 0, 0, 0, 0] }); render(); $('.clsform input')?.focus()
  }
  else if (a === 'classcancel') { INV.classing = null; render() }
  else if (a === 'classclear') send('DELETE', '/fund-classes/' + encodeURIComponent(d.v)).then(() => { INV.classing = null; INV.data = null }, e => { INV.formErr = e.message }).finally(render)
  else if (a === 'flowless') { state.flowAll = false; render() }
  else if (a === 'privacy') { privacy.on = !privacy.on; try { localStorage.setItem('privacy', privacy.on ? '1' : '0') } catch { /* storage blocked */ } render() }
  else if (a === 'theme') setTheme({ system: 'light', light: 'dark', dark: 'system' }[state.theme])
  else if (a === 'themeset') setTheme(d.v)
  else if (a === 'signout') fetch('/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).then(r => r.json()).then(o => location.assign(o.redirect)).catch(() => {}) // auth
  else if (a === 'reload') { state.err = ''; reload() }
  else if (a === 'retryperiod') { delete state.perr[periodKey()]; render() }
  else if (a === 'retryspend') { delete state.perr['s' + (state.spendBy === 'merchant' ? 'm:' : 'c:') + periodKey()]; render() }
  else if (a === 'spendby') { state.spendBy = d.v; render() }
  else if (a === 'spendcsv') downloadCSV(`spending-by-${state.spendBy || 'category'}-${periodKey()}.csv`, [[state.spendBy === 'merchant' ? 'Merchant' : 'Category', 'Amount'], ...spendRows.map(r => [r.name, r.v])])
  else if (a === 'repcsv') downloadCSV(`income-v-expense-${state.month}.csv`, repCSV)
  else if (a === 'retryreport') { delete state.perr['r' + state.month]; render() }
  else if (a === 'repcell') { const { month, ...f } = repCells[+d.i]; state.f = { ...blankF(), month, ...f }; go('transactions') }
  else if (a === 'txclear') { state.f = blankF(); render() }
  else if (a === 'unflow') { state.f.flow = ''; render() }
  else if (a === 'txretry') fetchTx('reset')
  else if (a === 'more') fetchTx('more')
  else if (a === 'pick') { state.editing = d.id; state.editNew = false; state.txMsg = null; paintTx(); $('.pick')?.focus() }
  else if (a === 'pickcancel') { state.editing = null; paintTx() }
  else if (a === 'acct') openAccount(d.id)
  else if (a === 'acctTx') { state.f = { ...blankF(), acct: d.id }; go('transactions') }
  else if (a === 'recq') { const r = D.recurring[+d.i]; state.f = { ...blankF(), q: r.name, flow: r.kind === 'income' ? 'in' : 'out' }; go('transactions') }
  else if (a === 'recmark') {
    const r = D.recurring[+d.i], key = encodeURIComponent(d.v === 'clear' ? r.mark_key || r.key : r.key)
    send(d.v === 'clear' ? 'DELETE' : 'PUT', `/recurring/${key}/mark`, d.v === 'clear' ? undefined : { state: d.v }).then(() => { D.recurring = null }, e => { state.perr.rec = e.message }).finally(render)
  }
  else if (a === 'rechidden') { state.recHidden = !state.recHidden; render() }
  else if (a === 'retryrec') { delete state.perr.rec; render() }
  else if (a === 'retryal') { delete state.perr.al; render() }
  else if (a === 'alchecked') { state.alChecked = !state.alChecked; render() }
  else if (a === 'altx') { const t = D.alerts[+d.i].tx; state.f = { ...blankF(), month: t.date.slice(0, 7), q: t.description }; go('transactions') }
  else if (a === 'alcat') { const p = D.alerts[+d.i]; state.f = { ...blankF(), month: p.month, cat: p.category }; go('transactions') }
  else if (a === 'alfine') {
    const al = D.alerts[+d.i], url = `/alerts/${al.id}/dismiss`, name = al.tx ? al.tx.merchant : catName(al.category) + ' spending'
    const redo = p => p.then(() => { D.alerts = null; return true }, e => { toast(e.message, { err: true }) }).finally(render)
    redo(send('POST', url, {})).then(ok => ok && toast(`Marked ${name} as fine.`, { undo: () => redo(send('DELETE', url)) }))
  }
  else if (a === 'spendcat') { state.f = { ...blankF(), month: periodKey(), ...spendRows[+d.i].f }; go('transactions') }
  else if (a === 'sync') doSync()
  else if (a === 'rule1') { state.ruleDel = +d.id; render() }
  else if (a === 'ruleCancel') { state.ruleDel = null; render() }
  else if (a === 'ruleOk') { state.ruleDel = null; send('DELETE', '/rules/' + d.id).then(() => { D.rules = null; cache.clear(); state.perr = {}; render() }, e => { state.rulesErr = e.message; render() }) }
  else if (a === 'rulesretry') { state.rulesErr = ''; D.rules = null; render() }
  else if (a === 'planretry') { PL.err = ''; render() }
  else if (a === 'plansrc') { state.f = { ...blankF(), flow: 'in', q: d.v }; go('transactions') }
  else if (a === 'knobreset') { delete PL.vals[d.v]; planChanged() }
  else if (a === 'acctreset') { delete PL.vals.accounts[d.v]; planChanged() }
  else if (a === 'planalldata') { const v = PL.vals; PL.vals = { birth_year: v.birth_year, events: v.events, vests: v.vests }; planChanged() }
  else if (a === 'planundo') { clearTimeout(PL.timer); PL.seq++; PL.pending = false; PL.vals = clone(PL.data.doc); PL.proj = null; PL.err = ''; render() }
  else if (a === 'plankeep') {
    PL.busy = true; render()
    send('PUT', '/plan', PL.vals).then(p => { setPlan(p); PL.err = ''; return api('/goals').then(g => { PL.goals = g }) }, e => { PL.err = e.message }).finally(() => { PL.busy = false; render() })
  }
  else if (a === 'evdel') { PL.vals.events.splice(+d.i, 1); planChanged() }
  else if (a === 'evedit') { const e = PL.vals.events[+d.i]; PL.ev = { ...e, amount: e.amount / 100, until: e.until || '', editing: +d.i }; render(); $('[data-planform="event"] [name="name"]')?.focus() }
  else if (a === 'vests') { PL.vals.vests = { ...PL.vals.vests, off: !PL.vals.vests?.off || undefined }; planChanged() }
  else if (a === 'goalnew') { PL.goal = {}; PL.gErr = ''; render(); $('[data-planform="goal"] [name="name"]')?.focus() }
  else if (a === 'goaledit') { PL.goal = clone(PL.goals.find(g => g.id === +d.v)); PL.gErr = ''; render(); $('[data-planform="goal"] [name="name"]')?.focus() }
  else if (a === 'goalcancel') { PL.goal = null; render() }
  else if (a === 'goaldel') send('DELETE', '/goals/' + d.v).then(() => api('/goals')).then(g => { PL.goals = g; PL.goal = null }, e => { PL.gErr = e.message }).finally(render)
})
document.addEventListener('change', e => {
  const t = e.target
  if (t.dataset.surplus !== undefined) tryVal(PL.vals, 'surplus_account', t.value, '')
  else if (t.dataset.knob) {
    const k = t.dataset.knob, kind = t.dataset.kind, n = kind === 'money' ? Math.round(+t.value * 100) : +t.value
    if (t.value !== '' && Number.isFinite(n)) tryVal(PL.vals, k, n, knobBase(k))
  } else if (t.dataset.acct) {
    const id = t.dataset.acct, f = t.dataset.f, base = PL.data.baseline.accounts.find(a => a.id === id)
    const n = f === 'include' ? t.checked : f === 'bucket' ? t.value : f === 'contribution' ? Math.round(+t.value * 100) : +t.value
    if (typeof n === 'number' && (t.value === '' || !Number.isFinite(n))) return
    const all = PL.vals.accounts ||= {}, o = all[id] ||= {}
    if (n === base[f]) delete o[f]; else o[f] = n
    if (!Object.keys(o).length) delete all[id]
    if (!Object.keys(all).length) delete PL.vals.accounts
    planChanged()
  } else if (t.matches('.typepick')) setAccountType(t.dataset.id, t.value)
  else if (t.matches('.pick')) { if (t.value === '__new') { state.editNew = true; paintTx(); $('#newcat')?.focus() } else if (t.value) applyCat(t.dataset.id, t.value, $('#always')?.checked) }
  else if (t.matches('.filters select')) { state.f[t.dataset.f] = t.value; render() }
})
let qTimer
document.addEventListener('input', e => {
  if (e.target.id !== 'q') return
  state.f.q = e.target.value
  clearTimeout(qTimer); qTimer = setTimeout(() => fetchTx('reset'), 250)
  $('#clrslot').innerHTML = clearHTML()
})
document.addEventListener('keydown', e => {
  const mo = e.target.closest?.('.mo')
  if (e.target.matches?.('tr[data-act="invrow"]') && e.key === 'Enter') { e.preventDefault(); e.target.click(); return }
  if (mo && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openMonth(mo.dataset.m); return }
  if (e.target.closest?.('input,textarea,select')) { if (e.key === 'Escape' && state.editing) { state.editing = null; paintTx() } return }
  if (e.key === 'Escape' && state.editing) { state.editing = null; paintTx() }
  if ((state.view === 'overview' || state.view === 'spending') && (e.key === 'ArrowLeft' || e.key === 'ArrowRight') && !e.target.closest?.('[data-id],.mo')) {
    const b = $(`.step[data-d="${e.key === 'ArrowLeft' ? -1 : 1}"]`); if (b && !b.disabled) step(e.key === 'ArrowLeft' ? -1 : 1)
  }
})

if (state.theme !== 'system') document.documentElement.dataset.theme = state.theme
const h = location.hash.slice(1); state.view = validView(h) ? h : 'overview'
render()
api('/me').then(m => { D.me = m; if (state.view === 'settings') render() }).catch(() => {}) // auth
api('/config').then(c => { D.config = c; if (state.view === 'settings') render() }).catch(() => {})
reload()
