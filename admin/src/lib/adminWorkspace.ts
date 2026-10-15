// 管理中心嵌入协议 v1；与主仓 frontend/src/lib/adminWorkspace.ts 对应。
// 仅共享导航与显示偏好，不接收身份、令牌或权限；每个应用继续读取 /api/auth/me。
const WORKSPACE = "storage";
const CHANNEL = "metafusion.admin.v1";
type Locale = "zh-CN" | "en-US" | "zh-TW" | "ja-JP";
const LOCALES: readonly string[] = ["zh-CN", "en-US", "zh-TW", "ja-JP"];

export function isAdminWorkspaceEmbedded(): boolean {
  if (typeof window === "undefined" || window.parent === window || window.name !== `metafusion-admin-${WORKSPACE}`) return false;
  try {
    return window.parent.location.origin === window.location.origin && window.parent.location.pathname.replace(/\/+$/, "") === "/admin";
  } catch { return false; }
}

let pendingNavigation: string | null = null;
const routeKey = (href: string) => {
  const url = new URL(href, window.location.origin);
  return url.pathname.replace(/\/+$/, "") + url.search + url.hash;
};

/** 嵌入时子路由使用 replace，用户发起的导航由宿主创建唯一的历史项。 */
export function prepareAdminNavigation(href: string, event?: Pick<MouseEvent, "button" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">): void {
  if (event && (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey)) return;
  if (isAdminWorkspaceEmbedded()) pendingNavigation = routeKey(href);
}

function send(type: "ready" | "navigate" | "login"): boolean {
  if (!isAdminWorkspaceEmbedded()) return false;
  const href = window.location.pathname + window.location.search + window.location.hash;
  const history = type === "navigate" && pendingNavigation === routeKey(href) ? "push" : "replace";
  if (history === "push") pendingNavigation = null;
  window.parent.postMessage({ channel: CHANNEL, workspace: WORKSPACE, type, href, history,
  }, window.location.origin);
  return true;
}

export function announceAdminNavigation(): void { send("navigate"); }
export function requestAdminWorkspaceLogin(): boolean { return send("login"); }

/** 独立打开时不改变外壳；嵌入时由父管理中心唯一控制顶栏、语言和主题。 */
export function connectAdminWorkspace(setLocale: (locale: Locale) => void): (() => void) | null {
  if (!isAdminWorkspaceEmbedded()) return null;
  const receive = (event: MessageEvent) => {
    if (event.source !== window.parent || event.origin !== window.location.origin) return;
    const data = event.data;
    if (!data || typeof data !== "object" || data.channel !== CHANNEL || data.workspace !== WORKSPACE) return;
    if (data.type === "connect") { send("ready"); return; }
    if (data.type !== "preferences" || !LOCALES.includes(data.locale) || (data.mode !== "dark" && data.mode !== "light")) return;
    setLocale(data.locale as Locale);
    const root = document.documentElement;
    root.setAttribute("data-theme-mode", data.mode);
    for (const key of ["accent", "tone"] as const) {
      if (typeof data[key] === "string" && /^[a-z0-9-]{1,40}$/.test(data[key])) root.setAttribute(`data-theme-${key}`, data[key]);
    }
    root.classList.toggle("dark", data.mode === "dark");
    root.classList.toggle("light", data.mode === "light");
    root.style.colorScheme = data.mode;
  };
  // 跨出当前应用的普通链接交给顶层窗口，避免把站点页面或登录页嵌进工作区。
  const followLink = (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target instanceof Element ? event.target.closest("a[href]") : null;
    if (!(anchor instanceof HTMLAnchorElement) || anchor.hasAttribute("download") || (anchor.target && anchor.target !== "_self")) return;
    const url = new URL(anchor.href, window.location.href);
    if (url.protocol !== "http:" && url.protocol !== "https:") return;
    const base = `/admin/${WORKSPACE}`;
    if (url.origin === window.location.origin && (url.pathname === base || url.pathname.startsWith(base + "/"))) return;
    event.preventDefault();
    if (url.origin === window.location.origin && url.pathname === "/login") { send("login"); return; }
    window.parent.location.assign(url.href);
  };
  document.addEventListener("click", followLink, true);
  window.addEventListener("message", receive);
  window.addEventListener("popstate", announceAdminNavigation);
  window.addEventListener("hashchange", announceAdminNavigation);
  send("ready");
  return () => {
    document.removeEventListener("click", followLink, true);
    window.removeEventListener("message", receive);
    window.removeEventListener("popstate", announceAdminNavigation);
    window.removeEventListener("hashchange", announceAdminNavigation);
  };
}
