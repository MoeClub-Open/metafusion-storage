"use client";
// 极简 UI 原件：管理台只有三页，不值得引入组件库。
// 视觉沿用主站的设计 token（tailwind.config.ts 与 shared/theme/globals.css 抄自主仓库），
// 因此这里出现的类名与主站组件是同一套语义色（surface / line / text-*）。
import React, { useEffect, useRef } from "react";
import type { Message } from "@/lib/api";
import { useI18n } from "@/shared/i18n/I18nProvider";

/** 把 Message（键 + 变量）解析成当前语言的文本。 */
export function useMessage() {
  const { t } = useI18n();
  return React.useCallback((m: Message) => t(m.key, m.vars), [t]);
}

const ICONS: Record<string, string> = {
  gauge: "M12 14a2 2 0 1 0 0-4 2 2 0 0 0 0 4Zm0-9v2m7.07 1.93-1.41 1.41M21 12h-2M6.34 6.34 4.93 4.93M3 12h2m3.34 5.66-1.41 1.41M12 12l4-4",
  search: "M21 21l-4.35-4.35M17 10.5a6.5 6.5 0 1 1-13 0 6.5 6.5 0 0 1 13 0Z",
  unlink: "M9 15l-1.5 1.5a4.24 4.24 0 0 1-6-6L3 9m12-3 1.5-1.5a4.24 4.24 0 0 1 6 6L21 12M8 12h8",
  refresh: "M21 12a9 9 0 1 1-3-6.7M21 3v6h-6",
  alert: "M12 9v4m0 4h.01M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z",
  check: "m5 13 4 4L19 7",
  external: "M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6m4-3h6v6m-11 5L21 3",
  copy: "M9 9h10v10a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2V9Zm0 0V7a2 2 0 0 1 2-2h6",
  shield: "M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z",
  info: "M12 16v-4m0-4h.01M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z",
  close: "M18 6 6 18M6 6l12 12",
  file: "M14 2H7a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V7l-5-5Zm0 0v5h5",
};

export function Icon({ name, className = "w-4 h-4" }: { name: keyof typeof ICONS | string; className?: string }) {
  const d = ICONS[name] ?? ICONS.info;
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className={className} aria-hidden="true">
      <path d={d} />
    </svg>
  );
}

export function Card({ children, className = "" }: { children: React.ReactNode; className?: string }) {
  return <section className={"rounded-card border border-line bg-surface p-4 sm:p-5 " + className}>{children}</section>;
}

export function SectionHeader({
  icon,
  title,
  desc,
  actions,
}: {
  icon?: string;
  title: string;
  desc?: string;
  actions?: React.ReactNode;
}) {
  return (
    <header className="mb-4 flex flex-wrap items-start justify-between gap-3">
      <div className="flex items-start gap-2.5">
        {icon ? (
          <span className="mt-0.5 text-primary">
            <Icon name={icon} className="w-4 h-4" />
          </span>
        ) : null}
        <div>
          <h2 className="text-sm font-semibold text-text-strong">{title}</h2>
          {desc ? <p className="mt-1 max-w-3xl text-xs leading-relaxed text-text-muted">{desc}</p> : null}
        </div>
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </header>
  );
}

type ButtonProps = {
  children: React.ReactNode;
  onClick?: () => void;
  type?: "button" | "submit";
  variant?: "primary" | "ghost" | "danger";
  disabled?: boolean;
  busy?: boolean;
};

export function Button({ children, onClick, type = "button", variant = "primary", disabled, busy }: ButtonProps) {
  const { t } = useI18n();
  const styles: Record<string, string> = {
    primary: "bg-primary text-primary-foreground hover:bg-primary-hover border border-transparent",
    ghost: "border border-line text-text-body hover:bg-surfaceHover",
    danger: "border border-red-500/40 text-danger hover:bg-red-500/10",
  };
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled || busy}
      className={
        "inline-flex items-center gap-1.5 rounded-control px-3 py-2 text-xs font-medium transition-colors duration-fast disabled:cursor-not-allowed disabled:opacity-50 " +
        styles[variant]
      }
    >
      {busy ? <Spinner /> : null}
      {busy ? t("common.loading") : children}
    </button>
  );
}

export function Spinner({ className = "w-3.5 h-3.5" }: { className?: string }) {
  return (
    <span
      className={"inline-block animate-spin rounded-full border-2 border-current border-t-transparent " + className}
      role="status"
      aria-hidden="true"
    />
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  label,
  onSubmit,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  label: string;
  onSubmit?: () => void;
}) {
  return (
    <input
      type="text"
      value={value}
      spellCheck={false}
      aria-label={label}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter" && onSubmit) {
          e.preventDefault();
          onSubmit();
        }
      }}
      className="min-w-0 flex-1 rounded-control border border-line bg-surfaceSubtle px-3 py-2 font-mono text-xs text-text-strong outline-none transition-colors focus:border-primary"
    />
  );
}

export function Badge({ children, tone = "neutral" }: { children: React.ReactNode; tone?: "neutral" | "ok" | "warn" | "bad" }) {
  const tones: Record<string, string> = {
    neutral: "border-line text-text-muted",
    ok: "border-emerald-500/40 text-success",
    warn: "border-amber-500/40 text-warn",
    bad: "border-red-500/40 text-danger",
  };
  return (
    <span className={"inline-flex items-center gap-1 rounded-chip border px-2 py-0.5 font-mono text-[11px] " + tones[tone]}>
      {children}
    </span>
  );
}

export function Notice({
  tone,
  title,
  children,
}: {
  tone: "info" | "ok" | "warn" | "error";
  title?: string;
  children: React.ReactNode;
}) {
  const tones: Record<string, { box: string; icon: string; mark: string }> = {
    info: { box: "border-line bg-surfaceSubtle", icon: "text-primary", mark: "info" },
    ok: { box: "border-emerald-500/30 bg-emerald-500/5", icon: "text-success", mark: "check" },
    warn: { box: "border-amber-500/30 bg-amber-500/5", icon: "text-warn", mark: "alert" },
    error: { box: "border-red-500/30 bg-red-500/5", icon: "text-danger", mark: "alert" },
  };
  const tone_ = tones[tone];
  return (
    <div className={"rounded-card border p-3 text-xs leading-relaxed " + tone_.box} role={tone === "error" ? "alert" : tone === "ok" ? "status" : undefined}>
      <div className="flex items-start gap-2">
        <span className={"mt-0.5 shrink-0 " + tone_.icon}>
          <Icon name={tone_.mark} className="w-4 h-4" />
        </span>
        <div className="space-y-1">
          {title ? <div className="font-medium text-text-strong">{title}</div> : null}
          <div className="text-text-body">{children}</div>
        </div>
      </div>
    </div>
  );
}

/** 键值对列表：管理台里"看一份记录的字段"用同一套排版，避免每页各写一版 grid。 */
export function DataList({ items }: { items: { label: string; value: React.ReactNode; mono?: boolean }[] }) {
  return (
    <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
      {items.map((item) => (
        <div key={item.label} className="min-w-0">
          <dt className="text-[11px] uppercase tracking-wide text-text-faint">{item.label}</dt>
          <dd className={"mt-1 break-all text-xs " + (item.mono ? "font-mono text-text-body" : "text-text-strong")}>
            {item.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** 可复制的等宽值：地址类字段一律走它（手抄 uuid 是运维事故的常见来源）。 */
export function CopyField({ value, label }: { value: string; label: string }) {
  const { t } = useI18n();
  const [copied, setCopied] = React.useState(false);
  const [failed, setFailed] = React.useState(false);
  return (
    <div className="space-y-2">
    <div className="flex min-w-0 items-start gap-2">
      <code className="min-w-0 flex-1 select-all break-all whitespace-pre-wrap rounded-control border border-line bg-surfaceSubtle px-2 py-1.5 font-mono text-[11px] text-text-body">
        {value}
      </code>
      <Button
        variant="ghost"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(value);
            setCopied(true);
            setFailed(false);
            window.setTimeout(() => setCopied(false), 1500);
          } catch {
            setCopied(false);
            setFailed(true);
          }
        }}
      >
        <Icon name="copy" className="w-3.5 h-3.5" />
        {copied ? t("common.copied") : t("common.copy")}
        <span className="sr-only">{label}</span>
      </Button>
    </div>
    {failed ? <p role="alert" className="text-xs text-danger">{t("common.copyFailed")}</p> : null}
    </div>
  );
}

/** 破坏性动作的二次确认。绑定解绑走这里，不做"点一下直接删"。 */
export function ConfirmDialog({
  open,
  title,
  body,
  confirmLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  body: React.ReactNode;
  confirmLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useI18n();
  const ref = useRef<HTMLDivElement>(null);
  const titleId = React.useId();
  const latest = useRef({busy, onCancel});
  latest.current = {busy, onCancel};
  useEffect(() => {
    if (!open || !ref.current) return;
    const element = ref.current;
    const previous = document.activeElement as HTMLElement | null;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const items = () => Array.from(element.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], input:not([disabled]), [tabindex]:not([tabindex="-1"])'));
    (items()[0] ?? element).focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.preventDefault(); if (!latest.current.busy) latest.current.onCancel(); }
      if (e.key !== "Tab") return;
      const all = items(); const first = all[0]; const last = all.at(-1);
      if (!first || !last) { e.preventDefault(); element.focus(); }
      else if (e.shiftKey && (document.activeElement === first || document.activeElement === element || !element.contains(document.activeElement))) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      document.body.style.overflow = overflow;
      if (previous?.isConnected) previous.focus();
    };
  }, [open]);

  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" role="presentation">
      <div ref={ref} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1} className="w-full max-w-md min-w-0 max-h-[90vh] overflow-y-auto rounded-panel border border-line bg-surface p-5 shadow-elevated outline-none">
        <h3 id={titleId} className="text-sm font-semibold text-text-strong">{title}</h3>
        <div className="mt-2 text-xs leading-relaxed text-text-body">{body}</div>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" onClick={onCancel} disabled={busy}>
            {t("common.cancel")}
          </Button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={busy}
            className="inline-flex items-center gap-1.5 rounded-control border border-red-500/40 px-3 py-2 text-xs font-medium text-danger transition-colors hover:bg-red-500/10 disabled:opacity-50"
          >
            {busy ? <Spinner /> : null}
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

export function EmptyState({ children }: { children: React.ReactNode }) {
  return <p className="rounded-card border border-dashed border-line px-4 py-6 text-center text-xs text-text-muted">{children}</p>;
}
