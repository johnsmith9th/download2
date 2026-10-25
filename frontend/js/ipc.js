// 全应用与后端之间只有这一处缝。
//
// 真窗口里 /wails/runtime.js 由 BundledAssetFileServer 自带；纯浏览器（devserve）里没有它，
// 就落到 devstub 的页内假 Go。两条路暴露同样两个符号（Call / Events），
// 所以 main.js 完全不知道自己在哪一边。
let rt;
export let backend = "native";
try {
  rt = await import("/wails/runtime.js");
} catch {
  rt = await import("./devstub.js");
  backend = "stub";
}

const { Call, Events } = rt;

// 绑定 FQN 含包名（bindings.go 用 PkgPath.TypeName.MethodName 建键）。
// 全应用只绑 main.UI 这一个类型，所以这条前缀是唯一的缝。
const PREFIX = "main.UI.";

// invoke 调一个绑定方法并拆信封。
//
// 后端一律返回 {ok,data,err}，所以这里只处理「连信封都没拿到」那一种意外：
// 传输层报错说明 Go 侧压根没跑到，界面必须说出这一点，不能显示成「保存失败」。
export async function invoke(name, ...args) {
  let raw;
  try {
    raw = await Call.ByName(PREFIX + name, ...args);
  } catch (e) {
    return { ok: false, data: null, err: { code: "E_TRANSPORT", msg: "叫不动后端：" + (e?.message || e) } };
  }
  if (!raw || typeof raw !== "object" || !("ok" in raw)) {
    return { ok: false, data: null, err: { code: "E_INTERNAL", msg: "后端回了一个看不懂的东西。" } };
  }
  return raw;
}

// on 订阅一个 Go 推过来的事件，返回退订函数。
//
// runtime.js 给回调的是包装对象（ev.data 才是负载），devstub 直接给值 ——
// 这里统一取 .data，两种形态都能喂。
export function on(name, fn) {
  return Events.On(name, (ev) => fn(ev && typeof ev === "object" && "data" in ev ? ev.data : ev));
}
