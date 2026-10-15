"use client";

// 会话门：同域 cookie 换 /api/auth/me，未登录直接跳主站登录页并带上回跳地址。
// 这里不做服务端鉴权（真正的闸门在存储服务侧），只是把"没登录/无权限"的界面收敛掉。
import React, { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import { Button, Icon, Notice } from "@/components/ui";
import { requestAdminWorkspaceLogin } from "@/lib/adminWorkspace";
import { BASE_PATH } from "@/lib/app";
import { useI18n } from "@/shared/i18n/I18nProvider";
import { can as canCode, fetchSession, loginRedirectUrl, type SessionUser } from "@/lib/session";

type State =
  | { status: "loading"; user: null; error?: undefined }
  | { status: "ready"; user: SessionUser; error?: undefined }
  | { status: "redirecting"; user: null; error?: undefined }
  | { status: "error"; user: null; error: string };

type Ctx = {
  state: State;
  /** 与服务端同口径的权限码判定（含 * 通配），见 lib/session.ts。 */
  can: (code: string) => boolean;
  reload: () => Promise<void>;
};

const SessionContext = createContext<Ctx>({
  state: { status: "loading", user: null },
  can: () => false,
  reload: async () => {},
});

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = useState<State>({ status: "loading", user: null });
  const redirected = useRef(false);

  const load = useCallback(async () => {
    setState({ status: "loading", user: null });
    try {
      const user = await fetchSession();
      if (!user) {
        setState({ status: "redirecting", user: null });
        // 只在本应用自己的路径上跳转：本应用没接住的路径（例如网关把登录页路由到了别处、
        // 或请求的是一个不属于本应用的地址）会被渲染成"未找到"，那里再跳一次就是死循环。
        const owned = window.location.pathname.startsWith(BASE_PATH);
        if (owned && !redirected.current) {
          redirected.current = true;
          if (!requestAdminWorkspaceLogin()) window.location.replace(loginRedirectUrl());
        }
        return;
      }
      setState({ status: "ready", user });
    } catch (err) {
      setState({ status: "error", user: null, error: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const value = useMemo<Ctx>(
    () => ({
      state,
      can: (code: string) => canCode(state.user, code),
      reload: load,
    }),
    [state, load]
  );

  return (
    <SessionContext.Provider value={value}>
      <Gate>{children}</Gate>
    </SessionContext.Provider>
  );
}

export function useSession() {
  return useContext(SessionContext);
}

/** Gate 只决定"内容区给不给看"：未登录时把页面换成一句可操作的话，而不是渲染半截再 401。 */
function Gate({ children }: { children: React.ReactNode }) {
  const { state, reload } = useSession();
  const { t } = useI18n();

  if (state.status === "ready") return <>{children}</>;

  if (state.status === "loading") {
    return (
      <Notice tone="info">
        <span className="inline-flex items-center gap-2">
          <Icon name="shield" className="w-4 h-4 text-primary" />
          {t("session.checking")}
        </span>
      </Notice>
    );
  }

  if (state.status === "redirecting") {
    return (
      <Notice tone="warn" title={t("session.redirecting")}>
        <a className="text-primary underline underline-offset-2" href={loginRedirectUrl()}>
          {loginRedirectUrl()}
        </a>
      </Notice>
    );
  }

  return (
    <div className="space-y-3">
      <Notice tone="error">{t("session.failed", { message: state.error })}</Notice>
      <Button variant="ghost" onClick={() => void reload()}>
        <Icon name="refresh" className="w-3.5 h-3.5" />
        {t("common.refresh")}
      </Button>
    </div>
  );
}
