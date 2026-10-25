// 页内假 Go：没有 wails 运行时时的替身，让药丸与面板在纯浏览器里就能被看见。
//
// 它只替到「信封与事件」这一层：校验规则照抄一份是为了让界面上的每条中文都看得见，
// 但真正的判定与搬运仍然在 Go 那边 —— 本文件里的一切读数都不得记成原生验证。
// 窗口几何在这里也是假的：拖不动，展开只用 CSS 换尺寸（probe 的图注要写明）。

const listeners = new Map();

const Events = {
  On(name, fn) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(fn);
    return () => Events.Off(name, fn);
  },
  Off(name, fn) { listeners.get(name)?.delete(fn); },
  Emit(name, ...data) {
    for (const fn of listeners.get(name) ?? []) fn({ data: data.length === 1 ? data[0] : data });
  },
};

const SRC = "C:\\Users\\you\\Downloads";
const ok = (data) => ({ ok: true, data, err: null });
const bad = (code, msg) => ({ ok: false, data: null, err: { code, msg } });

let settings = {
  version: 1,
  sourceDir: SRC,
  targetDir: "F:\\归档",
  // 常用转移目录：与 settings.json 的 targetDirPresets 同一形状，最近用的在最前。
  // 这里给两条，是为了在纯浏览器里就能看见那颗 ▾ 展开成什么样子。
  targetDirPresets: ["F:\\归档", "D:\\下载归档\\2026", "\\\\nas\\影音"],
  dateSubdirs: true,
  toastEnabled: true,
  toastSeconds: 4,
  paused: false,
  quietMs: 900,
  maxSettleMs: 600000,
  theme: "dark",
  lang: "auto",
};
// drop 在这里是假的：桩没有窗口几何可量，方向只能由 scenes.flipDrop 手动翻，
// 为的是能在浏览器里看见「面板排在药丸下面」那一档长什么样。
const ui = { expanded: false, drop: "up", toastOn: false, watching: true };

// layout 三个数必须和 ui.go 的常量一致，否则假环境里看着对、真窗口里就裁字。
const layout = { pillH: 40, panelH: 340, toastH: 52, pillW: 236, panelW: 360 };

let todayExists = true;
let handoffPending = false;
let owedNotice = "";
// 搬运流水在不在。桩里不碰真文件，所以这一位是给「还没有搬运记录」那条文案翻的。
// 清空之后它仍是 true：真后端就地截断，路径还在，下一次「查看日志」开得了一个空文件。
let movesExists = true;
const MOVES_LOG = "C:\\Users\\you\\AppData\\Roaming\\download2\\moves.jsonl";
const RUNTIME_LOG = "D:\\dev\\download2\\build\\bin\\download2_log.txt";

const methods = {
  "main.UI.State": () => ok({ settings, ui, layout, status: status(), backend: "stub", version: "dev-stub" }),

  // 桩里不会有第二次启动，也不会有人打错参数，所以这两位平时都不欠；
  // scenes.handoff() / scenes.badFlag() 各演一次「页面没听见」。
  "main.UI.TakeHandoff": () => {
    const v = { pending: handoffPending, notice: owedNotice };
    handoffPending = false;
    owedNotice = "";
    return ok(v);
  },

  "main.UI.SetExpanded": (v) => {
    ui.expanded = !!v;
    if (!ui.expanded) ui.drop = "up"; // 与 Go 同一条约定：收起时方向复位
    Events.Emit("download2:mode", { expanded: ui.expanded, drop: ui.drop });
    return ok({ expanded: ui.expanded, drop: ui.drop });
  },

  "main.UI.SetToast": (v) => {
    ui.toastOn = !!v;
    return ok({ toast: ui.toastOn });
  },

  "main.UI.SetTheme": (name) => {
    settings.theme = name === "light" ? "light" : "dark"; // 认不出的一律按深色，同 Hub.SetTheme
    return ok({ theme: settings.theme });
  },

  // 认不出的值回落 auto，同 Hub.SetLang。桩里它不落盘（这里没有盘），只改内存那一份。
  "main.UI.SetLang": (code) => {
    settings.lang = code === "zh" || code === "en" ? code : "auto";
    return ok({ lang: settings.lang });
  },

  "main.UI.SetPaused": (v) => {
    settings.paused = !!v;
    ui.watching = !settings.paused;
    Events.Emit("download2:status", status());
    return ok(status());
  },

  "main.UI.Save": (dto) => {
    const e = check(dto);
    if (e) return e;
    Object.assign(settings, dto);
    // 与 Hub.Save 同一件事：当前落点被记回列表最前（去重、上限 8），
    // 而界面没送列表（旧桩、或只改别的控件）时那份清单原样留着。
    const t = settings.targetDir;
    const rest = (dto.targetDirPresets || []).filter((d) => d && d !== t);
    settings.targetDirPresets = t ? [t, ...rest].slice(0, 8) : rest.slice(0, 8);
    Events.Emit("download2:status", status());
    // 与 ui.go 的 Save 同一份形状：{settings, status}，前端据此重画而不是自己拼状态。
    return ok({ settings, status: status() });
  },

  "main.UI.BrowseTarget": () => ok({ targetDir: "F:\\下载归档\\2026" }),
  "main.UI.OpenTargetDir": () => ok(null),
  // 与 Hub.TodayDir 同一个形状，且同样**不建目录**：exists=false 是一个答案不是失败，
  // 那时 opened 退到转移目录（桩里假定转移目录总在，所以 opened="" 那条只能靠原生验）。
  "main.UI.OpenTodayDir": () => {
    if (!settings.targetDir) return bad("E_NOT_CONFIGURED", "还没设转移目录，没有「今天」可看。");
    const dated = settings.dateSubdirs ? "2026-10-08" : "";
    const dir = settings.targetDir + (dated ? "\\" + dated : "");
    return ok({ dir, dated, exists: todayExists, opened: todayExists ? dir : settings.targetDir });
  },
  // 与 main.UI.OpenLog 同一个形状：文件还没生成时 opened=false，而不是回一个错误。
  // 真后端这里跑的是 notepad.exe，桩里当然开不了窗口 —— 能验的只有那两句文案。
  "main.UI.OpenLog": () => ok({ path: MOVES_LOG, opened: movesExists }),
  "main.UI.ClearLog": () => ok({ moves: MOVES_LOG, runtime: RUNTIME_LOG }),
  "main.UI.RelocateSource": () => ok({ sourceDir: SRC }),
  "main.UI.Quit": () => ok(null),
};

// check 照抄 config/validate.go 的码与中文，好在浏览器里把每条错误文案都走一遍。
function check(dto) {
  const t = (dto.targetDir || "").trim();
  if (!t) return bad("E_INVALID_ARG", "转移目录不能是空的。");
  if (!/^([A-Za-z]:\\|\\\\)/.test(t)) return bad("E_INVALID_ARG", "转移目录要写成绝对路径。");
  const norm = t.replace(/[\\/]+$/, "").toLowerCase();
  if (norm === SRC.toLowerCase() || norm.startsWith(SRC.toLowerCase() + "\\")) {
    return bad("E_PATH_FORBIDDEN", "转移目录不能在监控目录里面，否则刚搬过去的文件会被再搬一次。");
  }
  if (/^[A-Za-z]:\\$/.test(t)) return bad("E_PATH_FORBIDDEN", "不能搬到整个盘的根目录。");
  if (/[A-Za-z]:\\(windows|program files)/i.test(t)) {
    return bad("E_PATH_FORBIDDEN", "那是系统目录，不往那儿搬东西。");
  }
  if (t === "F:\\坏") return bad("E_FILE_OP_FAILED", "建不出目录：拒绝访问。");
  if (dto.quietMs < 200 || dto.quietMs > 5000) return bad("E_INVALID_ARG", "静置阈值只能在 200–5000ms。");
  return null;
}

function status() {
  return {
    watching: ui.watching,
    paused: settings.paused,
    pending: settings.paused ? 0 : 0,
    live: settings.paused ? 0 : 1,
    targetDir: settings.targetDir,
    sourceDir: settings.sourceDir,
  };
}

const Call = {
  ByName(name, ...args) {
    const fn = methods[name];
    if (!fn) return Promise.reject(new Error("没有绑定：" + name));
    return Promise.resolve(fn(...args));
  },
};

// 脚本化的现场：浏览器里按一下就能看到成/败/占用三种形态，不用真等一次下载。
const scenes = {
  moved: () => Events.Emit("download2:moved", {
    src: SRC + "\\电影.zip", dst: settings.targetDir + "\\2026-10-07\\电影.zip",
    size: 421_000_000, method: "rename", dated: "2026-10-07", ms: 3, tries: 0, pendingDelete: false,
  }),
  copied: () => Events.Emit("download2:moved", {
    src: SRC + "\\发票.pdf", dst: settings.targetDir + "\\2026-10-07\\发票.pdf",
    size: 12_345, method: "copy", dated: "2026-10-07", ms: 421, tries: 0, pendingDelete: true,
  }),
  stuck: () => Events.Emit("download2:stuck", { path: "占用中.docx", reason: "一直被其他程序占用" }),
  // 演一次「第二次启动把参数交给这一颗」：先把内存那份设置换掉（同 Hub.setRun），
  // 再欠上一句，最后把事件推出去 —— 页面收到就先重读再说明，配色和 toast 一起变。
  handoff: (theme) => {
    if (theme === "light" || theme === "dark") settings.theme = theme;
    handoffPending = true;
    Events.Emit("download2:secondLaunch");
  },
  // 演一次「某条启动参数没轮上」：话是后端那句原话，桩里不替它重造一份说法。
  badFlag: (msg) => {
    owedNotice = msg || "--lang 没用上：界面语言只认 auto、zh、en";
    handoffPending = false;
    Events.Emit("download2:secondLaunch");
  },
  aborted: () => Events.Emit("download2:aborted", { path: "半成品.zip" }),
  busy: () => Events.Emit("download2:status", { ...status(), pending: 2, live: 3 }),
  // 下面三条是给「桩里没有真窗口、也没有真文件系统」补的：翻一次堆叠方向、
  // 换一次「今天有没有东西」、换一次「有没有搬运记录」，
  // 按完再去点界面上的 配置/查看/查看日志，各条分支就能在浏览器里看见。
  flipDrop: () => {
    ui.drop = ui.drop === "down" ? "up" : "down";
    Events.Emit("download2:mode", { expanded: ui.expanded, drop: ui.drop });
  },
  flipToday: () => { todayExists = !todayExists; },
  // 「有没有搬运记录」翻一次，再去按查看日志：opened 真假两条文案都能在浏览器里看见。
  flipMoves: () => { movesExists = !movesExists; },
};
window.__stub = { scenes, ui, settings, events: Events };
export { Call, Events };
