"use client";
// 应用外壳：顶部（应用标识 + 当前身份 + 语言切换）、左侧导航、内容区。
// 与主站同域同路径前缀，因此语言与主题沿用同一套 cookie / localStorage 口径。
import React, { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Icon } from "@/components/ui";
import { useSession } from "@/components/SessionProvider";
import { useI18n } from "@/shared/i18n/I18nProvider";
import { BASE_PATH } from "@/lib/app";
import { PERMISSION_ASSET_MODERATE } from "@/lib/session";
import { locales } from "@/shared/i18n/routing";
import { connectAdminWorkspace, prepareAdminNavigation, announceAdminNavigation } from "@/lib/adminWorkspace";
import { ThemeModeSwitcher } from "./ThemeModeSwitcher";

const NAV = [
  { href: "/", key: "nav.overview", icon: "gauge" },
  { href: "/assets", key: "nav.assets", icon: "search" },
  { href: "/bindings", key: "nav.bindings", icon: "unlink" },
  { href: "/moderation", key: "nav.moderation", icon: "shield" },
];

export function AppShell({ children }: { children: React.ReactNode }) {
  const { t, setLocale } = useI18n();
  const [embedded, setEmbedded] = useState(false);
  useEffect(() => {
    const disconnect = connectAdminWorkspace(setLocale);
    setEmbedded(disconnect !== null);
    return disconnect ?? undefined;
  }, [setLocale]);
  const { state, can } = useSession();
  const pathname = usePathname();
  const router = useRouter();
  const searchParams = useSearchParams();

  // usePathname 返回的是带 basePath 的完整路径，去掉前缀后才能与上面的 href 比。
  const rest = pathname.startsWith(BASE_PATH) ? pathname.slice(BASE_PATH.length) || "/" : pathname;
  const current = rest.replace(/\/+$/, "") || "/";
  const visibleNav = NAV.filter((item) => item.href !== "/moderation" || can(PERMISSION_ASSET_MODERATE));

  useEffect(() => { announceAdminNavigation(); }, [pathname, searchParams]);

  return (
    <div className="min-h-screen bg-background">
      <header hidden={embedded} className="sticky top-0 z-30 border-b border-line bg-surface/90 backdrop-blur">
        <div className="mx-auto flex h-14 w-full max-w-page items-center justify-between gap-2 px-4 sm:px-6">
          <div className="flex min-w-0 flex-1 items-center gap-2">
            <a href="/admin" aria-label={t("nav.backToHub")} title={t("nav.backToHub")} className="inline-flex h-11 w-11 sm:w-auto shrink-0 items-center justify-center gap-1 text-xs text-text-muted hover:text-text-strong"><span aria-hidden="true">←</span><span className="hidden sm:inline">{t("nav.backToHub")}</span></a>
            <span className="hidden text-text-faint sm:inline">/</span>
            <div className="flex min-w-0 items-center gap-2 text-sm font-semibold text-text-strong"><Icon name="shield" className="h-4 w-4 shrink-0 text-primary" /><span className="truncate">{t("common.appName")}</span></div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <LocaleMenu />
            <ThemeModeSwitcher />
            {state.status === "ready" ? <span className="hidden max-w-[9rem] truncate rounded border border-primary/30 bg-primary/20 px-2 py-0.5 font-mono text-xs text-primary sm:inline-flex" title={state.user.username}>{state.user.username}</span> : null}
          </div>
        </div>
      </header>

      <div className="mx-auto flex max-w-page flex-col gap-6 px-4 py-6 lg:flex-row">
        <nav className="shrink-0 lg:w-52" aria-label={t("nav.groupLabel")}>
          <div className="rounded-control border border-line bg-surfaceSubtle p-3 lg:hidden">
            <label htmlFor="storage-admin-section" className="mb-2 block text-xs font-medium text-text-muted">{t("nav.groupLabel")}</label>
            <select id="storage-admin-section" value={current} onChange={(e) => { prepareAdminNavigation("/admin/storage" + e.target.value); if (embedded) router.replace(e.target.value); else router.push(e.target.value); }} className="w-full rounded-control border border-line bg-surface px-3 py-2.5 text-sm text-text-strong">
              {visibleNav.map((item) => <option key={item.href} value={item.href}>{t(item.key)}</option>)}
            </select>
          </div>
          <ul className={`hidden rounded-control border border-line bg-surfaceSubtle p-2 lg:sticky ${embedded ? "lg:top-6" : "lg:top-24"} lg:flex lg:flex-col`}>
            {visibleNav.map((item) => {
              const active = current === item.href;
              return (
                <li key={item.href} className="shrink-0">
                  <Link
                    href={item.href}
                    replace={embedded}
                    onClick={(event) => prepareAdminNavigation(event.currentTarget.href, event)}
                    aria-current={active ? "page" : undefined}
                    className={
                      "flex items-center gap-3 rounded-control px-3 py-2.5 text-sm transition-colors " +
                      (active
                        ? "bg-primary/15 font-semibold text-primary"
                        : "text-text-muted hover:bg-surfaceHover hover:text-text-body")
                    }
                  >
                    <Icon name={item.icon} className="w-4 h-4" />
                    {t(item.key)}
                  </Link>
                </li>
              );
            })}
          </ul>
        </nav>

        <main className="min-w-0 flex-1 space-y-4">{children}</main>
      </div>
    </div>
  );
}

const LOCALE_NAMES: Record<string, string> = { "zh-CN": "简体中文", "en-US": "English", "zh-TW": "繁體中文", "ja-JP": "日本語" };

function LocaleMenu() {
  const { t, locale, setLocale } = useI18n();
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onPointer = (event: MouseEvent) => { if (!container.current?.contains(event.target as Node)) setOpen(false); };
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onPointer); document.removeEventListener("keydown", onKey); };
  }, [open]);
  return <div className="relative" ref={container}>
    <button type="button" aria-label={t("common.language")} title={t("common.language")} aria-expanded={open} aria-haspopup="menu" onClick={() => setOpen(!open)} className="grid h-9 w-9 place-items-center rounded-full border border-line bg-surfaceSubtle text-text-body hover:bg-surfaceHover"><span aria-hidden="true">文</span></button>
    {open ? <div role="menu" aria-label={t("common.language")} className="absolute right-0 z-50 mt-2 w-44 rounded-card border border-line bg-surface p-1.5 shadow-elevated">
      {locales.map((loc) => <button key={loc} type="button" role="menuitemradio" aria-checked={locale === loc} onClick={() => { setLocale(loc); setOpen(false); }} className={"flex w-full items-center justify-between rounded-control px-3 py-2 text-left text-xs hover:bg-surfaceHover " + (locale === loc ? "font-semibold text-primary" : "text-text-body")}>{LOCALE_NAMES[loc] ?? loc}{locale === loc ? "✓" : null}</button>)}
    </div> : null}
  </div>;
}
