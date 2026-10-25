import { invoke, on, backend } from "./ipc.js";
import { applyStatic, current, resolveLang, setLang, t } from "./i18n.js";

const $ = (id) => document.getElementById(id);
const el = {
  shell: $("shell"), pill: $("pill"), dot: $("dot"), label: $("label"),
  panel: $("panel"), toast: $("toast"),
  gear: $("gear"), pause: $("pause"), quit: $("quit"),
  theme: $("theme"), close: $("close"),
  target: $("target"), browse: $("browse"), open: $("open"), view: $("view"), targetHint: $("target-hint"),
  targetMenu: $("target-menu"), targetPop: $("target-pop"),
  viewlog: $("viewlog"), clearlog: $("clearlog"),
  dateSubdirs: $("dateSubdirs"), toastEnabled: $("toastEnabled"), toastSeconds: $("toastSeconds"),
  source: $("source"), relocate: $("relocate"), sourceHint: $("source-hint"),
  quietMs: $("quietMs"), maxSettleMs: $("maxSettleMs"), lang: $("lang"),
  saveState: $("save-state"), foot: document.querySelector(".foot"),
  aboutVersion: $("about-version"),
  ack: $("ack"), ackDlg: $("ackdlg"), ackClose: $("ackclose"),
};
const tabs = Array.from(document.querySelectorAll(".tab"));
const panes = Array.from(document.querySelectorAll(".pane"));

let st = null;          // 后端给的那份真相（settings + ui + layout）
let expanded = false;
let toastTimer = 0;

const KB = 1024, MB = KB * 1024, GB = MB * 1024;
function human(n) {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n >= GB) return (n / GB).toFixed(n / GB >= 10 ? 0 : 1) + " GB";
  if (n >= MB) return (n / MB).toFixed(1) + " MB";
  if (n >= KB) return Math.round(n / KB) + " KB";
  return n + " B";
}
const base = (p) => String(p || "").replace(/[\\/]+$/, "").split(/[\\/]/).pop() || "";
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

function applyLayout(l) {
  if (!l) return;
  const r = document.documentElement.style;
  r.setProperty("--pill-h", l.pillH + "px");
  r.setProperty("--panel-h", l.panelH + "px");
  r.setProperty("--toast-h", l.toastH + "px");
}

// 药丸上的状态点与那一行字：只由 status 决定，界面不自创第二套说法。
function renderPill(s) {
  const paused = !!s?.paused;
  const watching = !!s?.watching;
  const busy = (s?.pending || 0) > 0 || (s?.live || 0) > 0;
  let k = "idle", key = "pill.watching";
  if (!st) { k = "off"; key = "pill.startup"; }
  else if (!watching && !paused) { k = "bad"; key = "pill.idle"; }
  else if (paused) { k = "paused"; key = "pill.paused"; }
  else if (busy) { k = "busy"; key = (s.live || 0) > 0 ? "pill.downloading" : "pill.moving"; }
  const n = (s?.live || 0) > 0 ? s.live : s?.pending || 0;
  const text = key === "pill.downloading" || key === "pill.moving" ? t(key, { n }) : t(key);
  el.dot.dataset.s = k;
  el.dot.title = text + "　" + (s?.targetDir ? "→ " + s.targetDir : t("pill.noTarget"));
  el.label.textContent = text;
  el.pause.textContent = t(paused ? "pill.resume" : "pill.pause");
  el.pause.title = t(paused ? "pill.resumeTitle" : "pill.pauseTitle");
}

// setVal 不动当前聚焦的那个控件：即时保存成功后要把后端那份真相写回框里（空的、
// 越界的数字会被打回实际生效的值），但用户可能已经跳到下一个框开始打了 ——
// 那时覆盖就等于把他正在输入的东西吞掉。
function setVal(node, v) {
  if (node === document.activeElement) return;
  node.value = v;
}

function renderSettings(s) {
  el.target.value = s.targetDir || "";
  el.dateSubdirs.checked = !!s.dateSubdirs;
  el.toastEnabled.checked = !!s.toastEnabled;
  setVal(el.toastSeconds, s.toastSeconds || 4);
  el.source.value = s.sourceDir || "";
  setVal(el.quietMs, s.quietMs ?? 900);
  setVal(el.maxSettleMs, Math.round((s.maxSettleMs ?? 600000) / 1000));
  setHint(el.targetHint, s.targetDir ? t("hint.movingTo", { dir: s.targetDir }) : t("hint.noTarget"),
    s.targetDir ? "" : "bad");
  renderPresets(s);
}

// —— 转移目录那颗 ▾ ——
//
// presetList 是「按掉一条 ✕ 之后、还没落盘的那一份」，而不是 st.settings 的一个别名：
// 保存失败时它要留在原地，否则下一次保存把刚删掉的那条又送回去，用户看到的是「删不掉」。
let presetList = [];

// sameDir 只比形状，不碰磁盘：它回答的是「这一行是不是正在用的那一个」，
// 而 Windows 的路径里大小写和分隔符方向都不算两个目录（后端 samePath 同一件事）。
const sameDir = (a, b) => {
  const n = (p) => String(p).trim().replace(/[\\/]+$/, "").toLowerCase().replace(/\\/g, "/");
  return n(a) !== "" && n(a) === n(b);
};

function renderPresets(s) {
  presetList = Array.isArray(s.targetDirPresets) ? s.targetDirPresets.slice() : [];
  drawPresets();
}

function drawPresets() {
  const cur = el.target.value.trim();
  el.targetPop.replaceChildren();
  if (!presetList.length) {
    const e = document.createElement("span");
    e.className = "empty";
    e.textContent = t("combo.empty");
    el.targetPop.appendChild(e);
    return;
  }
  for (const dir of presetList) {
    const inUse = sameDir(dir, cur);
    const row = document.createElement("span");
    row.className = "opt" + (inUse ? " cur" : "");

    const pick = document.createElement("button");
    pick.type = "button";
    pick.className = "pick";
    pick.textContent = dir;
    pick.title = inUse ? t("combo.inUse", { dir }) : dir;
    pick.onclick = () => {
      // 程序改输入框不会触发 change，所以这里自己走一次保存：点一下就是换档，没有第二步。
      el.target.value = dir;
      closePresets();
      saveNow();
    };
    row.appendChild(pick);

    // 正在用的那一条不给 ✕：后端每次写盘都把当前落点记回列表最前，
    // 那一行删得掉、下一秒又冒出来，不如根本删不了。
    if (!inUse) {
      const x = document.createElement("button");
      x.type = "button";
      x.className = "x";
      x.textContent = "✕";
      x.title = t("combo.remove");
      x.onclick = () => {
        presetList = presetList.filter((d) => d !== dir);
        drawPresets();
        el.targetMenu.focus(); // 焦点刚被删掉的那个节点带走了，交还给开列表的那颗钮
        saveNow();
      };
      row.appendChild(x);
    }
    el.targetPop.appendChild(row);
  }
}

function openPresets() {
  drawPresets(); // 打开那一刻重画：「正在用」那一记颜色跟着输入框走，不能拿上一次画的
  el.targetPop.hidden = false;
  el.targetMenu.setAttribute("aria-expanded", "true");
}

function closePresets() {
  el.targetPop.hidden = true;
  el.targetMenu.setAttribute("aria-expanded", "false");
}

el.targetMenu.onclick = () => (el.targetPop.hidden ? openPresets() : closePresets());

// 外点即收：只认「点到了这一行和浮层之外」。用 pointerdown 而不是 click ——
// 点输入框、点「打开」那排按钮时，click 要等鼠标抬起之后才轮到，浮层会慢半拍。
document.addEventListener("pointerdown", (e) => {
  if (el.targetPop.hidden) return;
  if (el.targetPop.contains(e.target) || e.target === el.targetMenu) return;
  closePresets();
});

// 列表里的键盘走位：↑↓ 在候选之间移、Esc 收起来并把焦点还给输入框。
// 焦点是移给候选按钮本身而不是 aria-activedescendant：这一层没有输入框可以「指向」。
el.targetPop.addEventListener("keydown", (e) => {
  const picks = Array.from(el.targetPop.querySelectorAll(".pick"));
  const i = picks.indexOf(document.activeElement);
  if (e.key === "Escape") {
    closePresets();
    el.target.focus();
    // 必须挡住这一层：窗口上那个 Escape 是「先近后远」的收三层，而它判远近看的是 hidden ——
    // 不挡的话这里刚把浮层收起，那里就以为「没东西开着」，接着把整个面板一起收了。
    e.stopPropagation();
  } else if (e.key === "ArrowDown") {
    e.preventDefault();
    (picks[i + 1] || picks[0])?.focus();
  } else if (e.key === "ArrowUp" && i > 0) {
    e.preventDefault();
    picks[i - 1].focus();
  }
});

el.target.addEventListener("keydown", (e) => {
  if (e.key !== "ArrowDown" || !el.targetPop.hidden) return;
  e.preventDefault();
  openPresets();
  el.targetPop.querySelector(".pick")?.focus();
});

function dto() {
  return {
    targetDir: el.target.value.trim(),
    targetDirPresets: presetList,
    dateSubdirs: el.dateSubdirs.checked,
    toastEnabled: el.toastEnabled.checked,
    toastSeconds: Number(el.toastSeconds.value) || 4,
    quietMs: Number(el.quietMs.value) || 900,
    maxSettleMs: (Number(el.maxSettleMs.value) || 600) * 1000,
  };
}

// setMode 只改 DOM；窗口尺寸和「往哪边长」都是 Go 说了算，所以以 download2:mode 事件为准。
// drop 是那个方向的唯一真相：DOM 靠 data-drop 翻 order，窗口靠它锚顶边还是锚底边。
// 两边不一致的表现是「面板画在药丸上面、窗口却往下长」——半截被屏幕切掉。
function setMode(on, dir) {
  expanded = !!on;
  el.panel.hidden = !expanded;
  el.shell.dataset.m = expanded ? "panel" : "pill";
  el.shell.dataset.drop = dir === "down" ? "down" : "up";
  el.gear.title = t(expanded ? "btn.gearClose" : "btn.gearOpen");
  // 浮层是 .panel 的后代，面板一 hidden 它就看不见了；但 aria-expanded 还挂在钮上，
  // 不收就等于留下一颗「读屏器说开着、屏幕上没有」的钮。口径同下面那句 setAck。
  if (!expanded) { closePresets(); setAck(false); }
}

// applyTheme 只贴一个 data-theme：所有颜色都在 CSS token 里跟着换。
// 认不出的一律按深色画 —— Go 的 normalize 已经兜过一道，这是第二道，
// 因为漏下去的那个表现是「浅底浅字」，比按钮点不动难查得多。
function applyTheme(name) {
  const light = name === "light";
  document.documentElement.dataset.theme = light ? "light" : "dark";
  el.theme.title = t(light ? "theme.toDark" : "theme.toLight");
  el.theme.setAttribute("aria-pressed", light ? "true" : "false");
}

// applyLang 是 theme 那颗的同一个形状：吃 raw 配置（auto 在这里当场判成 zh/en），
// 换语言就重写静态标签。按钮上的字（「文A」）刻意不进字典 —— 它标的是「另一种」语言，
// 翻成 EN 或 中文 都等于先假定一种起始语言。
function applyLang(code) {
  if (setLang(resolveLang(code, navigator.language))) applyStatic();
  const en = current() === "en";
  el.lang.title = t(en ? "lang.toZh" : "lang.toEn");
  el.lang.setAttribute("aria-pressed", en ? "true" : "false");
}

// 当前是哪一栏只写在 DOM 上（aria-selected + hidden）：收起展开只是给面板加 hidden，
// 节点本身没被重建，所以「回到上次那一栏」是免费的，不需要再造一个模块变量去记。
function setPane(name) {
  for (const t of tabs) t.setAttribute("aria-selected", t.dataset.pane === name ? "true" : "false");
  for (const p of panes) p.hidden = p.dataset.pane !== name;
}

function setHint(node, text, kind) {
  node.textContent = text;
  node.className = "hint" + (kind ? " " + kind : "");
}

// setAck 只有开合：清单是写死在 index.html 的静态内容（正文永久英文，见那里的注释），
// 既没有要渲染的数据也没有后端绑定。开的时候把焦点交给关闭键、还回去时给那颗按钮 ——
// 焦点留在一个已经 hidden 的节点上，等于 Tab 从一个谁也看不见的地方开始。
function setAck(on) {
  if (el.ackDlg.hidden === !on) return;
  el.ackDlg.hidden = !on;
  (on ? el.ackClose : el.ack).focus();
}

function shake() {
  el.pill.classList.remove("shake");
  void el.pill.offsetWidth;
  el.pill.classList.add("shake");
}

function saveState(kind, text) {
  el.saveState.className = "save-state" + (kind ? " " + kind : "");
  el.saveState.textContent = text || "";
  // 没话说就把整行收掉：那根上边框本来只是把「状态行」和字段隔开，
  // 现在没有保存按钮了，一条空荡荡的分隔带在关于页上看起来就像缺一块东西。
  el.foot.hidden = !text;
}

// showToast：卡片画在同一窗口里，所以要让 Go 把窗口往上长一截（setToast）。
async function showToast(kind, text) {
  if (!st?.settings.toastEnabled && kind !== "bad") return;
  el.toast.dataset.k = kind;
  el.toast.querySelector(".t-text").textContent = text;
  el.toast.hidden = false;
  await invoke("SetToast", true);
  clearTimeout(toastTimer);
  const secs = Math.min(Math.max(st?.settings.toastSeconds || 4, 1), 10);
  toastTimer = setTimeout(async () => {
    el.toast.hidden = true;
    await invoke("SetToast", false);
  }, secs * 1000);
}

// resyncSeq 管的是「谁的答案最新」：两次重读可以同时在路上，而慢的那一份回来得更晚。
// 交接补发的那一次和页面自己首屏的那一次正好会撞上（实测：首屏读到旧配色、把它盖在新配色上），
// 所以画之前先问一句「我还是最新那次吗」，不是就把手里这份旧答案扔掉。
let resyncSeq = 0;

async function resync() {
  const seq = ++resyncSeq;
  const r = await invoke("State");
  if (seq !== resyncSeq) return;
  if (!r.ok) { renderPill(null); return; }
  st = r.data;
  // 语言必须在渲染之前定下来：applyStatic 重写的是 index.html 里预置的中文，
  // 而药丸那行字与提示是现查字典 —— 顺序反了就会有一帧两种说法混在一起。
  // 每次 resync 都过一遍：它是幂等的，比记「上次是什么语言」那个变量便宜。
  applyLang(st.settings?.lang);
  applyLayout(st.layout);
  applyTheme(st.settings.theme);
  el.aboutVersion.textContent = st.version || "—";
  renderSettings(st.settings);
  renderPill({ ...st.ui, ...st.settings, ...(st.status || {}) });
  setMode(st.ui?.expanded, st.ui?.drop);
}

el.gear.onclick = () => invoke("SetExpanded", !expanded);
// 关闭 = 和 Esc 同一个出口：不给「收起」再造第二套语义，否则两套实现的差异
// 早晚变成「按 ✕ 收起来了但窗口还留着面板那么高」这种查不动的毛病。
el.close.onclick = () => invoke("SetExpanded", false);
for (const t of tabs) t.onclick = () => setPane(t.dataset.pane);
el.ack.onclick = () => setAck(true);
el.ackClose.onclick = () => setAck(false);
el.theme.onclick = async () => {
  const next = st?.settings.theme === "light" ? "dark" : "light";
  const r = await invoke("SetTheme", next);
  if (!r.ok) { saveState("bad", r.err.msg); shake(); return; }
  if (st) st.settings.theme = r.data.theme;
  applyTheme(r.data.theme);
};
// 语言当场落盘并生效，口径同 theme：它不是一个「等待保存的差量」。
// 换完直接 resync —— 让后端那份真相重画整个界面，比在这里手写「哪几块要重画」的清单可靠，
// 那份清单早晚漏一块（漏掉的那块就是「切了英文但药丸还是中文」这种报告）。
// 两态按钮只写显式的 zh/en：auto 是首次安装那一次猜测，不是一个档位（见设计文档）。
el.lang.onclick = async () => {
  const r = await invoke("SetLang", current() === "en" ? "zh" : "en");
  if (!r.ok) { saveState("bad", r.err.msg); shake(); return; }
  await resync();
};
el.pause.onclick = async () => {
  const r = await invoke("SetPaused", !(st?.settings.paused ?? false));
  if (!r.ok) { showToast("bad", r.err.msg); return; }
  if (st) st.settings.paused = !st.settings.paused;
  renderPill(r.data);
};
el.quit.onclick = () => invoke("Quit");

el.browse.onclick = async () => {
  const r = await invoke("BrowseTarget");
  if (!r.ok) { saveState("bad", r.err.msg); shake(); return; }
  el.target.value = r.data?.targetDir || "";
  // 选完当场生效：这里没有「再点一次保存」那第二步了。
  await saveNow();
};
el.open.onclick = async () => {
  const r = await invoke("OpenTargetDir");
  if (!r.ok) { saveState("bad", r.err.msg); shake(); }
};
// 「查看」= 今天的落点。目录还不存在不是失败：Go 那边刻意不建目录
// （一个「看一眼」不该留写盘副作用），而是退到转移目录，所以这里要说清打开的是哪一个 ——
// 不写清的话用户会在转移目录里找「今天的文件」，而它们其实躺在别的日期档里。
el.view.onclick = async () => {
  const r = await invoke("OpenTodayDir");
  if (!r.ok) { setHint(el.targetHint, r.err.msg, "bad"); shake(); return; }
  // 局部叫 d 不叫 t：t() 是字典那个函数，同名会把它在这一段里盖掉。
  const d = r.data || {};
  if (!d.opened) {
    setHint(el.targetHint, t("hint.nothingOpened", { dir: d.dir }), "bad");
    return;
  }
  if (d.opened !== d.dir) {
    setHint(el.targetHint, t("hint.todayFallback", { dir: d.opened }), "");
    return;
  }
  setHint(el.targetHint, t("hint.opened", { dir: d.dir }), "good");
};
// 「查看日志」开的是搬运流水 moves.jsonl。还没有搬运记录时 Go 那边不报错也不建文件，
// 这里就正常说一句「还没生成」—— 它和「打不开」是两回事，混成红字用户会以为程序坏了。
el.viewlog.onclick = async () => {
  const r = await invoke("OpenLog");
  if (!r.ok) { setHint(el.targetHint, r.err.msg, "bad"); shake(); return; }
  const d = r.data || {};
  if (!d.opened) { setHint(el.targetHint, t("hint.noLogYet", { path: d.path }), ""); return; }
  setHint(el.targetHint, t("hint.opened", { dir: d.path }), "good");
};
// 「清空日志」按用户要求不问第二次。文案要把清了哪两份、以及没清哪一份写出来：
// 一个没有确认的按钮，唯一该给的就是「它到底动了什么」的交代。
el.clearlog.onclick = async () => {
  const r = await invoke("ClearLog");
  if (!r.ok) { setHint(el.targetHint, r.err.msg, "bad"); shake(); return; }
  setHint(el.targetHint, t("hint.cleared"), "good");
};
el.relocate.onclick = async () => {
  const r = await invoke("RelocateSource");
  if (!r.ok) { setHint(el.sourceHint, r.err.msg, "bad"); return; }
  el.source.value = r.data?.sourceDir || "";
  if (st) st.settings.sourceDir = r.data?.sourceDir || "";
  // 这个按钮当场落盘并生效：监控目录在界面上是只读的，唯一的改法就是它，
  // 「再点一次保存才生效」等于把一件事拆成两步、只在第二步才校验。
  setHint(el.sourceHint, t("hint.relocated"), "good");
};

// —— 即时保存 ——
//
// 没有保存按钮：控件值一变就落盘。用 change 而不是 input —— 数字框在失焦或回车时才
// change，勾一下复选框立刻 change。用 input 的话，每打一个字符就写一次盘，而且
// 「D:\半」这种只打了一半的路径会被当成一次待保存的差量送去校验、报一片红。
//
// 保存串成一条链：两次改动挨得近时（勾完提示再改秒数）后一次的回复不能盖掉前一次，
// 而 doSave 是在真正执行时才读 dto()，所以链子排到谁头上就用谁那一刻的值。
let saving = Promise.resolve();
function saveNow() {
  // catch 只为护住链条：一次意外抛出若留在 rejected 上，之后每一次保存都不会再执行。
  saving = saving.then(doSave).catch(() => {});
  return saving;
}

async function doSave() {
  const r = await invoke("Save", dto());
  if (!r.ok) {
    // 报错时绝不 renderSettings：那会把用户刚打错、正打算改的那个字段擦回旧值，
    // 于是红字说的「转移目录不能是空的」和他眼里的「明明写着东西」对不上。
    saveState("bad", r.err.msg);
    shake();
    return;
  }
  st = { ...st, settings: r.data.settings ?? st?.settings, status: r.data.status ?? st?.status };
  renderSettings(st.settings); // 空框、越界数字在这里被打回实际生效的值
  saveState("good", t("hint.saved"));
  setTimeout(() => { if (el.saveState.className.includes("good")) saveState("", ""); }, 1500);
}

// 只认这些控件。转移目录是可编辑的 combobox，走的是同一条 change（失焦或回车，不是每打一个字符），
// 所以「D:\半」那种打了一半的路径不会被当成一次待保存的差量送去校验；
// 监控目录那个框仍是只读的，只有「重新定位」那颗按钮改它，改了当场落盘。
for (const e of [el.target, el.dateSubdirs, el.toastEnabled, el.toastSeconds, el.quietMs, el.maxSettleMs]) {
  e.addEventListener("change", saveNow);
}

on("download2:mode", (d) => setMode(d?.expanded, d?.drop));
on("download2:status", (d) => renderPill({ ...(st?.settings || {}), ...(d || {}) }));
on("download2:moved", (d) => {
  // reason/copy/debt 这三段是拼进一句里的，所以英文那一份自带前导空格或冒号（见字典）。
  const copy = d?.method === "copy" ? t("toast.copy") : "";
  const debt = d?.pendingDelete ? t("toast.debt") : "";
  showToast("good", t("toast.moved", { name: base(d?.dst) }) + copy + debt);
});
on("download2:stuck", (d) => showToast("bad", t("toast.stuck", { path: base(d?.path), reason: d?.reason || "" })));
on("download2:aborted", (d) => showToast("warn", t("toast.aborted", { path: base(d?.path) })));
// 欠界面的话有两句：一次没被听见的交接，和一条没轮上的启动参数。
// 两条都有两条到法：事件推过来，或者首屏加载完自己来取一次。
// 只靠推不行 —— 实测第二次启动赶在页面挂上监听之前，事件按后端说法发出去了，屏幕却没动；
// 而启动那一次页面必然还没挂上，所以参数那句只能靠取。取是一次性的，不会重复说。
async function speakHandoff() {
  const r = await invoke("TakeHandoff");
  if (!r.ok) return;
  const pending = !!r.data?.pending;
  const notice = r.data?.notice || "";
  if (!pending && !notice) return;
  await resync(); // 先重读：开口用的语言、配色得是参数要的那一套
  // 两句不拼成一句：toast 只有两行（.t-text 的 -webkit-line-clamp: 2），拼起来必然截尾。
  // 参数那一句优先 —— 打错字的人要看的是「哪一条没轮上、认哪几个词」，
  // 而「没有多挂一颗药丸」那一局由这句红字本身回答：第二次启动的话被这颗药丸读到了。
  if (notice) { showToast("bad", notice); return; }
  showToast("good", t("toast.secondLaunch"));
}
on("download2:secondLaunch", speakHandoff);

window.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  // 一个键三件事，先近后远：开着什么就先收什么。顺序反了的表现是
  // 「按了一下 Esc，浮层还在，整个面板却收掉了」——那一下没法撤销。
  if (!el.ackDlg.hidden) { setAck(false); return; }
  if (!el.targetPop.hidden) { closePresets(); el.target.focus(); return; }
  if (expanded) invoke("SetExpanded", false);
});

await resync();
await speakHandoff(); // 首屏挂上监听之前欠下的那两句话，在这里补上
if (backend === "stub") {
  el.label.dataset.stub = "1";
  document.title = t("doc.stubTitle");
}
