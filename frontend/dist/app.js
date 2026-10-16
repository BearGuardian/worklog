/*
 * worklog 前端逻辑。
 *
 * 与 Go 的通道:window.go.main.App.Xxx()(Wails 运行时注入)。
 * 浏览器里直接打开时它不存在,所以走 bridge 兜底成空实现 ——
 * 这样改样式时可以直接用浏览器打开调试。
 */
(() => {
  'use strict';

  const Go = window.go?.main?.App ?? null;

  // 浏览器里预览时的兜底,保证页面不报错
  const bridge = new Proxy({}, {
    get: (_, name) => {
      if (Go && typeof Go[name] === 'function') return (...a) => Go[name](...a);
      return async () => { throw new Error(`未连接到后端: ${String(name)}`); };
    },
  });

  const el = (id) => document.getElementById(id);
  const DOW = ['周一', '周二', '周三', '周四', '周五', '周六', '周日'];
  const STATUS_LABEL = { todo: '待办', doing: '进行中', done: '已完成' };
  const DAY = 864e5;

  // ---------------------------------------------------------------- 工具
  const pad = (n) => String(n).padStart(2, '0');
  const dur = (m) => (m >= 60 ? `${Math.floor(m / 60)}h${m % 60 ? pad(m % 60) + 'm' : ''}` : `${m}m`);
  const iso = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const parseHM = (s) => { const [h, m] = String(s).split(':').map(Number); return (h || 0) * 60 + (m || 0); };
  const toHM = (min) => `${pad(Math.floor(min / 60))}:${pad(min % 60)}`;
  const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

  function mondayOf(d) {
    const x = new Date(d);
    x.setHours(0, 0, 0, 0);
    x.setDate(x.getDate() - ((x.getDay() + 6) % 7));
    return x;
  }

  // ---------------------------------------------------------------- 状态
  const state = {
    weekOffset: 0,
    autoRange: true,
    fixedStart: 9,
    fixedEnd: 18,
    tasks: [],
    categories: [],
    selectedId: null,
    editingId: null,
    durTaskId: null,
    dayStart: 9,
    dayEnd: 18,
    span: 9,
    // 状态视角。空串 = 全部。只看不影响数据,只影响画什么
    statusFilter: '',
  };

  // 当前视角下可见的事项。时间轴、甘特图、每日合计都走它;
  // 统计条不走 —— 那四个数字是整周的概览,不该被视角改掉。
  const visible = () =>
    (state.statusFilter ? state.tasks.filter((t) => t.status === state.statusFilter) : state.tasks);

  const TODAY = new Date();
  const W0 = mondayOf(TODAY);

  const weekStart = () => new Date(W0.getTime() + state.weekOffset * 7 * DAY);
  const dayAt = (i) => new Date(weekStart().getTime() + i * DAY);
  const isToday = (i) => iso(dayAt(i)) === iso(TODAY);
  const isWeekend = (i) => i >= 5;
  const catById = (id) => state.categories.find((c) => c.id === id) || null;
  const catColor = (id) => (catById(id) || {}).color || 'var(--todo)';
  const catName = (id) => (catById(id) || {}).name || '未分类';
  const selected = () => state.tasks.find((t) => t.id === state.selectedId) || null;

  const pct = (min) => ((min / 60 - state.dayStart) / state.span) * 100;

  // ---------------------------------------------------------------- 取数
  async function reload() {
    const from = iso(weekStart());
    const to = iso(new Date(weekStart().getTime() + 6 * DAY));
    try {
      const [tasks, cats] = await Promise.all([
        bridge.ListTasks(from, to),
        bridge.ListCategories(),
      ]);
      state.tasks = tasks || [];
      state.categories = cats || [];
    } catch (e) {
      reportError('加载失败')(e);
      state.tasks = [];
    }
    refreshRange();
    renderAll();
  }

  function refreshRange() {
    if (state.autoRange) {
      let lo = 24 * 60, hi = 0;
      visible().forEach((t) => { lo = Math.min(lo, t.startMin); hi = Math.max(hi, t.endMin); });

      const todayInWeek = iso(TODAY) >= iso(weekStart()) && iso(TODAY) < iso(new Date(weekStart().getTime() + 7 * DAY));
      if (todayInWeek) {
        const n = new Date();
        hi = Math.max(hi, n.getHours() * 60 + n.getMinutes());
      }
      if (lo >= hi) { lo = 9 * 60; hi = 18 * 60; }
      state.dayStart = Math.max(0, Math.floor(lo / 60));
      state.dayEnd = Math.min(24, Math.ceil(hi / 60));
    } else {
      state.dayStart = state.fixedStart;
      state.dayEnd = state.fixedEnd;
    }
    if (state.dayEnd <= state.dayStart) state.dayEnd = state.dayStart + 1;
    state.span = state.dayEnd - state.dayStart;

    el('hStart').value = state.dayStart;
    el('hEnd').value = state.dayEnd;
  }

  // ---------------------------------------------------------------- 渲染
  function renderAll() {
    renderTop(); renderHead(); renderStats(); renderGantt(); renderDetail();
  }

  function renderTop() {
    const a = weekStart(), b = dayAt(6);
    el('rangeLabel').innerHTML =
      `${a.getFullYear()} 年 ${a.getMonth() + 1} 月 <em>·</em> ${a.getDate()} – ${b.getDate()} 日`;
  }

  function tasksOn(dateISO) {
    return visible().filter((t) => segsOf(t, dateISO).length > 0);
  }

  // 一条事项落在某一天的区间。
  //
  // 没有时段的事项(待办)按计划跨度画一段,标 plan —— 甘特图上画成虚线块,
  // 表示"打算做",而不是"做过了"。
  //
  // 跨午夜的时段会被切成两截:本日 [start, 24:00),次日 [0, 余下的部分)。
  // 后端保证 EndMin = StartMin + 时长,所以看 EndMin 有没有超过 1440 就知道。
  function segsOf(t, dateISO) {
    if (!t.spans || !t.spans.length) {
      if (t.date !== dateISO || t.endMin <= t.startMin) return [];
      return [{ start: t.startMin, end: Math.min(t.endMin, 24 * 60), plan: true }];
    }
    const out = [];
    t.spans.forEach((sp) => {
      if (sp.date === dateISO) {
        out.push({ start: sp.startMin, end: Math.min(sp.endMin, 24 * 60), open: sp.open, span: sp });
      }
      if (sp.endMin > 24 * 60) {
        const nx = iso(new Date(new Date(sp.date + 'T00:00:00').getTime() + DAY));
        if (nx === dateISO) {
          out.push({ start: 0, end: sp.endMin - 24 * 60, open: sp.open, span: sp });
        }
      }
    });
    return out;
  }

  // 一天里实际投入的分钟数(按时段算,不是按整条事项的时长)
  function dayMinutes(dateISO) {
    let m = 0;
    visible().forEach((t) => {
      segsOf(t, dateISO).forEach((s) => { if (!s.plan) m += s.end - s.start; });
    });
    return m;
  }

  function renderHead() {
    el('ganttHead').innerHTML =
      '<div class="gap"></div>' + DOW.map((dow, i) => {
        // 按当天实际投入的时段求和,不是把整条事项的时长搬过来 ——
        // 一条事项跨天时后者会在两天里各算一次
        const mins = dayMinutes(iso(dayAt(i)));
        return `<div class="day-head${isToday(i) ? ' today' : ''}${isWeekend(i) ? ' weekend' : ''}" data-day="${i}">
          <div class="dow">${dow}</div>
          <div class="dom">${dayAt(i).getDate()}</div>
          <div class="sum">${mins ? dur(mins) : '—'}</div>
        </div>`;
      }).join('');

    document.querySelectorAll('.day-head').forEach((node) => {
      node.onclick = () => openEdit(null, Number(node.dataset.day));
    });
  }

  function renderStats() {
    const total = state.tasks.reduce((a, t) => a + t.durationMin, 0);
    const n = (s) => state.tasks.filter((t) => t.status === s).length;
    el('stats').innerHTML = `
      <div class="stat accent"><div class="num">${(total / 60).toFixed(1)}<span>h</span></div><div class="lbl">本周总时长</div></div>
      <div class="stat done"><div class="num">${n('done')}<span>项</span></div><div class="lbl">已完成</div></div>
      <div class="stat working"><div class="num">${n('doing')}<span>项</span></div><div class="lbl">进行中</div></div>
      <div class="stat"><div class="num">${n('todo')}<span>项</span></div><div class="lbl">待办</div></div>`;
  }

  // 重叠的区间并排到不同的 lane,各自均分列宽 —— 不能一个占满整宽、
  // 另一个只占一半,那样两块会直接盖在一起。
  //
  // 输入单位是**区间**而不是事项:一条事项可能有多个时段,拿事项当单位的话
  // 它的几段会共用一个 lane,和别的段重叠时照样会盖住。
  function assignLanes(items) {
    const flat = [];
    items.forEach((it) => it.segs.forEach((sg) => flat.push(sg)));

    const laneEnds = [];
    flat.sort((a, b) => a.start - b.start || b.end - a.end).forEach((sg) => {
      let li = laneEnds.findIndex((end) => end <= sg.start);
      if (li === -1) { laneEnds.push(sg.end); li = laneEnds.length - 1; }
      else laneEnds[li] = sg.end;
      sg._lane = li;
    });
    return Math.max(laneEnds.length, 1);
  }

  // 一天里要画的东西:每条事项 + 它落在这一天的那些区间 + 要不要画跨度条。
  //
  // 跨度条只在"这些段之间有断档"时才画。单段的事项,跨度和实心块本来就是
  // 同一块,再画一条淡色底只是叠了一层没用的颜色。
  function dayItems(dateISO) {
    const out = [];
    visible().forEach((t) => {
      const segs = segsOf(t, dateISO);
      if (!segs.length) return;

      const lo = Math.min.apply(null, segs.map((s) => s.start));
      const hi = Math.max.apply(null, segs.map((s) => s.end));
      const solid = segs.reduce((a, s) => a + (s.end - s.start), 0);
      out.push({ task: t, segs, bar: hi - lo > solid + 1 ? { start: lo, end: hi } : null });
    });
    return out;
  }

  function renderGantt() {
    const body = el('ganttBody');
    body.innerHTML = '';
    body.style.setProperty('--hours', state.span);

    const hours = document.createElement('div');
    hours.className = 'hours';
    hours.style.setProperty('--hours', state.span);
    for (let h = state.dayStart; h <= state.dayEnd; h++) {
      const node = document.createElement('div');
      node.className = 'h' + (h === state.dayEnd ? ' last' : '');
      node.style.top = pct(h * 60) + '%';
      node.textContent = pad(h) + ':00';
      hours.appendChild(node);
    }
    body.appendChild(hours);

    for (let i = 0; i < 7; i++) {
      const col = document.createElement('div');
      col.className = 'col' + (isWeekend(i) ? ' weekend' : '') + (isToday(i) ? ' today' : '');
      col.style.setProperty('--hours', state.span);

      // 点列内空白 = 在那个时间点新建
      col.onclick = (e) => {
        if (e.target !== col) return;
        const r = col.getBoundingClientRect();
        const mins = state.dayStart * 60 + ((e.clientY - r.top) / r.height) * state.span * 60;
        openEdit(null, i, Math.round(mins / 15) * 15);
      };

      const items = dayItems(iso(dayAt(i)));
      const lanes = assignLanes(items);
      const w = 100 / lanes;

      // 选中 / 分类色 / 点击行为都按 **事项** 维度,不按段 ——
      // 点任意一段都是选中整条事项,双击任意一段打开的都是同一个编辑框。
      const bind = (node, t) => {
        node.style.setProperty('--cat', catColor(t.categoryId));
        if (t.id === state.selectedId) node.classList.add('sel');
        node.onclick = (e) => {
          e.stopPropagation();
          hideCtx();
          state.selectedId = t.id;
          renderGantt(); renderDetail();
          bridge.GetTask(t.id).then((full) => {
            // 列表里的对象不带备注,选中后单独拉一次补上
            const idx = state.tasks.findIndex((x) => x.id === full.id);
            if (idx >= 0) { state.tasks[idx] = full; renderDetail(); }
          }).catch(() => {});
        };
        node.ondblclick = (e) => { e.stopPropagation(); openEdit(t); };
        // 右键 = 选中 + 弹快捷菜单。先选中,菜单里的操作才有明确的目标,
        // 详情栏也会同步显示这条,不会出现"菜单在操作 A、详情栏是 B"。
        node.oncontextmenu = (e) => {
          e.preventDefault();
          e.stopPropagation();
          state.selectedId = t.id;
          renderGantt(); renderDetail();
          openCtx(t, e.clientX, e.clientY);
        };
      };

      items.forEach((it) => {
        const t = it.task;

        // 跨度条:淡色底,表示这件事从什么时候横到什么时候
        if (it.bar) {
          const bar = document.createElement('div');
          bar.className = 'span-bar';
          bar.style.top = pct(it.bar.start) + '%';
          bar.style.height = Math.max(pct(it.bar.end) - pct(it.bar.start), 0.4) + '%';
          bind(bar, t);
          bar.title = `${t.title}\n跨度 ${toHM(it.bar.start)}–${toHM(it.bar.end)} · 实际投入 ${dur(t.durationMin)}`;
          col.appendChild(bar);
        }

        it.segs.forEach((sg) => {
          const node = document.createElement('div');
          node.className = 'blk ' + t.status;
          if (sg.plan) node.classList.add('plan');
          if (sg.open) node.classList.add('live');

          const h = pct(sg.end) - pct(sg.start);
          if (h < 6) node.classList.add('short');

          node.style.left = `calc(${sg._lane * w}% + 3px)`;
          node.style.right = `calc(${(lanes - sg._lane - 1) * w}% + 3px)`;
          node.style.top = pct(sg.start) + '%';
          node.style.height = `calc(${h}% - 3px)`;

          const label = sg.plan ? '计划' : dur(sg.end - sg.start);
          const range = `${toHM(sg.start)}–${toHM(sg.end)}`;
          node.title = `${t.title}\n${catName(t.categoryId)} · ${range} · ${label} · ${statusLabel(t)}\n双击编辑`;
          node.innerHTML = `<div class="t">${esc(t.title)}</div>
                            <div class="m">${range} · ${label}</div>`;

          bind(node, t);
          col.appendChild(node);
        });
      });

      if (isToday(i)) {
        const n = new Date();
        const nm = n.getHours() * 60 + n.getMinutes();
        if (nm >= state.dayStart * 60 && nm <= state.dayEnd * 60) {
          const line = document.createElement('div');
          line.className = 'nowline';
          line.style.top = pct(nm) + '%';
          col.appendChild(line);
        }
      }
      body.appendChild(col);
    }

    // 切了视角之后这一周可能是空的,给句话,否则看着像坏了
    if (!visible().length) {
      const hint = document.createElement('div');
      hint.className = 'gantt-empty';
      hint.textContent = state.statusFilter
        ? `这一周没有「${STATUS_LABEL[state.statusFilter]}」的事项`
        : '这一周还没有记录';
      body.appendChild(hint);
    }
  }

  // 状态标签。doing 但没有时段在跑 = 已暂停 —— 状态仍是三态,
  // "已暂停"是从时段推导出来的,不是第四个状态。
  function statusLabel(t) {
    if (t.status === 'doing' && !t.running) return '已暂停';
    return STATUS_LABEL[t.status];
  }

  function renderDetail() {
    const host = el('detail');
    const t = selected();
    if (!t) {
      host.innerHTML = `<div class="detail-head">
        <div class="eyebrow">任务详情</div>
        <h2 style="color:var(--text-faint);font-weight:500;font-size:13.5px;line-height:1.6">
          点一个任务看详情<br>双击任务或在空白处单击可以新建 / 编辑
        </h2></div>`;
      return;
    }

    const notes = t.notes || [];
    const spans = t.spans || [];
    const d = new Date(t.date + 'T00:00:00');
    const dow = DOW[(d.getDay() + 6) % 7];
    const cat = catById(t.categoryId);
    const paused = t.status === 'doing' && !t.running;

    host.innerHTML = `
      <div class="detail-head">
        <div class="eyebrow">任务详情</div>
        <h2>${esc(t.title)}</h2>
        <div class="chips">
          <span class="chip ${t.status}${paused ? ' paused' : ''}">${statusLabel(t)}</span>
          ${cat ? `<span class="chip"><i class="swatch" style="background:${cat.color}"></i>${esc(cat.name)}</span>` : ''}
          <span class="chip">${dow} ${d.getMonth() + 1}/${d.getDate()}</span>
        </div>
      </div>

      <div class="detail-stats">
        <div><div class="k">实际投入${t.confirmed ? '' : '(未确认)'}</div><div class="v">${t.durationMin ? dur(t.durationMin) : '—'}</div></div>
        <div><div class="k">${spans.length ? '时段 · ' + spans.length + ' 段' : '计划时段'}</div><div class="v">${spans.length ? dur(t.durationMin) : t.start + '–' + t.end}</div></div>
      </div>

      <div class="spans">
        <div class="section">时段</div>
        ${spans.length
          ? spans.map((s) => `
              <div class="note${s.open ? ' live' : ''}">
                <div class="rail"><i></i></div>
                <div class="body">
                  <div class="who">
                    <b>${s.start}–${s.open ? '进行中' : s.end}</b>
                    <span>${s.open ? dur(s.minutes) + ' 且还在走' : dur(s.minutes)}</span>
                  </div>
                </div>
                <button class="seg-del" data-span="${s.id}" title="删除这一段">×</button>
              </div>`).join('')
          : `<div class="note"><div class="rail"><i></i></div>
               <div class="body"><p style="color:var(--text-faint)">还没开始计时</p></div></div>`}
        <div class="seg-add">
          <input type="time" id="segStart" value="${t.start}">
          <span class="dash">–</span>
          <input type="time" id="segEnd" value="${t.end}">
          <button class="text-btn" id="segAdd">+ 添加时段</button>
        </div>
      </div>

      <div class="notes">
        <div class="section">备注 · ${notes.length}</div>
        ${notes.length
          ? notes.map((n) => `
              <div class="note${n.who === 'claude' ? ' mcp' : ''}">
                <div class="rail"><i></i></div>
                <div class="body">
                  <div class="who"><b>${n.who === 'claude' ? 'Claude' : '我'}</b><span>${n.at}</span></div>
                  <p>${esc(n.text)}</p>
                </div>
              </div>`).join('')
          : `<div class="note"><div class="rail"><i></i></div>
               <div class="body"><p style="color:var(--text-faint)">还没有备注</p></div></div>`}
      </div>

      <div class="note-add">
        <input id="noteText" placeholder="加一条备注…" autocomplete="off">
        <button id="noteAdd" title="添加">+</button>
      </div>

      <div class="detail-foot">
        <button class="text-btn" id="btnEdit">编辑</button>
        ${t.status === 'doing' ? `<button class="text-btn" id="btnPause">${t.running ? '暂停' : '继续'}</button>` : ''}
        <button class="primary-btn" id="btnToggle">
          ${t.status === 'doing' ? '结束并确认耗时' : t.status === 'done' ? '重新开始' : '开始计时'}
        </button>
      </div>`;

    host.querySelectorAll('.seg-del').forEach((btn) => {
      btn.onclick = async (e) => {
        e.stopPropagation();
        try {
          await bridge.DeleteSpan(btn.dataset.span);
          replaceTask(await bridge.GetTask(t.id));
          renderDetail(); renderGantt(); renderHead(); renderStats();
        } catch (err) { reportError('删除时段失败')(err); }
      };
    });
    el('segAdd').onclick = async () => {
      const a = el('segStart').value, b = el('segEnd').value;
      if (!a || !b) return;
      try {
        const full = await bridge.AddSpan(t.id, t.date, a, b);
        replaceTask(full); renderDetail(); renderGantt(); renderHead(); renderStats();
      } catch (err) { reportError('添加时段失败')(err); }
    };

    el('noteAdd').onclick = async () => {
      const v = el('noteText').value.trim();
      if (!v) return;
      try {
        const full = await bridge.AddNote(t.id, v);
        replaceTask(full); renderDetail(); renderGantt();
      } catch (e) { reportError('添加备注失败')(e); }
    };
    el('noteText').onkeydown = (e) => { if (e.key === 'Enter') el('noteAdd').click(); };
    el('btnEdit').onclick = () => openEdit(t);
    el('btnToggle').onclick = async () => {
      if (t.status === 'doing') { confirmDuration(t); return; }
      try {
        const full = await bridge.SetTaskStatus(t.id, 'doing');
        replaceTask(full); renderAll();
      } catch (e) { reportError('操作失败')(e); }
    };
    const pauseBtn = el('btnPause');
    if (pauseBtn) {
      pauseBtn.onclick = async () => {
        try {
          const full = t.running
            ? await bridge.PauseTask(t.id)
            : await bridge.SetTaskStatus(t.id, 'doing');
          replaceTask(full); renderAll();
        } catch (e) { reportError('操作失败')(e); }
      };
    }
  }

  function replaceTask(full) {
    const i = state.tasks.findIndex((x) => x.id === full.id);
    if (i >= 0) state.tasks[i] = full; else state.tasks.push(full);
  }

  // ---------------------------------------------------------------- 新建 / 编辑
  const editOverlay = el('editOverlay');
  const f = {
    title: el('fTitle'), cat: el('fCat'), date: el('fDate'),
    start: el('fStart'), end: el('fEnd'), status: el('fStatus'),
  };

  function renderCatPicks() {
    const cur = f.cat.value.trim();
    el('catPicks').innerHTML = state.categories.map((c) =>
      `<button type="button" class="cat-pick${c.name === cur ? ' on' : ''}"
               data-name="${esc(c.name)}" style="--pc:${c.color}">
         <i class="swatch"></i>${esc(c.name)}
       </button>`).join('')
      + (cur && !catById(state.categories.find((c) => c.name === cur)?.id)
        ? `<span class="cat-pick" style="cursor:default"><span class="new">新建「${esc(cur)}」</span></span>` : '');

    document.querySelectorAll('.cat-pick[data-name]').forEach((node) => {
      node.onclick = () => { f.cat.value = node.dataset.name; renderCatPicks(); };
    });
  }

  function openEdit(task, dayIdx, startMin) {
    state.editingId = task ? task.id : null;
    el('editTitle').textContent = task ? '编辑任务' : '新建任务';
    el('fDelete').hidden = !task;

    if (task) {
      f.title.value = task.title;
      f.cat.value = catName(task.categoryId) === '未分类' ? '' : catName(task.categoryId);
      f.date.value = task.date;
      f.start.value = task.start;
      f.end.value = task.end;
      f.status.value = task.status;
    } else {
      const idx = dayIdx != null ? dayIdx : 0;
      const s = startMin != null ? startMin : 9 * 60;
      f.title.value = '';
      f.cat.value = '';
      f.date.value = iso(dayAt(idx));
      f.start.value = toHM(s);
      f.end.value = toHM(Math.min(s + 60, 23 * 60 + 59));
      f.status.value = 'todo';
    }
    updateDur();
    renderCatPicks();
    editOverlay.hidden = false;
    setTimeout(() => f.title.focus(), 30);
  }

  function updateDur() {
    const d = parseHM(f.end.value || '00:00') - parseHM(f.start.value || '00:00');
    el('fDur').textContent = d > 0 ? dur(d) : '—';
  }

  f.start.addEventListener('input', updateDur);
  f.end.addEventListener('input', updateDur);
  f.cat.addEventListener('input', renderCatPicks);

  const closeEdit = () => { editOverlay.hidden = true; state.editingId = null; };

  el('fCancel').onclick = closeEdit;
  editOverlay.onclick = (e) => { if (e.target === editOverlay) closeEdit(); };

  el('fDelete').onclick = async () => {
    try {
      await bridge.DeleteTask(state.editingId);
      if (state.selectedId === state.editingId) state.selectedId = null;
      closeEdit(); await reload();
    } catch (e) { reportError('删除失败')(e); }
  };

  el('fSave').onclick = async () => {
    const title = f.title.value.trim();
    if (!title) { f.title.focus(); return; }

    const catNameVal = f.cat.value.trim();
    try {
      // 分类名不存在时,SaveTask 那边不会自动建 —— 先确保它存在
      let categoryId = '';
      if (catNameVal) {
        const hit = state.categories.find((c) => c.name === catNameVal);
        categoryId = hit ? hit.id : (await bridge.SaveCategory('', catNameVal, '')).id;
      }

      const saved = await bridge.SaveTask({
        id: state.editingId || '',
        title,
        categoryId,
        status: f.status.value,
        date: f.date.value,
        start: f.start.value,
        end: f.end.value,
      });
      state.selectedId = saved.id;
      closeEdit(); await reload();
    } catch (e) { reportError('保存失败')(e); }
  };

  // ---------------------------------------------------------------- 确认耗时
  const durOverlay = el('durOverlay');

  function confirmDuration(t) {
    state.durTaskId = t.id;

    // 确认的是"正在跑的这一段":它的时长才是这次投入。之前暂停过的段
    // 已经定死了,不参与这次的调整。
    const open = (t.spans || []).find((s) => s.open);
    const mins = open ? open.minutes : t.durationMin;

    el('durBig').textContent = mins ? dur(mins) : '—';
    el('durSub').textContent = open
      ? `${open.start} 起,到现在 ${dur(mins)},可以改`
      : '这条事项还没有时段,按计划跨度记一段';
    el('durH').value = Math.floor(mins / 60);
    el('durM').value = mins % 60;
    durOverlay.hidden = false;
  }

  const durValue = () => Math.max(
    (Number(el('durH').value) || 0) * 60 + (Number(el('durM').value) || 0), 0);

  ['durH', 'durM'].forEach((id) => {
    el(id).addEventListener('input', () => {
      const v = durValue();
      el('durBig').textContent = v ? dur(v) : '—';
    });
  });

  const closeDur = () => { durOverlay.hidden = true; state.durTaskId = null; };
  el('durCancel').onclick = closeDur;
  durOverlay.onclick = (e) => { if (e.target === durOverlay) closeDur(); };

  el('durOk').onclick = async () => {
    const v = durValue();
    try {
      const full = await bridge.FinishTask(state.durTaskId, v);
      replaceTask(full);
    } catch (e) { reportError('结束任务失败')(e); }
    closeDur(); await reload();
  };

  // ---------------------------------------------------------------- 分类维护
  const catOverlay = el('catOverlay');

  function renderCatList() {
    el('catList').innerHTML = state.categories.map((c) => `
      <div class="cat-row">
        <label class="dot" style="background:${c.color}" title="改颜色">
          <input type="color" value="${c.color}" data-color="${c.id}">
        </label>
        <input class="name" value="${esc(c.name)}" data-name="${c.id}">
        <span class="cnt">${c.count || 0} 项</span>
        <button class="del" data-del="${c.id}" title="删除">×</button>
      </div>`).join('')
      || '<p style="color:var(--text-faint);font-size:12.5px;padding:8px 0">还没有分类</p>';

    document.querySelectorAll('[data-color]').forEach((node) => {
      node.oninput = async () => {
        const c = catById(node.dataset.color);
        try { await bridge.SaveCategory(c.id, c.name, node.value); await reload(); renderCatList(); }
        catch (e) { reportError('改颜色失败')(e); }
      };
    });
    document.querySelectorAll('input.name[data-name]').forEach((node) => {
      node.onchange = async () => {
        const c = catById(node.dataset.name);
        const n = node.value.trim();
        if (!n || n === c.name) { node.value = c.name; return; }
        try { await bridge.SaveCategory(c.id, n, c.color); await reload(); renderCatList(); }
        catch (e) { reportError('改名失败')(e); node.value = c.name; }
      };
    });
    document.querySelectorAll('[data-del]').forEach((node) => {
      node.onclick = async () => {
        try { await bridge.DeleteCategory(node.dataset.del); await reload(); renderCatList(); }
        catch (e) { reportError('删除分类失败')(e); }
      };
    });
  }

  el('btnCats').onclick = () => { renderCatList(); catOverlay.hidden = false; };
  el('catDone').onclick = () => { catOverlay.hidden = true; };
  catOverlay.onclick = (e) => { if (e.target === catOverlay) catOverlay.hidden = true; };
  el('catAdd').onclick = async () => {
    try { await bridge.SaveCategory('', '新分类', ''); await reload(); renderCatList(); }
    catch (e) { reportError('新增分类失败')(e); }
  };

  // ---------------------------------------------------------------- 周汇总
  const sumOverlay = el('sumOverlay');

  async function renderSummary() {
    const from = iso(weekStart());
    const to = iso(new Date(weekStart().getTime() + 6 * DAY));

    let rep;
    try { rep = await bridge.Report(from, to); }
    catch (e) { reportError('取汇总失败')(e); return; }

    const a = weekStart(), b = dayAt(6);
    el('sumTitle').textContent =
      `周汇总 · ${a.getFullYear()} 年 ${a.getMonth() + 1} 月 ${a.getDate()} – ${b.getDate()} 日`;

    const body = el('sumBody');
    if (!rep.taskCount) {
      body.innerHTML = '<p style="color:var(--text-faint);font-size:13px;padding:24px 0;text-align:center">这一周还没有记录</p>';
      return;
    }

    // 环形图:conic-gradient 直接按占比切分
    let acc = 0;
    const stops = rep.groups.map((g) => {
      const from2 = acc, to2 = acc + g.percent;
      acc = to2;
      return `${g.color} ${from2.toFixed(3)}% ${to2.toFixed(3)}%`;
    }).join(', ');

    body.innerHTML = `
      <div class="sum-top">
        <div class="donut-wrap">
          <div class="donut" style="background:conic-gradient(${stops})"></div>
          <div class="donut-center">
            <div class="v">${(rep.totalMinutes / 60).toFixed(1)}<span>h</span></div>
            <div class="k">本周总时长</div>
          </div>
        </div>
        <div class="legend">
          ${rep.groups.map((g) => `
            <div class="legend-row">
              <i class="swatch" style="background:${g.color}"></i>
              <span class="n">${esc(g.name)}</span>
              <span class="h">${dur(g.minutes)}</span>
              <span class="p">${g.percent.toFixed(0)}%</span>
            </div>`).join('')}
        </div>
      </div>
      ${rep.overlapMinutes > 0 ? `<p style="color:var(--text-faint);font-size:12px;margin-top:10px">
        注:其中 ${dur(rep.overlapMinutes)} 是并行推进、被重复计算的(墙钟实际 ${dur(rep.unionMinutes)})</p>` : ''}

      <div class="sum-title">按分类明细 · ${rep.taskCount} 项</div>
      ${rep.groups.map((g) => `
        <div class="cat-group">
          <div class="gh">
            <i class="swatch" style="background:${g.color}"></i>
            <b>${esc(g.name)}</b>
            <span class="meta">${dur(g.minutes)} · ${g.percent.toFixed(0)}% · ${g.tasks.length} 项</span>
          </div>
          <div class="items">
            ${g.tasks.map((t) => {
              const d = new Date(t.date + 'T00:00:00');
              return `<div class="it">
                <span class="t">${esc(t.title)}</span>
                <span class="d">${DOW[(d.getDay() + 6) % 7]} ${d.getMonth() + 1}/${d.getDate()}</span>
                <span class="h">${dur(t.durationMin)}</span>
              </div>`;
            }).join('')}
          </div>
        </div>`).join('')}`;
  }

  el('btnSummary').onclick = async () => { await renderSummary(); sumOverlay.hidden = false; };
  el('sumClose').onclick = () => { sumOverlay.hidden = true; };
  sumOverlay.onclick = (e) => { if (e.target === sumOverlay) sumOverlay.hidden = true; };

  // ---------------------------------------------------------------- MCP 接入
  async function refreshMCP() {
    let st;
    try { st = await bridge.MCPStatus(); } catch { return; }

    const hint = el('mcpHint');
    const text = el('mcpText');
    const cmd = el('mcpCmd');
    const btn = el('mcpInstall');

    if (st.installed && st.matches) {
      hint.style.display = 'none';
      return;
    }
    hint.style.display = '';

    if (!st.cli) {
      text.textContent = '没找到 claude 命令 —— 装好 Claude Code 后,把下面这条粘进去就能接上';
      cmd.textContent = await bridge.MCPCommand();
      cmd.hidden = false;
      btn.hidden = true;
      return;
    }
    if (st.installed && !st.matches) {
      text.textContent = 'Claude Code 里配的地址或令牌和当前不一致 —— 重新注册一次就好';
    } else {
      text.textContent = 'Claude Code 还没连上 worklog —— 装一次就能让它帮你记录和出周报';
    }
    cmd.textContent = st.url;
    cmd.hidden = false;
    btn.hidden = false;
  }

  el('mcpInstall').onclick = async () => {
    const btn = el('mcpInstall');
    btn.disabled = true;
    btn.textContent = '安装中…';
    try {
      const out = await bridge.InstallMCP();
      el('mcpText').textContent = '已写入 Claude Code 配置,重启 Claude Code 后生效';
      el('mcpCmd').textContent = out || '';
      el('mcpCmd').hidden = !out;
      btn.hidden = true;
      setTimeout(refreshMCP, 1200);
    } catch (e) {
      reportError('安装失败')(e);
      btn.disabled = false;
      btn.textContent = '一键安装';
    }
  };

  // ---------------------------------------------------------------- 主题
  const THEME_KEY = 'worklog.theme';
  const SUN = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><circle cx="8" cy="8" r="3.2"/><path d="M8 1v1.6M8 13.4V15M15 8h-1.6M2.6 8H1M12.9 3.1l-1.1 1.1M4.2 11.8l-1.1 1.1M12.9 12.9l-1.1-1.1M4.2 4.2L3.1 3.1"/></svg>';
  const MOON = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"><path d="M13.5 9.6A5.8 5.8 0 0 1 6.4 2.5a5.8 5.8 0 1 0 7.1 7.1z"/></svg>';

  function applyTheme(t) {
    document.documentElement.dataset.theme = t;
    el('btnTheme').innerHTML = t === 'dark' ? SUN : MOON;
    el('btnTheme').title = t === 'dark' ? '切到浅色' : '切到深色';
    try { localStorage.setItem(THEME_KEY, t); } catch { /* 无痕模式等,忽略 */ }
  }

  let savedTheme = 'dark';
  try { savedTheme = localStorage.getItem(THEME_KEY) || 'dark'; } catch { /* ignore */ }
  applyTheme(savedTheme);
  el('btnTheme').onclick = () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  };

  // ---------------------------------------------------------------- 顶部交互
  el('prev').onclick = async () => { state.weekOffset--; await reload(); };
  el('next').onclick = async () => { state.weekOffset++; await reload(); };
  el('todayBtn').onclick = async () => { state.weekOffset = 0; await reload(); };
  el('btnNew').onclick = () => {
    const idx = Array.from({ length: 7 }, (_, i) => i).find(isToday);
    openEdit(null, idx != null ? idx : 0);
  };

  // ---------------------------------------------------------------- 状态视角
  //
  // 切视角只改"画什么",不动数据。选中项如果被过滤掉了就一并取消选中,
  // 否则详情栏会一直显示一条在当前视角里看不见的事项。
  function renderStatusFilter() {
    el('statusFilter').querySelectorAll('button').forEach((b) => {
      b.classList.toggle('on', b.dataset.status === state.statusFilter);
    });
  }

  el('statusFilter').onclick = (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    state.statusFilter = btn.dataset.status || '';
    const sel = selected();
    if (sel && state.statusFilter && sel.status !== state.statusFilter) {
      state.selectedId = null;
    }
    renderStatusFilter();
    refreshRange();
    renderAll();
  };
  renderStatusFilter();

  // ---------------------------------------------------------------- 右键菜单
  //
  // 右键某个任务块直接操作,不用先选中再点详情栏。菜单内容跟着状态走:
  // 待办给"开始",跑着的给"暂停",暂停中的给"继续"。
  const ctxMenu = el('ctxMenu');

  function hideCtx() {
    ctxMenu.hidden = true;
    ctxMenu.innerHTML = '';
  }

  function ctxItems(t) {
    const out = [];
    if (t.status === 'doing') {
      out.push(t.running ? { k: 'pause', label: '暂停' } : { k: 'resume', label: '继续' });
      out.push({ k: 'finish', label: '结束并确认耗时' });
    } else if (t.status === 'todo') {
      out.push({ k: 'start', label: '开始计时' });
      out.push({ k: 'finish', label: '直接结束' });
    } else {
      out.push({ k: 'restart', label: '重新开始' });
    }
    out.push({ sep: true });
    out.push({ k: 'edit', label: '编辑', hint: '双击' });
    out.push({ k: 'delete', label: '删除', danger: true });
    return out;
  }

  function openCtx(t, x, y) {
    ctxMenu.innerHTML =
      `<div class="ct-title">${esc(t.title)}</div>` +
      ctxItems(t).map((it) => it.sep
        ? '<div class="ct-sep"></div>'
        : `<button data-k="${it.k}"${it.danger ? ' class="danger"' : ''}>${it.label}${
            it.hint ? `<span class="k">${it.hint}</span>` : ''}</button>`).join('');
    ctxMenu.hidden = false;

    // 先显示再量尺寸,才能把它夹在视口里 —— 否则贴着右下角右键会跑出屏幕
    const r = ctxMenu.getBoundingClientRect();
    ctxMenu.style.left = Math.max(4, Math.min(x, window.innerWidth - r.width - 8)) + 'px';
    ctxMenu.style.top = Math.max(4, Math.min(y, window.innerHeight - r.height - 8)) + 'px';
  }

  ctxMenu.onclick = async (e) => {
    const btn = e.target.closest('button[data-k]');
    if (!btn) return;
    const k = btn.dataset.k;
    const t = selected();
    if (!t) { hideCtx(); return; }

    // 删除要点两次。右键顺手删掉一条记录太容易了,而这是不可撤销的
    if (k === 'delete' && !btn.classList.contains('arming')) {
      btn.classList.add('arming');
      btn.textContent = '再点一次确认删除';
      return;
    }
    hideCtx();

    try {
      switch (k) {
        case 'pause':
          replaceTask(await bridge.PauseTask(t.id));
          renderAll();
          break;
        case 'start':
        case 'resume':
        case 'restart':
          replaceTask(await bridge.SetTaskStatus(t.id, 'doing'));
          renderAll();
          break;
        case 'finish':
          confirmDuration(t);
          break;
        case 'edit':
          openEdit(t);
          break;
        case 'delete':
          await bridge.DeleteTask(t.id);
          if (state.selectedId === t.id) state.selectedId = null;
          await reload();
          break;
      }
    } catch (err) { reportError('操作失败')(err); }
  };

  // 点别处 / 按 Esc / 滚轮 / 窗口失焦都关掉菜单。
  // 不关的话它会一直挂在那儿,看着像卡住了。
  document.addEventListener('click', (e) => { if (!ctxMenu.contains(e.target)) hideCtx(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideCtx(); });
  document.addEventListener('scroll', hideCtx, true);
  window.addEventListener('resize', hideCtx);
  window.addEventListener('blur', hideCtx);

  // 时间轴空白处的右键:吞掉 WebView2 自带的菜单(在这儿它没有意义),
  // 顺手关掉我们自己的。任务块自己的处理里 stopPropagation 了,不会走到这。
  el('ganttBody').addEventListener('contextmenu', (e) => {
    e.preventDefault();
    hideCtx();
  });

  const autoBox = el('autoRange'), hS = el('hStart'), hE = el('hEnd');
  autoBox.onchange = () => {
    state.autoRange = autoBox.checked;
    hS.disabled = hE.disabled = state.autoRange;
    refreshRange(); renderAll();
  };
  const applyFixed = () => {
    state.fixedStart = Math.max(0, Math.min(23, Number(hS.value) || 0));
    state.fixedEnd = Math.max(state.fixedStart + 1, Math.min(24, Number(hE.value) || 1));
    if (!state.autoRange) { refreshRange(); renderAll(); }
  };
  hS.onchange = applyFixed;
  hE.onchange = applyFixed;

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!durOverlay.hidden) closeDur();
    else if (!sumOverlay.hidden) sumOverlay.hidden = true;
    else if (!catOverlay.hidden) catOverlay.hidden = true;
    else if (!editOverlay.hidden) closeEdit();
  });

  function reportError(prefix) {
    return (err) => {
      const msg = typeof err === 'string' ? err : (err?.message ?? String(err));
      const t = el('toast') || document.createElement('div');
      console.error(prefix, msg);
      if (Go && typeof Go.ClientLog === 'function') {
        try { Go.ClientLog(`${prefix}: ${msg}`); } catch { /* ignore */ }
      }
    };
  }

  // ---------------------------------------------------------------- 刷新
  const REFRESH = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M13.6 8a5.6 5.6 0 1 1-1.7-4"/><path d="M13.9 2.2v3.6h-3.6"/></svg>';
  const AUTO_REFRESH_MS = 30000;

  let reloadTimer = null;
  let reloadDeferred = false;

  // 拉一次数据,并把刷新按钮转一圈 —— 否则点了没反应,人不知道生效没有。
  async function refresh(force) {
    // 用户正在编辑就先不打断;关掉弹窗时 closeEdit 会自己刷一次
    if (!force && !el('editOverlay').hidden) { reloadDeferred = true; return; }
    reloadDeferred = false;

    const btn = el('btnRefresh');
    btn.classList.remove('spinning');
    void btn.offsetWidth; // 强制重排,让动画能连着触发
    btn.classList.add('spinning');

    await reload();
  }

  // MCP 侧的写入也要能立刻看到:Claude 记完一条事项,窗口不该还停在旧数据上。
  // 合并短时间内的多次通知 —— 连着记三条不该刷三次。
  function scheduleReload() {
    if (!el('editOverlay').hidden) { reloadDeferred = true; return; }
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => refresh(), 150);
  }

  el('btnRefresh').innerHTML = REFRESH;
  el('btnRefresh').onclick = () => refresh(true);

  // 兜底轮询:事件丢了、或者数据是被别的进程改的(比如另开了一个实例)时还有救
  setInterval(() => {
    if (document.visibilityState === 'visible') scheduleReload();
  }, AUTO_REFRESH_MS);

  async function init() {
    await reload();
    refreshMCP();

    if (window.runtime?.EventsOn) {
      window.runtime.EventsOn('worklog:changed', scheduleReload);
    }
    // 事件也可能是在窗口最小化时错过的,重新聚焦再兜一次
    window.addEventListener('focus', scheduleReload);
  }
  init();
})();
