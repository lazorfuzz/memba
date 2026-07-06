package api

// Minimal human curation surface (spec §16): the memory health dashboard is
// the front page, the review queue the second. Single embedded page, no
// build step; data calls use the operator's bearer token (kept in
// localStorage, never sent anywhere but this API).

import "net/http"

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	_, _ = w.Write([]byte(uiPage))
}

const uiPage = `<!doctype html>
<meta charset="utf-8">
<title>memba — memory health</title>
<style>
  :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, sans-serif; }
  body { margin: 2rem auto; max-width: 72rem; padding: 0 1rem; line-height: 1.45; }
  h1 { font-size: 1.3rem; } h2 { font-size: 1.05rem; margin-top: 2rem; }
  .cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(11rem, 1fr)); gap: .8rem; }
  .stat { border: 1px solid color-mix(in srgb, currentColor 25%, transparent); border-radius: .5rem; padding: .7rem .9rem; }
  .stat b { display: block; font-size: 1.5rem; }
  .stat span { opacity: .75; font-size: .82rem; }
  .warn b { color: #c2410c; }
  table { border-collapse: collapse; width: 100%; font-size: .9rem; }
  td, th { text-align: left; padding: .45rem .6rem; border-bottom: 1px solid color-mix(in srgb, currentColor 18%, transparent); vertical-align: top; }
  button { cursor: pointer; border-radius: .35rem; border: 1px solid color-mix(in srgb, currentColor 35%, transparent); background: transparent; color: inherit; padding: .2rem .6rem; }
  button.ok { border-color: #16a34a; } button.no { border-color: #dc2626; }
  input { padding: .35rem .5rem; border-radius: .35rem; border: 1px solid color-mix(in srgb, currentColor 35%, transparent); background: transparent; color: inherit; }
  #token { width: 26rem; } #ns { width: 20rem; }
  .cite { opacity: .7; font-size: .8rem; }
  .err { color: #dc2626; }
  code { font-size: .85em; }
</style>
<h1>memba — memory health</h1>
<p>
  <input id="token" type="password" placeholder="bearer token (memctl token …)">
  <input id="ns" placeholder="namespace filter (optional)">
  <button onclick="refresh()">load</button>
  <span id="status" class="err"></span>
</p>
<div id="health" class="cards"></div>
<h2>Review queue <span id="qcount" class="cite"></span></h2>
<table id="queue"><thead><tr><th>type</th><th>card</th><th>citations</th><th>decide</th></tr></thead><tbody></tbody></table>
<script>
const $ = (id) => document.getElementById(id);
const tokenEl = $('token');
tokenEl.value = localStorage.getItem('memba_token') || '';
$('ns').value = localStorage.getItem('memba_ns') || '';

async function api(path, opts = {}) {
  const res = await fetch(path, { ...opts, headers: {
    'Authorization': 'Bearer ' + tokenEl.value,
    'Content-Type': 'application/json', ...(opts.headers || {}) } });
  if (!res.ok) throw new Error(path + ' → ' + res.status + ' ' + (await res.text()).slice(0, 200));
  return res.json();
}

function stat(label, value, warn) {
  return '<div class="stat' + (warn ? ' warn' : '') + '"><b>' + value + '</b><span>' + label + '</span></div>';
}

async function refresh() {
  localStorage.setItem('memba_token', tokenEl.value);
  localStorage.setItem('memba_ns', $('ns').value);
  $('status').textContent = '';
  try {
    const { health: h } = await api('/v1/admin/health');
    const s = h.cards_by_status || {};
    $('health').innerHTML =
      stat('active cards', s.active || 0) +
      stat('proposal queue', h.proposal_queue_depth, h.proposal_queue_depth > 20) +
      stat('verify overdue', h.verify_overdue, h.verify_overdue > 0) +
      stat('stale', s.stale || 0, (s.stale || 0) > 0) +
      stat('dormant', s.dormant || 0) +
      stat('active facts', h.facts_active) +
      stat('evidence items', h.evidence_total) +
      stat('quarantined', h.quarantined_raw, h.quarantined_raw > 0) +
      stat('median time-to-promotion', fmtDur(h.median_time_to_promotion_seconds)) +
      stat('low-answerability (7d)', h.low_answerability_7d, h.low_answerability_7d > 5) +
      stat('consolidation runs (7d)', h.consolidation_runs_7d);
    await loadQueue();
  } catch (e) { $('status').textContent = e.message; }
}

function fmtDur(sec) {
  if (!sec) return '—';
  if (sec < 90) return Math.round(sec) + 's';
  if (sec < 5400) return Math.round(sec / 60) + 'm';
  return (sec / 3600).toFixed(1) + 'h';
}

function esc(t) { const d = document.createElement('div'); d.textContent = t ?? ''; return d.innerHTML; }

async function loadQueue() {
  const ns = encodeURIComponent($('ns').value);
  const { cards } = await api('/v1/cards?status=proposed&limit=50' + (ns ? '&namespace=' + ns : ''));
  $('qcount').textContent = '(' + cards.length + ')';
  $('queue').querySelector('tbody').innerHTML = cards.map((c) =>
    '<tr><td><code>' + esc(c.card_type) + '</code><div class="cite">' + esc(c.namespace_id) + '</div></td>' +
    '<td><b>' + esc(c.title) + '</b><div>' + esc(c.body) + '</div></td>' +
    '<td class="cite">' + (c.sources || []).map((x) => esc(x.source_uri)).join('<br>') + '</td>' +
    '<td><button class="ok" onclick="review(\'' + c.id + '\',\'approve\')">approve</button> ' +
    '<button class="no" onclick="review(\'' + c.id + '\',\'reject\')">reject</button></td></tr>'
  ).join('') || '<tr><td colspan="4">queue empty 🎉</td></tr>';
}

async function review(id, decision) {
  const reason = decision === 'reject' ? (prompt('reason?') || '') : 'approved via review UI';
  try {
    await api('/v1/cards/' + id + '/review', { method: 'POST', body: JSON.stringify({ decision, reason }) });
    await refresh();
  } catch (e) { $('status').textContent = e.message; }
}

if (tokenEl.value) refresh();
</script>
`
