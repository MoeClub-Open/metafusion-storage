"use client";
// 绑定列表 + 解绑（资产页与绑定页共用一份实现）。
//
// 解绑的两个行为要点（服务端 internal/handler/handler.go 的 unbind）：
//   1. **不幂等**：DELETE /bindings/:id 先按 id 查这一行再删，第二次调用只剩 not_found——
//      界面必须讲成"该绑定已不存在（可能已被别人解除）"，不能当成失败吓人；
//   2. 无论成功还是 404，都**重新取数**：成功要拿掉那一行，404 说明手上这份列表已经过期。
import React from "react";
import Link from "next/link";
import { isAdminWorkspaceEmbedded, prepareAdminNavigation } from "@/lib/adminWorkspace";
import { ApiError, describeApiError, type Message } from "@/lib/api";
import { deleteBinding, type Asset, type Binding } from "@/lib/storage";
import { formatDateTime } from "@/lib/format";
import { Badge, Button, ConfirmDialog, EmptyState, Icon, Notice, useMessage } from "@/components/ui";
import { useI18n } from "@/shared/i18n/I18nProvider";

export type BindingRow = Binding & { asset?: Asset };

export function BindingTable({
  rows,
  title,
  emptyLabel,
  loading,
  onReload,
  showAsset = false,
}: {
  rows: BindingRow[];
  title: string;
  emptyLabel: string;
  loading?: boolean;
  /** 解绑后由调用方重新取数；这里不自己发请求，避免两处对"当前列表"的理解分叉。 */
  onReload: () => Promise<void> | void;
  showAsset?: boolean;
}) {
  const { t, locale } = useI18n();
  const text = useMessage();
  const [pending, setPending] = React.useState<BindingRow | null>(null);
  const [busyId, setBusyId] = React.useState<string | null>(null);
  const [notice, setNotice] = React.useState<{ tone: "ok" | "warn" | "error"; message: Message } | null>(null);

  const unbind = async (row: BindingRow) => {
    setBusyId(row.id);
    setNotice(null);
    try {
      await deleteBinding(row.id);
      setNotice({ tone: "ok", message: { key: "bindings.unbindDone" } });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.code === "not_found")) {
        setNotice({ tone: "warn", message: { key: "bindings.unbindGone" } });
      } else if (err instanceof ApiError && (err.status === 403 || err.code === "forbidden")) {
        setNotice({ tone: "error", message: { key: "bindings.unbindForbidden" } });
      } else {
        setNotice({ tone: "error", message: describeApiError(err) });
      }
    } finally {
      setBusyId(null);
      setPending(null);
      await onReload();
    }
  };

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-xs font-semibold text-text-strong">{title}</h3>
        {loading ? (
          <span className="text-[11px] text-text-muted">{t("bindings.loading")}</span>
        ) : null}
      </div>

      {notice ? (
        <Notice tone={notice.tone}>{text(notice.message)}</Notice>
      ) : null}

      {rows.length === 0 ? (
        <EmptyState>{emptyLabel}</EmptyState>
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full min-w-[46rem] border-collapse text-left text-xs">
            <thead className="bg-surfaceSubtle text-[11px] uppercase tracking-wide text-text-faint">
              <tr>
                {showAsset ? <th className="px-3 py-2 font-medium">{t("bindings.column.asset")}</th> : null}
                <th className="px-3 py-2 font-medium">{t("bindings.column.role")}</th>
                <th className="px-3 py-2 font-medium">{t("bindings.column.target")}</th>
                <th className="px-3 py-2 font-medium">{t("bindings.column.kind")}</th>
                <th className="px-3 py-2 font-medium">{t("bindings.column.createdBy")}</th>
                <th className="px-3 py-2 font-medium">{t("bindings.column.createdAt")}</th>
                <th className="px-3 py-2 font-medium">{t("bindings.column.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} className="border-t border-line/60 align-top">
                  {showAsset ? (
                    <td className="px-3 py-2">
                      <Link replace={isAdminWorkspaceEmbedded()} onClick={(event) => prepareAdminNavigation(event.currentTarget.href, event)} href={`/assets?id=${encodeURIComponent(row.asset_id)}`} className="font-mono text-[11px] text-primary hover:underline">{row.asset?.file_name ?? row.asset_id}</Link>
                      <div className="font-mono text-[10px] text-text-faint">{row.asset_id}</div>
                    </td>
                  ) : null}
                  <td className="px-3 py-2">
                    <Badge tone="neutral">{row.binding_role}</Badge>
                  </td>
                  <td className="px-3 py-2">
                    {/* 目录详情页是主站的路径（另一个应用）：只能用原生 <a>，
                        next/link 会按本应用的 basePath 拼成 /admin/storage/catalog/…。 */}
                    <a
                      className="font-mono text-[11px] text-primary hover:underline"
                      href={"/catalog/" + row.target_entity_id}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {row.target_entity_id}
                    </a>
                  </td>
                  <td className="px-3 py-2 font-mono text-[11px] text-text-muted">{row.target_kind}</td>
                  <td className="px-3 py-2 font-mono text-[11px] text-text-muted">{row.created_by}</td>
                  <td className="px-3 py-2 text-[11px] text-text-muted">{formatDateTime(row.created_at, locale)}</td>
                  <td className="px-3 py-2">
                    <Button variant="danger" busy={busyId === row.id} onClick={() => setPending(row)}>
                      <Icon name="unlink" className="w-3.5 h-3.5" />
                      {t("bindings.unbind")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <ConfirmDialog
        open={pending !== null}
        title={t("bindings.unbindTitle")}
        body={t("bindings.unbindBody", {
          role: pending?.binding_role ?? "",
          target: pending?.target_entity_id ?? "",
        })}
        confirmLabel={t("bindings.unbind")}
        busy={pending !== null && busyId === pending.id}
        onConfirm={() => {
          if (pending) void unbind(pending);
        }}
        onCancel={() => setPending(null)}
      />
    </div>
  );
}
