// 移植自 CPA quotaTimelineModel/QuotaTimeline：本地日期、单凭证单轨、完整窗口投影。
(() => {
  const HOUR = 3600000;
  const DAY = 24 * HOUR;
  const pad = n => String(n).padStart(2, '0');
  const dateText = ms => { const d = new Date(ms); return `${pad(d.getMonth() + 1)}/${pad(d.getDate())}`; };
  const timeText = ms => { const d = new Date(ms); return `${pad(d.getHours())}:${pad(d.getMinutes())}`; };
  const weekdays = ['日', '一', '二', '三', '四', '五', '六'];
  const humanize = seconds => {
    if (seconds <= 0) return '可刷新';
    const minutes = Math.max(1, Math.ceil(seconds / 60));
    const days = Math.floor(minutes / 1440);
    const hours = Math.floor(minutes % 1440 / 60);
    return days ? `${days} 天 ${hours} 小时后刷新` : hours ? `${hours} 小时 ${minutes % 60} 分钟后刷新` : `${minutes} 分钟后刷新`;
  };
  let mode = 'week';
  let offset = 0;

  // 只读取服务端白名单展示字段，不向浏览器提供凭证 JSON。
  const boxes = [...document.querySelectorAll('.grid-box')];
  for (const box of boxes) {
    box.rows = [...box.querySelectorAll('.timeline-lane')].map(row => ({
      row,
      track: row.querySelector('.track'),
      windows: [...row.querySelectorAll('[data-window-code]')].map(el => ({
        code: el.dataset.windowCode,
        duration: Number(el.dataset.duration),
        reset: Date.parse(el.dataset.reset),
        remaining: Number(el.dataset.remaining)
      })).filter(w => Number.isFinite(w.reset) && w.duration > 0)
    }));
  }
  function spanFor(kind, now) {
    const start = new Date(now);
    start.setHours(0, 0, 0, 0);
    if (kind === 'week') start.setDate(start.getDate() - start.getDay());
    start.setDate(start.getDate() + offset * (kind === 'week' ? 7 : 1));
    const end = new Date(start);
    end.setDate(end.getDate() + (kind === 'week' ? 14 : 3));
    return {start: start.getTime(), end: end.getTime()};
  }
  function choose(kind, windows) {
    const preferred = windows.find(w => w.code === (kind === 'week' ? '7d' : '5h'));
    if (preferred) return preferred;
    const pool = windows.filter(w => kind !== 'hour' || w.duration === 5 * HOUR);
    return pool.sort((a, b) => b.duration - a.duration || a.reset - b.reset)[0];
  }
  function paintTrack(track, win, span, now, kind, ticks) {
    track.querySelectorAll('.seg,.nowline,.lane-idle').forEach(el => el.remove());
    const grid = track.querySelector('.track-grid');
    grid.replaceChildren(...ticks.map(() => document.createElement('span')));
    if (now >= span.start && now < span.end) {
      const line = document.createElement('i');
      line.className = 'nowline';
      line.style.left = `${(now - span.start) / (span.end - span.start) * 100}%`;
      track.append(line);
    }
    if (!win) {
      const idle = document.createElement('span'); idle.className = 'lane-idle'; idle.textContent = '没有正在计时的窗口'; track.append(idle); return;
    }
    // 与 CPA windowsIn 一样，从已知重置边界按完整周期向前后推算。
    let end = win.reset + Math.ceil((span.start - win.reset) / win.duration) * win.duration;
    const limit = Math.ceil((span.end - span.start) / win.duration) + 2;
    for (let count = 0; end - win.duration < span.end && count < limit; count++, end += win.duration) {
      const begin = end - win.duration;
      const left = Math.max(0, (begin - span.start) / (span.end - span.start) * 100);
      const right = Math.min(100, (end - span.start) / (span.end - span.start) * 100);
      if (right <= left) continue;
      const state = end <= now ? 'past' : begin <= now ? 'cur' : 'next';
      const known = state === 'cur' && end === win.reset;
      const bar = document.createElement('i');
      bar.className = `seg ${state}`;
      bar.style.left = `${left}%`;
      bar.style.width = `${right - left}%`;
      bar.title = `${dateText(begin)} ${timeText(begin)} → ${dateText(end)} ${timeText(end)}${known ? `\n剩余 ${Math.round(win.remaining)}%` : ''}`;
      if (known) {
        const fill = document.createElement('i'); fill.className = 'window-fill'; fill.style.width = `${100 - win.remaining}%`; bar.append(fill);
      }
      if (right - left > (kind === 'hour' ? 4.5 : 9)) {
        const label = document.createElement('span'); label.className = 'window-label';
        label.textContent = `${known ? Math.round(win.remaining) + '% · ' : ''}${kind === 'hour' ? timeText(end) : dateText(end) + ' ' + timeText(end)}`;
        bar.append(label);
      }
      track.append(bar);
    }
  }
  function renderTimelines(now) {
    for (const box of boxes) {
      const kind = box.classList.contains('gv-hour') ? 'hour' : 'week';
      const span = spanFor(kind, now);
      const count = kind === 'hour' ? 12 : 14;
      const step = (span.end - span.start) / count;
      const ticks = Array.from({length: count}, (_, i) => span.start + step * i);
      const heads = box.querySelector('.axis-cells');
      heads.replaceChildren(...ticks.map(at => {
        const cell = document.createElement('div'); cell.className = 'axis-cell';
        const dayStart = kind === 'week' || new Date(at).getHours() === 0;
        const top = document.createElement('span'); top.className = 'gh-top'; top.textContent = dayStart ? weekdays[new Date(at).getDay()] : '';
        const bottom = document.createElement('span'); bottom.className = 'gh-bottom'; bottom.textContent = dayStart ? dateText(at) : timeText(at);
        cell.append(top, bottom); return cell;
      }));
      for (const row of box.rows) {
        const win = choose(kind, row.windows);
        row.row.querySelector('.lane-period').textContent = win ? kind === 'hour' ? '5h' : win.duration < DAY ? `${Math.round(win.duration / HOUR)}h` : `${Math.round(win.duration / DAY)}d` : '';
        paintTrack(row.track, win, span, now, kind, ticks);
      }
      const range = document.querySelector(`[data-range="${kind}"]`);
      if (range) range.textContent = `${dateText(span.start)} – ${dateText(span.end - 1)} · ${kind === 'hour' ? '三天' : '两周'}${offset === 0 ? ' · 当前' : ''}`;
      box.style.display = kind === mode ? '' : 'none';
    }
    document.querySelectorAll('[data-range]').forEach(el => { el.style.display = el.dataset.range === mode ? '' : 'none'; });
    document.querySelectorAll('[data-view]').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.view === mode)));
    const todayButton = document.querySelector('[data-nav="today"]');
    if (todayButton) {
      todayButton.toggleAttribute('disabled', offset === 0);
      // 与 CPA 一致：离开当前时间段后中间按钮显示所看区间的起始日期，点它回到今天。
      todayButton.textContent = offset === 0 ? '今天' : dateText(spanFor(mode, now).start);
      todayButton.title = offset === 0 ? '' : '今天';
    }
  }
  function tick() {
    const now = Date.now();
    document.querySelectorAll('time[data-until]').forEach(t => { const at = Date.parse(t.dateTime); if (Number.isFinite(at)) t.textContent = humanize((at - now) / 1000); });
    document.querySelectorAll('time[data-plan-exp]').forEach(t => { const at = Date.parse(t.dateTime); if (Number.isFinite(at)) t.textContent = dateText(at) + ' ' + timeText(at); });
    if (boxes.length) renderTimelines(now);
  }
  // 模型可用性整块的折叠状态跨刷新保持：默认展开，收过一次就一直收着。
  const statusPanel = document.getElementById('model-status');
  if (statusPanel) {
    try { statusPanel.open = sessionStorage.getItem('mq-status-open') !== '0'; } catch {}
    statusPanel.addEventListener('toggle', () => {
      try { sessionStorage.setItem('mq-status-open', statusPanel.open ? '1' : '0'); } catch {}
    });
  }
  // 模型可用性的三组服务切换，与「按周 / 5小时」同一套 pills 交互。
  const cohortTabs = [...document.querySelectorAll('[data-cohort]')];
  const cohortPanels = [...document.querySelectorAll('[data-cohort-panel]')];
  const pickCohort = id => {
    cohortTabs.forEach(b => b.setAttribute('aria-pressed', String(b.dataset.cohort === id)));
    cohortPanels.forEach(p => { p.style.display = p.dataset.cohortPanel === id ? '' : 'none'; });
  };
  cohortTabs.forEach(b => b.addEventListener('click', () => pickCohort(b.dataset.cohort)));
  document.querySelectorAll('[data-view]').forEach(b => b.addEventListener('click', () => { mode = b.dataset.view; offset = 0; tick(); }));
  document.querySelectorAll('[data-nav]').forEach(b => b.addEventListener('click', () => { offset = b.dataset.nav === 'today' ? 0 : offset + (b.dataset.nav === 'prev' ? -1 : 1); tick(); }));
  document.querySelectorAll('[data-refresh]').forEach(b => b.addEventListener('click', () => { b.disabled = true; try { sessionStorage.setItem('mq-refreshed', '1'); } catch {} location.reload(); }));
  try { if (sessionStorage.getItem('mq-refreshed')) { sessionStorage.removeItem('mq-refreshed'); const toast = document.getElementById('toast'); toast.classList.add('show'); setTimeout(() => toast.classList.remove('show'), 2600); } } catch {}
  tick(); setInterval(tick, 1000);
})();
