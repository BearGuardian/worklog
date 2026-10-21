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
  // 秒级时长。只给"正在跑"的读数用 —— 静止的值精确到秒是噪音,
  // 走着的值精确到秒才让人相信它真的在走
  const durSec = (min) => {
    const t = Math.max(Math.floor(min * 60), 0);
    const h = Math.floor(t / 3600), m = Math.floor((t % 3600) / 60), s = t % 60;
    if (h) return `${h}h${pad(m)}m${pad(s)}s`;
    return m ? `${m}m${pad(s)}s` : `${s}s`;
  };
  const iso = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const parseHM = (s) => { const [h, m] = String(s).split(':').map(Number); return (h || 0) * 60 + (m || 0); };
  const toHM = (min) => `${pad(Math.floor(min / 60))}:${pad(min % 60)}`;

  // 当前时刻的分钟数,取整到 5 分钟。
  //
  // 取整是为了让时间栏里出现 14:30 而不是 14:32 —— 粒度够细,不会明显偏离"现在"。
  // 上限夹到 23:55:23:58 向上取整会变成 24:00,那不是合法的 time 值。
  const nowMin = () => { const d = new Date(); return d.getHours() * 60 + d.getMinutes(); };
  const snap5 = (m) => Math.min(Math.round(m / 5) * 5, 23 * 60 + 55);
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
    // 'timeline' | 'req'。两个视图的数据互不相干,切换是把主区整个换掉
    view: 'timeline',
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

    // ---- 需求视图
    components: [],
    requirements: [],
    reqStats: null,
    stages: [],       // [{value, label}],顺序由后端给
    reqEditingId: null,
    reqCompFilter: '', // 组件筛选,空 = 全部
    // 汇总弹窗里的季度筛选,空 = 全部。只影响汇总,不影响看板
    reqQuarter: '',
    // 分类和组件共用一套 CRUD 弹窗,这个决定它现在在维护哪个
    crudKind: 'cat',
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

  const compById = (id) => state.components.find((c) => c.id === id) || null;
  const compColor = (id) => (compById(id) || {}).color || 'var(--todo)';
  const compName = (id) => (compById(id) || {}).name || '未指定组件';

  // ---------------------------------------------------------------- 走表
  //
  // 后端给的 durationMin 是取数那一刻的值,30 秒的轮询间隔里它会停住 ——
  // 而"做了多久"正是盯着看的时候最不该冻住的数字。所以界面上自己一秒算一次。

  // 还在跑的那一段从开始到现在过了多少秒。没有在跑的段返回 null。
  function liveSeconds(t) {
    const open = (t.spans || []).find((s) => s.open);
    if (!open) return null;
    const started = new Date(`${open.date}T${open.start}:00`).getTime();
    if (Number.isNaN(started)) return null;
    return Math.max((Date.now() - started) / 1000, 0);
  }

  // 事项此刻的总时长(分钟,带小数)。已暂停 / 已完成的原样返回后端算好的值。
  //
  // 算法:拿后端的合计减掉那一段的旧读数,再加上此刻的实时值 ——
  // 这样"逐段取整再求和"的口径不变,只有正在跑的那一段是活的。
  function liveMinutes(t) {
    const secs = liveSeconds(t);
    if (secs == null) return t.durationMin;
    const open = t.spans.find((s) => s.open);
    return (t.durationMin - open.minutes) + secs / 60;
  }

  // 有输入框的弹窗开着时不该重画:重画会把手打到一半的字冲掉
  const editing = () => !el('editOverlay').hidden || !el('reqOverlay').hidden;

  // 时间轴那一侧要隐藏的控件。需求视图里它们全都说不通。
  // 汇总不在里面 —— 两个视图都能汇总,只是汇总的东西不同
  const TIMELINE_ONLY = ['rangeCtl', 'statusFilter', 'btnCats', 'weekNav'];

  const pct = (min) => ((min / 60 - state.dayStart) / state.span) * 100;

  // ---------------------------------------------------------------- 取数
  async function reload() {
    const from = iso(weekStart());
    const to = iso(new Date(weekStart().getTime() + 6 * DAY));
    try {
      const [tasks, cats, board, comps, stages] = await Promise.all([
        bridge.ListTasks(from, to),
        bridge.ListCategories(),
        bridge.RequirementBoard(),
        bridge.ListComponents(),
        bridge.RequirementStages(),
      ]);
      state.tasks = tasks || [];
      state.categories = cats || [];
      // 需求这一份总是拉:数据量小,按视图懒加载只会多一处"切过去是空的"的时机
      state.requirements = board?.requirements || [];
      state.reqStats = board?.stats || null;
      state.components = comps || [];
      state.stages = stages || [];
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
    const req = state.view === 'req';

    // 顶栏按视图换装:时间轴的控件在需求视图里全都说不通,反之亦然
    renderViewSwitch();
    TIMELINE_ONLY.forEach((id) => { el(id).hidden = req; });
    el('btnComps').hidden = !req;
    el('btnNew').textContent = req ? '+ 新建需求' : '+ 新建任务';

    el('main').classList.toggle('req-mode', req);
    el('ganttPanel').hidden = req;
    el('detail').hidden = req;
    el('reqPanel').hidden = !req;

    renderTop();
    renderStats();
    if (req) { renderReqs(); return; }
    renderHead(); renderGantt(); renderDetail();
  }

  // 高亮必须在这里跟着 state.view 走,不能只在点击处理里改 ——
  // 否则任何别的入口改了 view,按钮就会停在上一个视图上,和主区对不上。
  function renderViewSwitch() {
    el('viewSwitch').querySelectorAll('button').forEach((b) => {
      b.classList.toggle('on', b.dataset.view === state.view);
    });
  }

  function renderTop() {
    if (state.view === 'req') return; // 需求不是按周看的,rangeLabel 这会儿是隐藏的
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
      if (t.date !== dateISO) return [];
      // 未定结束(跨度零长):不画时长,但**得画出来** ——
      // 不画的话新建的任务会直接从周视图里消失,像是没建上。
      if (t.endMin <= t.startMin) return [{ start: t.startMin, end: t.startMin, plan: true, openEnd: true }];
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
    if (state.view === 'req') return renderReqStats();

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
          if (sg.openEnd) node.classList.add('open-end');

          const h = pct(sg.end) - pct(sg.start);
          // 未定结束的块不加 .short —— 那个类会把第二行藏起来,
          // 而「未定结束」正要显示在那儿。高度交给 CSS 的 min-height。
          // 太矮就只留标题一行。阈值按**像素**判而不是按百分比 ——
          // 百分比和实际高度脱钩:矮窗口下 6% 只有二十来像素,两行内容挤在
          // 里面比只有标题还难看。甘特图的高度渲染时就知道,直接用。
          const bodyH = body.clientHeight || 700;
          const px = (h / 100) * bodyH;
          if (px < 38 && !sg.openEnd) node.classList.add('short');

          node.style.left = `calc(${sg._lane * w}% + 3px)`;
          node.style.right = `calc(${(lanes - sg._lane - 1) * w}% + 3px)`;
          node.style.top = pct(sg.start) + '%';
          if (!sg.openEnd) node.style.height = `calc(${h}% - 3px)`;

          const plain = sg.openEnd ? '未定结束' : sg.plan ? '计划' : dur(sg.end - sg.start);
          const range = sg.openEnd ? `${toHM(sg.start)}–` : `${toHM(sg.start)}–${toHM(sg.end)}`;
          node.title = `${t.title}\n${catName(t.categoryId)} · ${range} · ${plain} · ${statusLabel(t)}\n双击编辑`;

          // 还在跑的那一块:时长文字一秒一跳,块本身也跟着长高。
          // 只标记,值交给 tickLive 算 —— 重画整个甘特图会把悬停和选中态弄丢
          const label = sg.open
            ? `<span data-live="${t.id}" data-fmt="min">${plain}</span>`
            : plain;
          if (sg.open) {
            node.dataset.grow = t.id;
            node.dataset.startMin = sg.start;
            node.dataset.day = iso(dayAt(i));
          }

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

  // 备注框跟着内容长高。多行备注是常态,固定两行会让人写到第三行就开始
  // 在框里搓滚轮 —— 那正是"太小了"的来源
  const growNote = (ta) => {
    if (!ta) return;
    ta.style.height = 'auto';
    ta.style.height = Math.min(ta.scrollHeight, 180) + 'px';
  };

  // 详情栏是可以整个重画的(30 秒轮询、MCP 写入、删时段都会触发)。
  // 重画会把正在写的备注冲掉 —— 打到一半没了,比框小得多难受。
  // 所以画之前把草稿记下来,画完连光标位置一起补回去。
  function captureNoteDraft(host) {
    const ta = host.querySelector('#noteText');
    if (!ta) return null;
    const focused = document.activeElement === ta;
    if (!ta.value && !focused) return null; // 没东西可保
    // 记下是哪个任务的框 —— 切到别的事項时草稿要丢掉,不能串到别人身上
    return { taskId: host.dataset.taskId || '', value: ta.value,
             start: ta.selectionStart, end: ta.selectionEnd, focused };
  }

  function restoreNoteDraft(host, d, taskId) {
    if (!d || d.taskId !== taskId) return;
    const ta = host.querySelector('#noteText');
    if (!ta) return;
    ta.value = d.value;
    growNote(ta);
    if (d.focused) {
      ta.focus();
      ta.setSelectionRange(d.start, d.end);
    }
  }

  function renderDetail() {
    const host = el('detail');
    const draft = captureNoteDraft(host);
    const t = selected();
    if (!t) {
      host.dataset.taskId = '';
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
        <div><div class="k">实际投入${t.confirmed ? '' : '(未确认)'}</div>
          <div class="v${t.running ? ' live' : ''}" data-live="${t.id}" data-fmt="sec">${t.durationMin ? dur(t.durationMin) : '—'}</div></div>
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
                    <span>${s.open
                      ? `<b data-live="${t.id}" data-fmt="min">${dur(s.minutes)}</b> 且还在走`
                      : dur(s.minutes)}</span>
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
              <div class="note${n.who === 'claude' ? ' mcp' : ''}" data-note="${n.id}">
                <div class="rail"><i></i></div>
                <div class="body">
                  <div class="who"><b>${n.who === 'claude' ? 'Claude' : '我'}</b><span>${n.at}</span>${
                    n.edited ? '<span class="edited">已编辑</span>' : ''}</div>
                  <p>${esc(n.text)}</p>
                </div>
                <button class="note-act" data-note-edit="${n.id}" title="编辑">编辑</button>
                <button class="note-act del" data-note-del="${n.id}" title="删除">×</button>
              </div>`).join('')
          : `<div class="note"><div class="rail"><i></i></div>
               <div class="body"><p style="color:var(--text-faint)">还没有备注</p></div></div>`}
      </div>

      <div class="note-add">
        <textarea id="noteText" rows="2" placeholder="加一条备注…(Enter 提交,Shift+Enter 换行)"></textarea>
        <button id="noteAdd" title="添加">+</button>
      </div>

      <div class="detail-foot">
        <button class="text-btn" id="btnEdit">编辑</button>
        ${t.status === 'doing' ? `<button class="text-btn" id="btnPause">${t.running ? '暂停' : '继续'}</button>` : ''}
        <button class="primary-btn" id="btnToggle">
          ${t.status === 'doing' ? '结束并确认耗时' : t.status === 'done' ? '重新开始' : '开始计时'}
        </button>
      </div>`;

    restoreNoteDraft(host, draft, t.id);
    host.dataset.taskId = t.id;

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
        el('noteText').value = ''; // 提交成功才清空,失败要留着(见下面 reportError)
        replaceTask(full); renderDetail(); renderGantt();
      } catch (e) { reportError('添加备注失败')(e); }
    };

    // Enter 提交、Shift+Enter 换行 —— 聊天框的习惯。换行留给真正需要多行的人,
    // 否则一行备注要按 Shift 才能提交,反了。
    el('noteText').onkeydown = (e) => {
      if (e.key !== 'Enter' || e.shiftKey) return;
      e.preventDefault();
      el('noteAdd').click();
    };
    el('noteText').oninput = () => growNote(el('noteText'));
    el('noteText').onblur = () => {
      // 有焦点时憋住的刷新在离开时补上,不然详情栏会一直停在旧数据上
      if (reloadDeferred) { reloadDeferred = false; scheduleReload(); }
    };

    // 备注的编辑 / 删除。两个按钮平时是透明的,悬停才显形 ——
    // 备注主要是拿来读的,不该一排按钮抢视线。
    host.querySelectorAll('[data-note-edit]').forEach((btn) => {
      btn.onclick = (e) => { e.stopPropagation(); startNoteEdit(t, btn.dataset.noteEdit); };
    });
    host.querySelectorAll('.note[data-note] .body').forEach((body) => {
      body.onclick = () => startNoteEdit(t, body.closest('.note').dataset.note);
    });
    host.querySelectorAll('[data-note-del]').forEach((btn) => {
      btn.onclick = async (e) => {
        e.stopPropagation();
        try {
          await bridge.DeleteNote(btn.dataset.noteDel);
          replaceTask(await bridge.GetTask(t.id));
          renderDetail(); renderGantt();
        } catch (err) { reportError('删除备注失败')(err); }
      };
    });

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

  // 把一条备注就地变成可编辑的。不跳出去、不弹窗 —— 时间线原地改。
  //
  // 保存走 Ctrl+Enter(或点保存),**失焦不保存**:备注常常是多行的,
  // 写着写着点到别处太常见了,自动保存要么丢内容要么误存半截。
  function startNoteEdit(t, noteId) {
    const n = (t.notes || []).find((x) => x.id === noteId);
    const node = el('detail').querySelector(`.note[data-note="${noteId}"]`);
    if (!n || !node) return;
    const body = node.querySelector('.body');
    if (body.querySelector('textarea')) return; // 已经在这个编辑态里了,别再套一层

    body.innerHTML = `
      <div class="who"><b>${n.who === 'claude' ? 'Claude' : '我'}</b><span>${n.at}</span>${
        n.edited ? '<span class="edited">已编辑</span>' : ''}</div>
      <textarea class="note-edit" rows="2">${esc(n.text)}</textarea>
      <div class="note-edit-foot">
        <button class="text-btn" data-cancel>取消</button>
        <button class="primary-btn" data-save>保存</button>
      </div>`;

    const ta = body.querySelector('textarea');
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length); // 光标落到末尾,接着改最顺

    const cancel = () => renderDetail(); // 重画一遍就是还原,不用手工回滚 DOM
    const save = async () => {
      const v = ta.value.trim();
      if (!v) { ta.focus(); return; } // 改成空不算删除,见 store.UpdateNote
      try {
        replaceTask(await bridge.UpdateNote(noteId, v));
        renderDetail(); renderGantt();
      } catch (e) { reportError('改备注失败')(e); }
    };

    body.querySelector('[data-cancel]').onclick = cancel;
    body.querySelector('[data-save]').onclick = save;
    ta.onkeydown = (e) => {
      if (e.key === 'Escape') { e.stopPropagation(); cancel(); }
      else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); save(); }
    };
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
      f.end.value = task.openEnd ? '' : task.end; // 未定结束就留空,别填个假的
      f.status.value = task.status;
    } else {
      const idx = dayIdx != null ? dayIdx : 0;
      // 没指定时刻就用"现在"取整到 5 分钟 —— 默认 09:00 的话,下午打开弹窗
      // 得先改开始、再拉结束才存得上。在空白格子上点是另一回事:
      // 那个手势本身就表达了"我要这个时刻",照用点击位置。
      const s = startMin != null ? startMin : snap5(nowMin());
      f.title.value = '';
      f.cat.value = '';
      f.date.value = iso(dayAt(idx));
      f.start.value = toHM(s);
      f.end.value = ''; // 结束时间默认留空 = 未定结束
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

  const closeEdit = () => {
    editOverlay.hidden = true;
    state.editingId = null;
    // 弹窗开着时憋住的刷新(定时轮询 / MCP 事件)在这里补上,不然关掉后
    // 要等下一个 30 秒周期才更新
    if (reloadDeferred) { reloadDeferred = false; scheduleReload(); }
  };

  el('fCancel').onclick = closeEdit;
  editOverlay.onclick = (e) => { if (e.target === editOverlay) closeEdit(); };

  el('fDelete').onclick = async () => {
    try {
      await bridge.DeleteTask(state.editingId);
      if (state.selectedId === state.editingId) state.selectedId = null;
      closeEdit(); await reload();
    } catch (e) { reportError('删除失败')(e); }
  };

  // 保存。startNow 为真表示点的是「开始计时」—— 存下来并立刻开始走表。
  //
  // 这时日期和开始时刻**强制成此刻**:按钮的意思就是"现在开始做这件事",
  // 照表单里那个值开会把之前改过的时间也算进去,时长从一开头就是错的。
  async function saveTask(startNow) {
    const title = f.title.value.trim();
    if (!title) { f.title.focus(); return; }

    if (startNow) {
      const now = new Date();
      f.date.value = iso(now);
      f.start.value = toHM(snap5(now.getHours() * 60 + now.getMinutes()));
      f.end.value = ''; // 还没结束,结束时间当然也还没有
    }

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
        status: startNow ? 'doing' : f.status.value,
        date: f.date.value,
        start: f.start.value,
        end: f.end.value,
      });

      // 新建时状态给了 doing,CreateTask 已经顺带开了一段;但编辑一条暂停中的
      // 事项时不会 —— SetTaskStatus 会把那一段补上,已经在跑则幂等返回。
      if (startNow) await bridge.SetTaskStatus(saved.id, 'doing');

      state.selectedId = saved.id;
      closeEdit(); await reload();
    } catch (e) { reportError('保存失败')(e); }
  }

  el('fSave').onclick = () => saveTask(false);
  // 注意 id 是 fStartNow 不是 fStart —— 后者是上面那个"开始时间"输入框。
  // 两个用同一个 id 的话 getElementById 只会拿到先出现的那个输入框,
  // 按钮永远接不到点击(踩过)
  el('fStartNow').onclick = () => saveTask(true);

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
    el('durNote').value = ''; // 每次打开都是空的,别把上一条的内容带过来
    durOverlay.hidden = false;
    // 焦点给备注而不是时长:时长多数时候不用改,那句话才是不写就忘的
    setTimeout(() => el('durNote').focus(), 30);
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
    const note = el('durNote').value.trim();
    try {
      const full = await bridge.FinishTask(state.durTaskId, v, note);
      replaceTask(full);
    } catch (e) { reportError('结束任务失败')(e); }
    closeDur(); await reload();
  };

  // ---------------------------------------------------------------- 需求看板
  //
  // 时间轴记的是"今天做了什么",看板记的是"这个季度在推什么"。两者共用一套
  // 刷新和事件,但画法完全不同,所以在 renderAll 里就分岔了。
  const reqOverlay = el('reqOverlay');

  // 阶段短名:"需求提交" → "提交"。进度条上五个节点横排,带"需求"两个字会挤成一团。
  // 前缀由后端那套标签统一给出,这里只是去掉它,不另立一份名单。
  const shortStage = (label) => String(label || '').replace(/^需求/, '');

  function renderReqStats() {
    const st = state.reqStats || { total: 0, online: 0, thisQuarter: 0 };
    el('stats').innerHTML = `
      <div class="stat accent"><div class="num">${st.total}<span>条</span></div><div class="lbl">需求总数</div></div>
      <div class="stat done"><div class="num">${st.online}<span>条</span></div><div class="lbl">已上线</div></div>
      <div class="stat working"><div class="num">${st.total - st.online}<span>条</span></div><div class="lbl">推进中</div></div>
      <div class="stat"><div class="num">${st.thisQuarter}<span>条</span></div><div class="lbl">本季度上线</div></div>`;
  }

  // 按组件分组。顺序、数量和完成度都直接取统计里的 —— 自己再折一遍,
  // 就有和头部数字对不上的可能。
  function reqGroups() {
    const st = state.reqStats || { components: [] };
    const byComp = new Map();
    state.requirements.forEach((r) => {
      const k = r.componentId || '';
      if (!byComp.has(k)) byComp.set(k, []);
      byComp.get(k).push(r);
    });

    return st.components
      .filter((c) => (state.reqCompFilter ? c.id === state.reqCompFilter : c.total > 0))
      .map((c) => ({ ...c, items: byComp.get(c.id) || [] }));
  }

  function reqCard(r) {
    const steps = state.stages.map((s, i) => {
      const cls = i === r.stageIndex ? ' on' : (i < r.stageIndex ? ' done' : '');
      return `<i class="step${cls}" data-stage="${s.value}" title="标为${s.label}">${shortStage(s.label)}</i>`;
    }).join('');

    // 链接去掉协议头再显示 —— 一屏卡片里满眼 https:// 是噪音,主机名才是有用的部分
    const link = r.link
      ? `<a class="rc-link" data-url="${esc(r.link)}" title="${esc(r.link)}">${esc(r.link.replace(/^https?:\/\//, ''))}</a>`
      : '';

    return `<div class="req-card${r.stage === 'online' ? ' online' : ''}" data-id="${r.id}"
                 style="--cc:${compColor(r.componentId)}">
      <div class="rc-top">
        <span class="rc-title">${esc(r.title)}</span>
        <span class="rc-badge">${esc(r.stageLabel)}</span>
      </div>
      <div class="steps">${steps}</div>
      <div class="rc-meta">
        ${link}
        <span class="rc-date">${r.start.slice(5)} 起</span>
        ${r.online ? `<span class="rc-online">上线 ${r.online.slice(5)} · ${esc(r.quarter)}</span>` : ''}
      </div>
    </div>`;
  }

  function renderReqs() {
    renderReqHead();

    const groups = reqGroups();
    const body = el('reqBody');

    if (!groups.length) {
      body.innerHTML = `<div class="req-empty">${
        state.requirements.length ? '这个组件下还没有需求' : '还没有需求。点右上角「+ 新建需求」记一条'}</div>`;
      return;
    }

    body.innerHTML = groups.map((g) => `
      <div class="req-group">
        <div class="rg-head">
          <i class="dot" style="background:${g.color || 'var(--todo)'}"></i>
          <span class="rg-name">${esc(g.name)}</span>
          <span class="rg-count">${g.online}/${g.total}</span>
          <span class="rg-bar"><i style="width:${g.percent}%"></i></span>
          <span class="rg-pct">${g.percent.toFixed(0)}%</span>
        </div>
        <div class="rg-cards">${g.items.map(reqCard).join('')}</div>
      </div>`).join('');

    bindReqCards();
  }

  function bindReqCards() {
    document.querySelectorAll('.req-card').forEach((node) => {
      const id = node.dataset.id;

      // 点进度条上的节点 = 直接跳到那个阶段。这是"标记进度"的主路径,一次点击,
      // 不用开弹窗。卡片本身点开弹窗,所以这里必须拦住冒泡。
      node.querySelectorAll('.step').forEach((step) => {
        step.onclick = async (e) => {
          e.stopPropagation();
          await setReqStage(id, step.dataset.stage);
        };
      });

      node.querySelectorAll('[data-url]').forEach((a) => {
        a.onclick = (e) => {
          e.stopPropagation();
          if (Go && typeof Go.OpenURL === 'function') Go.OpenURL(a.dataset.url).catch(reportError('打开链接失败'));
        };
      });

      node.onclick = () => openReqEdit(state.requirements.find((r) => r.id === id) || null);
    });
  }

  function renderReqHead() {
    const st = state.reqStats || { total: 0, online: 0, percent: 0, quarters: [] };

    const chips = st.quarters.map((q) =>
      `<span class="q-chip">${esc(q.label)}<b>${q.online}</b></span>`).join('');

    const opts = state.components.map((c) =>
      `<option value="${c.id}"${c.id === state.reqCompFilter ? ' selected' : ''}>${esc(c.name)}</option>`).join('');

    el('reqHead').innerHTML = `
      <div class="rh-total">
        <div class="rh-num">${st.online}<span>/${st.total}</span></div>
        <div class="rh-lbl">已上线 · ${st.percent.toFixed(0)}%</div>
        <div class="rh-bar"><i style="width:${st.percent}%"></i></div>
      </div>
      <div class="rh-quarters">
        <span class="rh-cap">按季度</span>
        ${chips || '<span class="rh-none">还没有上线的需求</span>'}
      </div>
      <select id="reqCompFilter" title="只看某个组件">
        <option value="">全部组件</option>${opts}
      </select>`;

    el('reqCompFilter').onchange = (e) => {
      state.reqCompFilter = e.target.value;
      renderReqs();
    };
  }

  // 标记阶段。切到「上线」且还没日期时,后端会记成现在 —— 这里不重复那套规则。
  async function setReqStage(id, stage) {
    try {
      await bridge.SetRequirementStage(id, stage, '');
      await reload();
    } catch (e) {
      reportError('改阶段失败')(e);
    }
  }

  // ---------------------------------------------------------------- 新建 / 编辑需求
  const r = {
    title: el('rTitle'), comp: el('rComp'), link: el('rLink'),
    detail: el('rDetail'), stage: el('rStage'), online: el('rOnline'),
  };

  function renderCompPicks() {
    const cur = r.comp.value.trim();
    el('rCompPicks').innerHTML = state.components.map((c) =>
      `<button type="button" class="cat-pick${c.name === cur ? ' on' : ''}"
               data-name="${esc(c.name)}" style="--pc:${c.color}">
         <i class="swatch"></i>${esc(c.name)}
       </button>`).join('')
      + (cur && !state.components.some((c) => c.name === cur)
        ? `<span class="cat-pick" style="cursor:default"><span class="new">新建「${esc(cur)}」</span></span>` : '');

    document.querySelectorAll('#rCompPicks .cat-pick[data-name]').forEach((node) => {
      node.onclick = () => { r.comp.value = node.dataset.name; renderCompPicks(); };
    });
  }

  // 阶段下拉的选项从后端拿。顺序和中文名只在 model.Stages 里定义一次,
  // 不在 HTML 里再抄一份 —— 加一个阶段时两边都得改,迟早对不上
  function renderStageOptions() {
    el('rStage').innerHTML = state.stages
      .map((s) => `<option value="${s.value}">${esc(s.label)}</option>`).join('');
  }

  function openReqEdit(req) {
    state.reqEditingId = req ? req.id : null;
    el('reqTitle').textContent = req ? '编辑需求' : '新建需求';
    el('rDelete').hidden = !req;

    r.title.value = req ? req.title : '';
    r.comp.value = req && req.componentId ? compName(req.componentId) : '';
    r.link.value = req ? req.link : '';
    r.detail.value = req ? req.detail : '';
    // 先填选项再赋值,否则 value 落不到任何一项上,下拉是空的
    renderStageOptions();
    r.stage.value = req ? req.stage : 'submit';
    r.online.value = req ? req.online : '';

    renderCompPicks();
    reqOverlay.hidden = false;
    setTimeout(() => r.title.focus(), 30);
  }

  async function saveReq() {
    const title = r.title.value.trim();
    if (!title) { r.title.focus(); return; }

    try {
      await bridge.SaveRequirement({
        id: state.reqEditingId || '',
        title,
        component: r.comp.value.trim(),
        link: r.link.value.trim(),
        detail: r.detail.value.trim(),
        stage: r.stage.value,
        onlineDate: r.online.value,
      });
      reqOverlay.hidden = true;
      await reload();
    } catch (e) {
      reportError('保存需求失败')(e);
    }
  }

  r.comp.oninput = renderCompPicks;
  el('rSave').onclick = saveReq;
  el('rCancel').onclick = () => { reqOverlay.hidden = true; };
  reqOverlay.onclick = (e) => { if (e.target === reqOverlay) reqOverlay.hidden = true; };
  el('rDelete').onclick = async () => {
    if (!state.reqEditingId) return;
    try {
      await bridge.DeleteRequirement(state.reqEditingId);
      reqOverlay.hidden = true;
      await reload();
    } catch (e) {
      reportError('删除需求失败')(e);
    }
  };
  // 阶段下拉和上线时间是一件事的两面,但后端会自己收敛,这里不做联动校验 ——
  // 在前端拦一道只会让人以为"填了日期却没生效"
  el('rStage').onchange = () => { if (r.stage.value === 'online' && !r.online.value) r.online.focus(); };

  // ---------------------------------------------------------------- 分类维护
  const catOverlay = el('catOverlay');

  // 分类和组件是同一套 CRUD 界面:一行 = 色块 + 名字 + 用量 + 删除。
  // 只有标题、数据源和调哪个接口不同,所以共用一个渲染函数,不复制第二份。
  const CRUD = {
    cat: {
      title: '分类', addText: '+ 新增分类', noun: '分类', unit: '项',
      list: () => state.categories,
      byId: catById,
      save: (id, name, color) => bridge.SaveCategory(id, name, color),
      del: (id) => bridge.DeleteCategory(id),
      create: () => bridge.SaveCategory('', '新分类', ''),
    },
    comp: {
      title: '组件', addText: '+ 新增组件', noun: '组件', unit: '条',
      list: () => state.components,
      byId: compById,
      save: (id, name, color) => bridge.SaveComponent(id, name, color),
      del: (id) => bridge.DeleteComponent(id),
      create: () => bridge.SaveComponent('', '新组件', ''),
    },
  };

  function openCrud(kind) {
    state.crudKind = kind;
    el('crudTitle').textContent = CRUD[kind].title;
    el('catAdd').textContent = CRUD[kind].addText;
    renderCrudList();
    catOverlay.hidden = false;
  }

  function renderCrudList() {
    const k = CRUD[state.crudKind];

    el('catList').innerHTML = k.list().map((c) => `
      <div class="cat-row">
        <label class="dot" style="background:${c.color}" title="改颜色">
          <input type="color" value="${c.color}" data-color="${c.id}">
        </label>
        <input class="name" value="${esc(c.name)}" data-name="${c.id}">
        <span class="cnt">${c.count || 0} ${k.unit}</span>
        <button class="del" data-del="${c.id}" title="删除">×</button>
      </div>`).join('')
      || `<p style="color:var(--text-faint);font-size:12.5px;padding:8px 0">还没有${k.noun}</p>`;

    document.querySelectorAll('#catList [data-color]').forEach((node) => {
      node.oninput = async () => {
        const c = k.byId(node.dataset.color);
        if (!c) return;
        try { await k.save(c.id, c.name, node.value); await reload(); renderCrudList(); }
        catch (e) { reportError('改颜色失败')(e); }
      };
    });
    document.querySelectorAll('#catList input.name[data-name]').forEach((node) => {
      node.onchange = async () => {
        const c = k.byId(node.dataset.name);
        if (!c) return;
        const n = node.value.trim();
        if (!n || n === c.name) { node.value = c.name; return; }
        try { await k.save(c.id, n, c.color); await reload(); renderCrudList(); }
        catch (e) { reportError('改名失败')(e); node.value = c.name; }
      };
    });
    document.querySelectorAll('#catList [data-del]').forEach((node) => {
      node.onclick = async () => {
        try { await k.del(node.dataset.del); await reload(); renderCrudList(); }
        catch (e) { reportError(`删除${k.noun}失败`)(e); }
      };
    });
  }

  el('btnCats').onclick = () => openCrud('cat');
  el('btnComps').onclick = () => openCrud('comp');
  el('catDone').onclick = () => { catOverlay.hidden = true; };
  catOverlay.onclick = (e) => { if (e.target === catOverlay) catOverlay.hidden = true; };
  el('catAdd').onclick = async () => {
    try { await CRUD[state.crudKind].create(); await reload(); renderCrudList(); }
    catch (e) { reportError('新增失败')(e); }
  };

  // ---------------------------------------------------------------- 周汇总
  const sumOverlay = el('sumOverlay');

  async function renderSummary() {
    // 汇总按钮两个视图都有,但汇总的东西完全不是一回事:时间轴按分类算时长,
    // 需求按组件算条数。所以在这里分岔,弹窗和样式复用同一套
    if (state.view === 'req') return renderReqSummary();

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

  // ---------------------------------------------------------------- 需求汇总
  //
  // 和上面的周汇总同构:环形图 + 图例 + 明细。只是切分的口径从"按分类的时长"
  // 换成"按组件的条数" —— 需求没有时长,能盘的就是数量和完成情况。
  // 数据已经在 state 里,不用再取一次。
  //
  // 季度可以点着筛选:点某一季只看那一季上线的,再点一次回到全部。
  // 筛选只在这个弹窗里生效,不动看板。
  function renderReqSummary() {
    const st = state.reqStats;
    const body = el('sumBody');

    if (!st || !st.total) {
      el('sumTitle').textContent = '需求汇总';
      body.innerHTML = '<p style="color:var(--text-faint);font-size:13px;padding:24px 0;text-align:center">还没有需求</p>';
      return;
    }

    const quarters = st.quarters || [];
    // 筛选的季度要是已经不存在了(数据变了),就当没筛
    if (state.reqQuarter && !quarters.some((q) => q.label === state.reqQuarter)) {
      state.reqQuarter = '';
    }
    const q = state.reqQuarter;

    // 筛选后所有统计都要基于这批需求重算 —— 直接拿 st.components 会混进别的季度
    const reqs = q ? state.requirements.filter((r) => r.quarter === q) : state.requirements;
    const online = reqs.filter((r) => r.stage === 'online').length;
    const pct = reqs.length ? (online / reqs.length) * 100 : 0;

    el('sumTitle').textContent = q
      ? `需求汇总 · ${q} · 已上线 ${online} 条`
      : `需求汇总 · 已上线 ${online}/${reqs.length}`;

    // 按组件分组。顺序跟组件维护列表一致,未指定组件的排最后
    const byComp = new Map();
    reqs.forEach((r) => {
      const k = r.componentId || '';
      if (!byComp.has(k)) byComp.set(k, []);
      byComp.get(k).push(r);
    });
    const groups = [];
    state.components.forEach((c) => {
      const items = byComp.get(c.id);
      if (items && items.length) groups.push({ id: c.id, name: c.name, color: c.color, items });
    });
    if (byComp.get('')) {
      groups.push({ id: '', name: '未指定组件', color: 'var(--todo)', items: byComp.get('') });
    }
    // 兜底:组件列表没取到时不能让需求凭空消失,否则环形图会是一片空白
    const covered = new Set(groups.map((g) => g.id));
    byComp.forEach((items, id) => {
      if (id && !covered.has(id)) {
        groups.push({ id, name: compName(id), color: compColor(id), items });
      }
    });

    let acc = 0;
    const stops = groups.map((g) => {
      const p = (g.items.length / reqs.length) * 100;
      const a = acc, b = acc + p;
      acc = b;
      return `${g.color} ${a.toFixed(3)}% ${b.toFixed(3)}%`;
    }).join(', ');

    // 季度条按最大值归一,否则只有一个季度时永远满格看不出对比。
    // 这一列始终列全部季度 —— 它是筛选控件本身,不能被自己筛掉
    const maxQ = Math.max(...quarters.map((x) => x.online), 1);

    body.innerHTML = `
      <div class="sum-top">
        <div class="donut-wrap">
          <div class="donut" style="background:conic-gradient(${stops})"></div>
          <div class="donut-center">
            <div class="v">${reqs.length}<span>条</span></div>
            <div class="k">${q ? esc(q) : '需求总数'}</div>
          </div>
        </div>
        <div class="legend">
          ${groups.map((g) => `
            <div class="legend-row">
              <i class="swatch" style="background:${g.color}"></i>
              <span class="n">${esc(g.name)}</span>
              <span class="h">${g.items.length} 条</span>
              <span class="p">${((g.items.length / reqs.length) * 100).toFixed(0)}%</span>
            </div>`).join('') || '<div class="legend-row empty"><span class="n">这个季度没有需求</span></div>'}
        </div>
      </div>
      <p class="sum-note">${q
        // 筛了季度就不报完成度了:这一季里的需求按定义都是已上线的,100% 是句废话
        ? `共 ${reqs.length} 条`
        : `已上线 ${online} 条 · 完成度 ${pct.toFixed(0)}%`}</p>

      <div class="sum-title">按季度上线 · ${online} 条 <span class="sum-hint">点季度可以只看那一季</span></div>
      ${quarters.length ? quarters.map((x) => `
        <div class="q-row${x.label === q ? ' on' : ''}" data-q="${esc(x.label)}" title="${x.label === q ? '再看全部季度' : '只看 ' + x.label}">
          <span class="q-name">${esc(x.label)}</span>
          <span class="q-bar"><i style="width:${(x.online / maxQ) * 100}%"></i></span>
          <span class="q-num">${x.online}</span>
        </div>`).join('')
        : '<p style="color:var(--text-faint);font-size:12.5px">还没有上线的需求</p>'}

      <div class="sum-title">${q ? esc(q) + ' 明细' : '按组件明细'} · ${reqs.length} 条</div>
      ${groups.map((g) => {
        const on = g.items.filter((r) => r.stage === 'online').length;
        return `
        <div class="cat-group">
          <div class="gh">
            <i class="swatch" style="background:${g.color}"></i>
            <b>${esc(g.name)}</b>
            <span class="meta">${on}/${g.items.length} 已上线 · ${((on / g.items.length) * 100).toFixed(0)}%</span>
          </div>
          <div class="items">
            ${g.items.map((r) => `
              <div class="it">
                <span class="t">${esc(r.title)}</span>
                <span class="d">${esc(r.stageLabel)}</span>
                <span class="h">${r.online ? r.online.slice(5) : r.start.slice(5) + ' 起'}</span>
              </div>`).join('')}
          </div>
        </div>`;
      }).join('')}`;

    body.querySelectorAll('.q-row').forEach((row) => {
      row.onclick = () => {
        state.reqQuarter = row.dataset.q === state.reqQuarter ? '' : row.dataset.q;
        renderReqSummary();
      };
    });
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

  // 「新建」按当前视图走 —— 同一个按钮,两个视图里建的是两种东西
  el('btnNew').onclick = () => {
    if (state.view === 'req') { openReqEdit(null); return; }
    const idx = Array.from({ length: 7 }, (_, i) => i).find(isToday);
    openEdit(null, idx != null ? idx : 0);
  };

  el('viewSwitch').onclick = (e) => {
    const btn = e.target.closest('button');
    if (!btn || btn.dataset.view === state.view) return;
    state.view = btn.dataset.view;
    renderAll(); // 高亮由 renderViewSwitch 统一负责
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
    else if (!reqOverlay.hidden) reqOverlay.hidden = true;
    else if (!catOverlay.hidden) catOverlay.hidden = true;
    else if (!editOverlay.hidden) closeEdit();
  });

  // 报错要让人看见。原先这里只 console.error + 写日志,界面上一声不吭 ——
  // 保存失败的时候用户只会觉得"点了没反应",根本没有线索可循。
  let toastTimer = null;
  function showToast(text) {
    const t = el('toast');
    if (!t) return;
    t.textContent = text;
    t.classList.add('on');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove('on'), 5000);
  }

  function reportError(prefix) {
    return (err) => {
      const msg = typeof err === 'string' ? err : (err?.message ?? String(err));
      console.error(prefix, msg);
      if (Go && typeof Go.ClientLog === 'function') {
        try { Go.ClientLog(`${prefix}: ${msg}`); } catch { /* ignore */ }
      }
      showToast(`${prefix}:${msg}`);
    };
  }

  // ---------------------------------------------------------------- 刷新
  const REFRESH = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M13.6 8a5.6 5.6 0 1 1-1.7-4"/><path d="M13.9 2.2v3.6h-3.6"/></svg>';
  const AUTO_REFRESH_MS = 30000;

  let reloadTimer = null;
  let reloadDeferred = false;

  // 备注框有焦点时不要重画详情栏:中文输入法正在拼字,
  // 节点一换拼音就丢了。离开时会补刷(见 noteText 的 onblur)
  const noteBusy = () => {
    const ta = document.getElementById('noteText');
    return !!ta && document.activeElement === ta;
  };

  // 拉一次数据,并把刷新按钮转一圈 —— 否则点了没反应,人不知道生效没有。
  async function refresh(force) {
    // 用户正在编辑就先不打断;关掉弹窗时 closeEdit 会自己刷一次
    if (!force && (editing() || noteBusy())) { reloadDeferred = true; return; }
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
    if (editing() || noteBusy()) { reloadDeferred = true; return; }
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => refresh(), 150);
  }

  el('btnRefresh').innerHTML = REFRESH;
  el('btnRefresh').onclick = () => refresh(true);

  // ---------------------------------------------------------------- 走表
  //
  // 一秒一跳,只改文字和高度,不重画 —— 每秒重建一遍甘特图会把悬停、选中
  // 这些状态全弄丢,而且那点活儿本来就是白干的。
  //
  // 数据仍然由 30 秒的轮询负责:这里只是把"正在跑"的那一段在两次取数之间
  // 接着往下走,不改变任何口径。
  function tickLive() {
    if (state.view !== 'timeline') return;
    const running = state.tasks.filter((t) => t.running);
    if (!running.length) return;

    document.querySelectorAll('[data-live]').forEach((node) => {
      const t = running.find((x) => x.id === node.dataset.live);
      if (!t) return;
      const mins = liveMinutes(t);
      const txt = node.dataset.fmt === 'sec' ? durSec(mins) : dur(Math.round(mins));
      if (node.textContent !== txt) node.textContent = txt;
    });

    // 还在跑的那一块跟着长高。只处理今天那一列 —— 跨天的那半截起点在 0 点,
    // 用同一个公式会算错
    const today = iso(new Date());
    document.querySelectorAll('[data-grow]').forEach((node) => {
      if (node.dataset.day !== today) return;
      if (!running.some((x) => x.id === node.dataset.grow)) return;
      const n = new Date();
      const end = n.getHours() * 60 + n.getMinutes() + n.getSeconds() / 60;
      const pct = ((end - Number(node.dataset.startMin)) / 60 / state.span) * 100;
      if (pct > 0) node.style.height = `calc(${pct}% - 3px)`;
    });
  }
  setInterval(tickLive, 1000);

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
