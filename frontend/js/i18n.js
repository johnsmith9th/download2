// 界面语言的唯一一份字典。
//
// 为什么只在前端：后端一个词的文案都不出（`errorDTO.msg` 是另一回事，见下），
// 所以「这句怎么说」不该让 Go 参与第二遍 —— 两处各存一份说法，早晚对不上。
//
// key 按「谁在说什么」命名（pill.* / tab.* / hint.* / toast.*），不按节点 id 命名：
// 节点会重构，说的话不会。
//
// zh 与 en 必须对等，唯一的例外是 index.html 里那份致谢清单：它写死在页面、正文永久英文、
// 一个 key 都不进这里（理由见设计书「界面」一节）。看到那份表别顺手补进字典。
//
// 英文那一列是这套界面的**主要风险所在**：窗口尺寸由 Go 定（药丸 236×40、面板 360×340 DIP），
// 而英文单词比中文长得多。改任何一个 en 值都要回 uiprobe 截图看字有没有被裁。
export const DICT = {
  zh: {
    "pill.startup": "启动中",
    "pill.watching": "监控中",
    "pill.idle": "没在监控",
    "pill.paused": "已暂停",
    "pill.downloading": "下载中 {n}",
    "pill.moving": "在搬 {n}",
    "pill.noTarget": "还没设转移目录",
    "pill.pause": "暂停",
    "pill.resume": "继续",
    "pill.pauseTitle": "暂停：不再搬任何文件",
    "pill.resumeTitle": "继续监控并搬运",
    "btn.config": "配置",
    "btn.quit": "退出",
    "btn.gearOpen": "配置（打开后原地展开）",
    "btn.gearClose": "收起配置（Esc）",

    "tab.main": "常规",
    "tab.adv": "高级",
    "tab.about": "关于",
    "panel.aria": "配置",
    "tabs.aria": "配置分栏",
    "theme.toLight": "切到浅色",
    "theme.toDark": "切到深色",
    "lang.toEn": "切换到英文界面",
    "lang.toZh": "切换到中文界面",

    "lab.target": "转移目录",
    "lab.source": "监控目录",
    "lab.quietMs": "静置阈值（ms）",
    "lab.maxSettle": "最长等待（秒）",
    "ph.unset": "还没设置",
    "combo.title": "常用转移目录",
    "combo.empty": "还没有常用目录：搬过的目录自动记在这里。",
    "combo.inUse": "{dir}：正在用这一个",
    "combo.remove": "从这个列表里移除，不删目录本身",
    "btn.browse": "浏览…",
    "btn.open": "打开",
    "btn.view": "查看",
    "btn.viewlog": "查看日志",
    "btn.clearlog": "清空日志",
    "view.title": "打开今天落档的那个目录；今天那一档还没有就打开转移目录",
    "viewlog.title": "用记事本打开搬运流水 moves.jsonl",
    "clearlog.title": "清空搬运流水与运行日志，不问第二次；崩溃账本不在它动的范围里",
    "chk.dateSubdirs": "按下载日期分子目录",
    "chk.toast": "搬完提示",
    "unit.seconds": "秒",
    "note.conflict": "同名文件会自动加 (1)、(2)：Windows 的改名会静默吃掉目标里已有的文件，这个保护不关。",
    "hint.quietMs": "停止变化这么久之后才考虑搬，200–5000。",
    "hint.maxSettle": "超过它还判定不成就地留着，只提示不强搬，10–3600。",
    "btn.relocate": "重新定位",
    "about.version": "版本",
    "about.author": "作者",
    "about.mail": "邮箱",
    "btn.ack": "致谢",
    "btn.ackClose": "关闭致谢（Esc）",

    "hint.movingTo": "搬到：{dir}",
    "hint.noTarget": "还没设置转移目录，药丸只会看着不动手。",
    "hint.saved": "已保存",
    "hint.nothingOpened": "哪儿也没打开：{dir} 和转移目录现在都不存在。",
    "hint.todayFallback": "今天还没搬过东西，已打开转移目录 {dir}。补搬的文件按它自己的日期落档，可能在别的日期目录里。",
    "hint.opened": "已打开 {dir}",
    "hint.noLogYet": "还没有搬运记录，{path} 还没生成。",
    "hint.cleared": "已清空搬运流水与运行日志（崩溃账本没动）。已开着的记事本不会自己刷新。",
    "hint.relocated": "已重新定位，监控立刻按这个目录跑。",
    "toast.moved": "已搬走 {name}",
    "toast.copy": "（复制，跨卷）",
    "toast.debt": "：源还欠一次补删",
    "toast.stuck": "没搬动：{path} {reason}",
    "toast.aborted": "下载没完成：{path}",
    "toast.secondLaunch": "已有实例在运行，那一次没多挂一颗药丸",
    "doc.stubTitle": "Download2（浏览器替身）",
  },
  en: {
    "pill.startup": "Starting",
    "pill.watching": "Watching",
    "pill.idle": "Stopped",
    "pill.paused": "Paused",
    "pill.downloading": "Getting {n}",
    "pill.moving": "Moving {n}",
    "pill.noTarget": "No target folder set",
    "pill.pause": "Pause",
    "pill.resume": "Resume",
    "pill.pauseTitle": "Pause: stop moving files",
    "pill.resumeTitle": "Resume watching and moving",
    "btn.config": "Config",
    "btn.quit": "Quit",
    "btn.gearOpen": "Settings (opens in place)",
    "btn.gearClose": "Close settings (Esc)",

    "tab.main": "General",
    "tab.adv": "Advanced",
    "tab.about": "About",
    "panel.aria": "Settings",
    "tabs.aria": "Settings sections",
    "theme.toLight": "Switch to light",
    "theme.toDark": "Switch to dark",
    "lang.toEn": "Switch to English",
    "lang.toZh": "Switch to Chinese",

    "lab.target": "Target folder",
    "lab.source": "Watched folder",
    "lab.quietMs": "Settle time (ms)",
    "lab.maxSettle": "Max wait (sec)",
    "ph.unset": "Not set",
    "combo.title": "Recent target folders",
    "combo.empty": "Nothing yet — folders you move to get remembered here.",
    "combo.inUse": "in use",
    "combo.remove": "Remove from this list, the folder itself is untouched",
    "btn.browse": "Browse…",
    "btn.open": "Open",
    "btn.view": "View",
    "btn.viewlog": "Log",
    "btn.clearlog": "Clear log",
    "view.title": "Open today's folder; falls back to the target folder when today is empty",
    "viewlog.title": "Open the move log (moves.jsonl) in Notepad",
    "clearlog.title": "Clear the move log and the runtime log, no confirmation. The crash ledger is not touched.",
    "chk.dateSubdirs": "Subfolders by download date",
    "chk.toast": "Toast when done",
    "unit.seconds": "sec",
    "note.conflict": "Same-name files get (1), (2) appended: a Windows rename silently eats whatever is already there, so this guard stays on.",
    "hint.quietMs": "Wait this long without changes before moving, 200–5000.",
    "hint.maxSettle": "Past this, leave it in place and just warn, 10–3600.",
    "btn.relocate": "Relocate",
    "about.version": "Version",
    "about.author": "Author",
    "about.mail": "Email",
    "btn.ack": "Acknowledgements",
    "btn.ackClose": "Close (Esc)",

    "hint.movingTo": "Moving to: {dir}",
    "hint.noTarget": "No target folder yet — the pill only watches.",
    "hint.saved": "Saved",
    "hint.nothingOpened": "Nothing opened: neither {dir} nor the target folder exists.",
    "hint.todayFallback": "Nothing moved today; opened the target folder {dir}. Backfilled files sit under their own date.",
    "hint.opened": "Opened {dir}",
    "hint.noLogYet": "No moves yet — {path} hasn't been created.",
    "hint.cleared": "Move log and runtime log cleared (crash ledger untouched). An open Notepad window won't refresh itself.",
    "hint.relocated": "Relocated; watching this folder now.",
    "toast.moved": "Moved {name}",
    "toast.copy": " (copied, cross-volume)",
    "toast.debt": ": source still needs one more delete",
    "toast.stuck": "Not moved: {path} {reason}",
    "toast.aborted": "Download not finished: {path}",
    "toast.secondLaunch": "Already running; no second instance was started",
    "doc.stubTitle": "Download2 (browser stub)",
  },
};

// 初值取 zh：DOM 里预置的就是中文（那是首屏不闪一屏 key 的代价），
// 所以「还没人调用过 setLang」和「当前是中文」在页面上是同一个样子。
let lang = "zh";

// resolveLang 把设置里那个词换成一个真的语言。auto 的判据只有 navigator.language：
// 猜错了用户看得见、也改得动，所以这里不值得为「猜得更准」去加一套后端探测。
export function resolveLang(pref, nav) {
  if (pref === "zh" || pref === "en") return pref;
  const n = String(nav || "").toLowerCase();
  if (!n) return "zh";
  return n.startsWith("zh") ? "zh" : "en";
}

export function current() { return lang; }

// setLang 换语言并同步 <html lang>（读屏器和换行规则都看它）。返回是否真的换了。
export function setLang(code) {
  const next = code === "en" ? "en" : "zh";
  document.documentElement.lang = next === "en" ? "en" : "zh-CN";
  if (next === lang) return false;
  lang = next;
  return true;
}

// t 查一句说法。查不到时先回 zh 那一份（英文没翻完的表现是「那一句还是中文」，
// 而不是界面上出现一个 key），再没有才用调用方给的 fallback —— 后端那句中文 msg 就走这里。
export function t(key, params, fallback) {
  const raw = DICT[lang][key] ?? DICT.zh[key] ?? fallback;
  if (raw == null) return key;
  if (!params) return raw;
  return raw.replace(/\{(\w+)\}/g, (_, name) => (name in params ? String(params[name]) : ""));
}

const ATTRS = [
  ["data-i18n", (node, key) => { node.textContent = t(key); }],
  ["data-i18n-title", (node, key) => { node.title = t(key); }],
  ["data-i18n-placeholder", (node, key) => { node.placeholder = t(key); }],
  ["data-i18n-aria", (node, key) => { node.setAttribute("aria-label", t(key)); }],
];

// applyStatic 重写 index.html 上那些静态标签。动态内容（药丸那行字、提示、toast）
// 由各渲染函数自己查 t()，两边共用同一份字典，所以换语言只需要「重写一遍 + 重渲染一遍」。
export function applyStatic(root = document) {
  for (const [attr, write] of ATTRS) {
    for (const node of root.querySelectorAll(`[${attr}]`)) write(node, node.getAttribute(attr));
  }
}
