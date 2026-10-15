"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { isAdminWorkspaceEmbedded, prepareAdminNavigation } from "@/lib/adminWorkspace";
import { Button, Card, ConfirmDialog, EmptyState, Notice, SectionHeader, TextInput, useMessage } from "@/components/ui";
import { describeApiError, normalizeIdInput, type Message } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { PERMISSION_ASSET_MODERATE } from "@/lib/session";
import { fetchBlockedAssets, setAssetBlocked, type Asset } from "@/lib/storage";
import { useSession } from "@/components/SessionProvider";
import { useI18n } from "@/shared/i18n/I18nProvider";

const PAGE_SIZE = 100;

export default function ModerationPage() {
  const { t, locale } = useI18n();
  const text = useMessage();
  const { state, can } = useSession();
  const permitted = can(PERMISSION_ASSET_MODERATE);
  const [assets, setAssets] = useState<Asset[]>([]);
  const [offset, setOffset] = useState(0);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Message | null>(null);
  const [notice, setNotice] = useState<Message | null>(null);
  const [assetId, setAssetId] = useState("");
  const [reason, setReason] = useState("");
  const [action, setAction] = useState<{ id: string; blocked: boolean } | null>(null);

  const load = useCallback(async (pageOffset: number) => {
    setLoading(true);
    setError(null);
    try {
      const result = await fetchBlockedAssets(PAGE_SIZE, pageOffset);
      setAssets(result.assets ?? []);
      setOffset(pageOffset);
    } catch (err) {
      setAssets([]);
      setError(describeApiError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (state.status === "ready" && permitted) void load(0);
  }, [state.status, permitted, load]);

  const prepareBlock = () => {
    const id = normalizeIdInput(assetId);
    if (!id) { setError({ key: "assets.invalidId" }); return; }
    if (!reason.trim()) { setError({ key: "moderation.reasonRequired" }); return; }
    if (reason.trim().length > 280) { setError({ key: "moderation.reasonTooLong" }); return; }
    setError(null);
    setAssetId(id);
    setAction({ id, blocked: true });
  };

  const confirm = async () => {
    if (!action) return;
    setBusy(true);
    setNotice(null);
    setError(null);
    try {
      await setAssetBlocked(action.id, action.blocked, action.blocked ? reason.trim() : undefined);
      setNotice({ key: action.blocked ? "moderation.blockedDone" : "moderation.unblockedDone" });
      if (action.blocked) { setAssetId(""); setReason(""); }
      setAction(null);
      await load(0);
    } catch (err) {
      setError(describeApiError(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <Card>
        <SectionHeader title={t("moderation.title")} desc={t("moderation.desc")} actions={permitted ? <Button variant="ghost" onClick={() => void load(offset)} busy={loading}>{t("common.refresh")}</Button> : null} />
        {!permitted ? <Notice tone="warn">{t("common.requiredPermission", { code: PERMISSION_ASSET_MODERATE })}</Notice> : null}
        {error ? <div className="mb-3"><Notice tone="error">{text(error)}</Notice></div> : null}
        {notice ? <div className="mb-3"><Notice tone="ok">{text(notice)}</Notice></div> : null}
        {permitted ? (
          <div className="space-y-3">
            <div className="flex flex-col gap-2 sm:flex-row">
              <TextInput value={assetId} onChange={setAssetId} label={t("moderation.assetId")} placeholder={t("assets.idPlaceholder")} onSubmit={prepareBlock} />
              <TextInput value={reason} onChange={setReason} label={t("moderation.reason")} placeholder={t("moderation.reasonPlaceholder")} onSubmit={prepareBlock} />
              <Button variant="danger" onClick={prepareBlock}>{t("moderation.block")}</Button>
            </div>
            <p className="text-xs text-text-muted">{t("moderation.blockHint")}</p>
          </div>
        ) : null}
      </Card>
      {permitted ? (
        <Card>
          <SectionHeader title={t("moderation.listTitle")} desc={t("moderation.listDesc")} />
          {loading && assets.length === 0 ? <p className="text-xs text-text-muted">{t("common.loading")}</p> : null}
          {!loading && assets.length === 0 ? <EmptyState>{t("moderation.empty")}</EmptyState> : null}
          {assets.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[48rem] text-left text-xs">
                <thead className="border-b border-line text-text-faint"><tr><th className="px-3 py-2">{t("moderation.file")}</th><th className="px-3 py-2">{t("moderation.reason")}</th><th className="px-3 py-2">{t("moderation.blockedAt")}</th><th className="px-3 py-2">{t("bindings.column.actions")}</th></tr></thead>
                <tbody>{assets.map((asset) => <tr key={asset.id} className="border-b border-line-subtle align-top">
                  <td className="px-3 py-3"><Link replace={isAdminWorkspaceEmbedded()} onClick={(event) => prepareAdminNavigation(event.currentTarget.href, event)} href={`/assets?id=${encodeURIComponent(asset.id)}`} className="font-medium text-primary hover:underline">{asset.file_name || asset.id}</Link><div className="mt-1 font-mono text-[11px] text-text-faint">{asset.id}</div></td>
                  <td className="max-w-xs break-words px-3 py-3 text-text-body">{asset.blocked_reason || "—"}</td>
                  <td className="px-3 py-3 text-text-muted">{asset.blocked_at ? formatDateTime(asset.blocked_at, locale) : "—"}</td>
                  <td className="px-3 py-3"><Button variant="ghost" onClick={() => setAction({ id: asset.id, blocked: false })}>{t("moderation.unblock")}</Button></td>
                </tr>)}</tbody>
              </table>
            </div>
          ) : null}
          <div className="mt-4 flex items-center gap-2">
            <Button variant="ghost" disabled={offset === 0 || loading} onClick={() => void load(Math.max(0, offset - PAGE_SIZE))}>{t("moderation.previous")}</Button>
            <span className="text-xs text-text-muted">{t("moderation.page", { page: Math.floor(offset / PAGE_SIZE) + 1 })}</span>
            <Button variant="ghost" disabled={assets.length < PAGE_SIZE || loading} onClick={() => void load(offset + PAGE_SIZE)}>{t("moderation.next")}</Button>
          </div>
        </Card>
      ) : null}
      <ConfirmDialog open={action !== null} title={t(action?.blocked ? "moderation.blockConfirmTitle" : "moderation.unblockConfirmTitle")} body={t(action?.blocked ? "moderation.blockConfirmBody" : "moderation.unblockConfirmBody", { id: action?.id ?? "" })} confirmLabel={t(action?.blocked ? "moderation.block" : "moderation.unblock")} busy={busy} onConfirm={() => void confirm()} onCancel={() => setAction(null)} />
    </div>
  );
}
