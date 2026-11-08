// 各页共用：侧栏导航、阶段切换、契约表渲染、节点详情弹窗、目录高亮。
// 节点契约的数据在 contracts.js，这里只负责呈现。
(function(){
  var PAGES = [
    ['方案', [
      ['index.html', '总览'],
      ['device.html', '领夹与手机'],
      ['cloud-asr.html', '云端识别与判定'],
      ['text.html', '入库与下游'],
      ['eval.html', '规格与评测'],
      ['platform.html', '服务、数据与删除']
    ]],
    ['详细设计', [
      ['design-device.html', '端侧'],
      ['design-asr.html', '识别'],
      ['design-speaker.html', '说话人与热词'],
      ['design-text.html', '裁决、文字与删除']
    ]]
  ];
  var root = document.documentElement;
  var here = document.body.getAttribute('data-page') || 'index.html';

  function el(tag, attrs, html){
    var e = document.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function(k){ e.setAttribute(k, attrs[k]); });
    if (html != null) e.innerHTML = html;
    return e;
  }
  // 链接一律写成 页面#锚点；指向本页的改成 #锚点，避免整页重载
  function norm(scope){
    scope.querySelectorAll('a[href]').forEach(function(a){
      var h = a.getAttribute('href');
      if (h.indexOf(here + '#') === 0) a.setAttribute('href', h.slice(here.length));
    });
  }

  // ---- 侧栏 ----
  var side = document.getElementById('side');
  var nav = '';
  PAGES.forEach(function(g){
    nav += '<h4>' + g[0] + '</h4><nav class="toc">';
    g[1].forEach(function(p){
      var cur = p[0] === here;
      nav += '<a href="' + p[0] + '"' + (cur ? ' class="pg" aria-current="page"' : '') + '>' + p[1] + '</a>';
      if (cur) {
        document.querySelectorAll('main > section[id]').forEach(function(s){
          var h = s.querySelector('h2'); if (!h) return;
          var no = h.querySelector('.no'), t = h.textContent.replace(no ? no.textContent : '', '').trim();
          nav += '<a class="sub" href="#' + s.id + '">' + (no ? '<span class="n">' + no.textContent.replace('§', '') + '</span>' : '') + t + '</a>';
        });
      }
    });
    nav += '</nav>';
  });
  side.innerHTML =
    '<a class="brand" href="index.html">领夹 ASR 整体方案</a>' +
    '<div><h4>阶段视图</h4><div class="seg" role="group" aria-label="切换一期或二期视图">' +
      '<button type="button" data-phase-btn="1" aria-pressed="false">一期 · 无 VPU</button>' +
      '<button type="button" data-phase-btn="2" aria-pressed="true">二期 · 硬件</button></div>' +
      '<p class="muted small" style="margin-top:6px">二期才有的部件在一期视图中变灰、虚线框显示。</p></div>' +
    '<div class="navs">' + nav + '</div>' +
    '<div><h4>闸门（按最严处置着色）</h4><div class="legend">' +
      '<div class="row"><svg width="26" height="28" viewBox="-13 -14 26 28"><use href="#hex" class="g-drop"/></svg>可丢弃数据　G0 G1 G2 G4</div>' +
      '<div class="row"><svg width="26" height="28" viewBox="-13 -14 26 28"><use href="#hex" class="g-iso"/></svg>最多降为隔离　G5 G6</div>' +
      '<div class="row"><svg width="26" height="28" viewBox="-13 -14 26 28"><use href="#hex" class="g-tag"/></svg>只打标签　G3（二期可选跳过）</div>' +
      '<div class="row"><svg width="26" height="28" viewBox="-13 -14 26 28"><use href="#hex" class="g-verdict"/></svg>汇总裁决　G7</div>' +
      '<div class="row"><svg width="26" height="28" viewBox="-13 -14 26 28"><use href="#hex" class="g-store"/></svg>入库规则　G8</div></div></div>' +
    '<div><h4>连线</h4><div class="legend">' +
      '<div class="row"><svg width="36" height="10"><path class="e" d="M2 5H34"/></svg>数据流</div>' +
      '<div class="row"><svg width="36" height="10"><path class="e d" d="M2 5H34"/></svg>回执 / 用户纠错</div>' +
      '<div class="row"><svg width="36" height="10"><path class="e dot" d="M2 5H34"/></svg>存储读写</div></div></div>' +
    '<div><h4>数字标注</h4><div class="legend">' +
      '<div class="row"><span class="tg as" style="margin:0">假设</span>需求方给定的前提</div>' +
      '<div class="row"><span class="tg ok" style="margin:0">已确认</span>已在现有设备核实</div>' +
      '<div class="row"><span class="tg es" style="margin:0">估算</span>推算，须实测校准</div>' +
      '<div class="row"><span class="tg go" style="margin:0">目标</span>验收目标，非承诺</div>' +
      '<div class="row"><span class="tg ask" style="margin:0">待询价</span>待询价 / 待确认</div></div></div>';

  // ---- 阶段切换：默认二期 ----
  var btns = document.querySelectorAll('[data-phase-btn]');
  function setPhase(p){
    root.setAttribute('data-phase', p);
    btns.forEach(function(b){ b.setAttribute('aria-pressed', b.getAttribute('data-phase-btn') === p ? 'true' : 'false'); });
    try { localStorage.setItem('lav-phase', p); } catch(e) {}
  }
  var saved = null;
  try { saved = localStorage.getItem('lav-phase'); } catch(e) {}
  setPhase(saved === '1' ? '1' : '2');
  btns.forEach(function(b){ b.addEventListener('click', function(){ setPhase(b.getAttribute('data-phase-btn')); }); });

  // ---- L1 缩略图：标出放大自 L0 的哪一段 ----
  var names = ['领夹','手机','识别与判定','入库与下游'];
  var widths = [30,34,52,26];
  document.querySelectorAll('.minimap').forEach(function(m){
    var seg = +m.getAttribute('data-seg'), x = 1, parts = '';
    widths.forEach(function(w, i){
      var on = i === seg;
      parts += '<rect x="'+x+'" y="2" width="'+w+'" height="18" rx="3" fill="'+(on?'var(--accent)':'var(--surface-2)')+'" stroke="var(--line)"/>';
      x += w + 3;
    });
    m.innerHTML = '<svg width="'+(x)+'" height="22" viewBox="0 0 '+x+' 22" aria-hidden="true">'+parts+'</svg><span>放大自 L0 · '+names[seg]+'</span>';
  });

  // ---- 契约表：按 data-rows 渲染；定义在别的图的行标 ↗ 并链回定义处 ----
  function homeHref(key){ return FIGS[NC[key].fig].page + '#nc-' + key; }
  function figName(fig){ return FIGS[fig].short || FIGS[fig].name; }
  document.querySelectorAll('details.nc').forEach(function(d){
    var fig = d.id.replace(/^nc-of-/, '');
    d.querySelectorAll('tbody[data-rows]').forEach(function(tb){
      tb.getAttribute('data-rows').split(/\s+/).forEach(function(k){
        var r = NC[k]; if (!r) return;
        var home = r.fig === fig, tr = el('tr', {'data-name': r.name});
        if (home) tr.id = 'nc-' + k; else tr.classList.add('ref');
        if (r.cls) r.cls.split(' ').forEach(function(c){ tr.classList.add(c); });
        var th = el('th', {scope: 'row'});
        th.appendChild(document.createTextNode(r.name));
        th.appendChild(el('a', {'class': 'sec', href: r.ref}, r.refLabel));
        if (r.dd) th.appendChild(el('a', {'class': 'dd', href: r.dd}, '详细设计'));
        if (!home) th.appendChild(el('a', {href: homeHref(k)}, '定义见 ' + figName(r.fig)));
        tr.appendChild(th);
        r.cells.forEach(function(c){ tr.appendChild(el('td', {'data-k': c[0]}, c[1])); });
        tb.appendChild(tr);
      });
    });
  });
  // 详细设计页：每节标题旁链回契约
  document.querySelectorAll('[data-dd-of]').forEach(function(h){
    h.getAttribute('data-dd-of').split(/\s+/).forEach(function(k){
      if (!NC[k]) return;
      h.appendChild(el('a', {'class': 'ncl', href: homeHref(k)}, NC[k].name));
    });
  });
  norm(document);

  // ---- 模块详情：悬停显示，点按固定 ----
  var tip = document.getElementById('tip'), pinned = null;
  function place(t){
    var r = t.getBoundingClientRect(), tw = tip.offsetWidth, th = tip.offsetHeight;
    var left = Math.min(Math.max(8, r.left + r.width/2 - tw/2), window.innerWidth - tw - 8);
    var top = r.bottom + 8;
    if (top + th > window.innerHeight - 8) top = Math.max(8, r.top - th - 8);
    tip.style.left = left + 'px'; tip.style.top = top + 'px';
  }
  function show(t){
    var key = t.getAttribute('data-nc'), r = NC[key];
    if (!r) return;
    tip.textContent = '';
    var h = el('p', {'class': 'tt-h'}); h.appendChild(el('b')).textContent = r.name; tip.appendChild(h);
    var dl = el('dl');
    r.cells.forEach(function(c){ dl.appendChild(el('dt')).textContent = c[0]; dl.appendChild(el('dd', null, c[1])); });
    tip.appendChild(dl);
    var nav = el('p', {'class': 'tt-a'});
    nav.appendChild(el('a', {href: r.ref}, '查看 ' + r.refLabel));
    if (r.dd) nav.appendChild(el('a', {href: r.dd}, '详细设计'));
    nav.appendChild(el('a', {href: homeHref(key)}, '在契约表中查看'));
    tip.appendChild(nav);
    norm(tip);
    tip.querySelectorAll('.tt-a a').forEach(function(a){ a.addEventListener('click', function(){ if (a.getAttribute('href').charAt(0) === '#' && a.getAttribute('href').indexOf('#nc-') !== 0) hide(); }); });
    tip.hidden = false; tip.scrollTop = 0; place(t);
  }
  function reveal(id){
    var row = document.getElementById(id);
    if (!row) return false;
    var d = row.closest('details'); if (d) d.open = true;
    row.scrollIntoView({block:'center'});
    document.querySelectorAll('.nct tr.hit').forEach(function(x){ x.classList.remove('hit'); });
    row.classList.add('hit');
    setTimeout(function(){ row.classList.remove('hit'); }, 2400);
    return true;
  }
  document.addEventListener('click', function(e){
    var a = e.target.closest && e.target.closest('a[href^="#nc-"]');
    if (!a) return;
    var id = a.getAttribute('href').slice(1);
    if (reveal(id)) { e.preventDefault(); try { history.replaceState(null, '', '#' + id); } catch(err) {} hide(); }
  });
  if (location.hash.indexOf('#nc-') === 0) reveal(location.hash.slice(1));
  // 鼠标只靠悬停：离开节点后稍等片刻再关，留时间把指针移进弹窗点链接；
  // 点按固定只给触屏与触控笔（没有悬停可用）。键盘靠焦点显示、失焦关闭。
  var hideTimer = null, lastPointer = 'mouse';
  function hide(){ clearTimeout(hideTimer); hideTimer = null; pinned = null; tip.hidden = true; }
  function hideSoon(){ clearTimeout(hideTimer); hideTimer = setTimeout(function(){ if (!pinned) hide(); }, 150); }
  function keep(){ clearTimeout(hideTimer); hideTimer = null; }
  document.addEventListener('pointerdown', function(e){ lastPointer = e.pointerType || 'mouse'; }, true);
  document.querySelectorAll('[data-nc]').forEach(function(t){
    t.addEventListener('pointerenter', function(e){ if (e.pointerType === 'mouse') { keep(); pinned = null; show(t); } });
    t.addEventListener('pointerleave', function(e){ if (e.pointerType === 'mouse') hideSoon(); });
    t.addEventListener('focus', function(){ if (lastPointer === 'mouse' && t.matches(':hover')) return; keep(); show(t); });
    t.addEventListener('blur', function(e){ if (!pinned && !tip.contains(e.relatedTarget)) hideSoon(); });
    t.addEventListener('click', function(e){
      e.stopPropagation();
      if (lastPointer === 'mouse') return;
      if (pinned === t) { hide(); return; }
      keep(); show(t); pinned = t;
    });
    t.addEventListener('keydown', function(e){ if (e.key === 'Enter') { var a = tip.querySelector('a'); if (a) a.click(); } });
  });
  tip.addEventListener('pointerenter', function(e){ if (e.pointerType === 'mouse') keep(); });
  tip.addEventListener('pointerleave', function(e){ if (e.pointerType === 'mouse' && !pinned) hideSoon(); });
  tip.addEventListener('focusout', function(e){ if (!pinned && !tip.contains(e.relatedTarget)) hideSoon(); });
  document.addEventListener('click', function(e){ if (!tip.hidden && !tip.contains(e.target)) hide(); });
  document.addEventListener('keydown', function(e){ if (e.key === 'Escape') hide(); });
  window.addEventListener('scroll', function(){ if (!tip.hidden) hide(); }, {passive:true});

  // ---- 目录高亮 ----
  var links = {};
  side.querySelectorAll('.toc a.sub').forEach(function(a){ links[a.getAttribute('href').slice(1)] = a; });
  if ('IntersectionObserver' in window) {
    var io = new IntersectionObserver(function(entries){
      entries.forEach(function(en){
        if (en.isIntersecting && links[en.target.id]) {
          Object.keys(links).forEach(function(k){ links[k].removeAttribute('aria-current'); });
          links[en.target.id].setAttribute('aria-current', 'true');
        }
      });
    }, {rootMargin: '-20% 0px -70% 0px'});
    document.querySelectorAll('main > section').forEach(function(s){ io.observe(s); });
  }
  // 表格在加载后才渲染：带锚点打开时补一次定位
  if (location.hash && location.hash.indexOf('#nc-') !== 0) {
    var t0 = document.getElementById(location.hash.slice(1));
    if (t0) { var d0 = t0.closest('details'); if (d0) d0.open = true; t0.scrollIntoView(); }
  }
})();
