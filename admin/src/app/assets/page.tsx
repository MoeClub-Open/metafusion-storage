"use client";
// 资产查询：GET /api/storage/assets/:id（元数据 + 绑定）与 /content（原样预览）。
// 服务端把"不存在"与"不可读"都折叠成 404 not_found，界面必须照实说明这一点，
// 否则运维会以为是自己拼错了 id。
import React, { useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { isAdminWorkspaceEmbedded, prepareAdminNavigation } from "@/lib/adminWorkspace";
import { Badge, Button, Card, CopyField, DataList, EmptyState, Icon, Notice, SectionHeader, Spinner, TextInput, useMessage } from "@/components/ui";
import { BindingTable } from "@/components/BindingTable";
import { ApiError, describeApiError, normalizeIdInput, type Message } from "@/lib/api";
import { formatBytes, formatDateTime, isUuid } from "@/lib/format";
import { assetContentUrl, assetDownloadUrl, fetchAsset, fetchAssets, previewKind, type Asset, type AssetResponse } from "@/lib/storage";
import { useI18n } from "@/shared/i18n/I18nProvider";
import { useSession } from "@/components/SessionProvider";
import { PERMISSION_ASSET_MODERATE } from "@/lib/session";
import { BASE_PATH } from "@/lib/app";

export default function AssetsPage() {
  const { t, locale } = useI18n();
  const text = useMessage();
  const selectedId = useSearchParams().get("id");
  const { can } = useSession();
  const [value, setValue] = useState("");
  const [data, setData] = useState<AssetResponse | null>(null);
  const [error, setError] = useState<Message | null>(null);
  const [loading, setLoading] = useState(false);
  const [rows, setRows] = useState<Asset[]>([]);
  const [offset, setOffset] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [listLoading, setListLoading] = useState(false);
  const [listError, setListError] = useState<Message | null>(null);
  const [status, setStatus] = useState("");
  const [name, setName] = useState("");
  const listRequest = useRef(0);
  const assetRequest = useRef(0);
  useEffect(() => () => { listRequest.current += 1; assetRequest.current += 1; }, []);
  const [filter, setFilter] = useState({ status: "", name: "" });

  const loadList = useCallback(async (nextOffset: number, nextFilter: { status: string; name: string }) => {
    const request = ++listRequest.current;
    setListLoading(true);
    setRows([]);
    setHasMore(false);
    setListError(null);
    try {
      const result = await fetchAssets(50, nextOffset, nextFilter.status, nextFilter.name);
      if (request !== listRequest.current) return;
      setRows(result.assets);
      setHasMore(result.has_more);
      setOffset(nextOffset);
      setFilter(nextFilter);
    } catch (err) {
      if (request !== listRequest.current) return;
      setRows([]);
      setListError(describeApiError(err));
    } finally {
      if (request === listRequest.current) setListLoading(false);
    }
  }, []);

  useEffect(() => {
    if (can(PERMISSION_ASSET_MODERATE)) void loadList(0, { status: "", name: "" });
  }, [can, loadList]);

  const load = useCallback(async (assetId: string) => {
    const request = ++assetRequest.current;
    setLoading(true);
    setData(null);
    setError(null);
    try {
      const result = await fetchAsset(assetId);
      if (request === assetRequest.current) setData(result);
    } catch (err) {
      if (request !== assetRequest.current) return;
      setData(null);
      setError(describeApiError(err, { notFound: { key: "assets.notReadable" } }));
    } finally {
      if (request === assetRequest.current) setLoading(false);
    }
  }, []);

  // 深链：资产页在绑定页里被引用时带 ?id=<uuid> 过来，落地即查。
  useEffect(() => {
    const fromUrl = selectedId;
    if (fromUrl && isUuid(fromUrl)) {
      setValue(fromUrl);
      void load(fromUrl);
    } else {
      assetRequest.current += 1;
      setValue(fromUrl ?? ""); setData(null); setLoading(false);
      setError(fromUrl ? { key: "assets.invalidId" } : null);
    }
  }, [load, selectedId]);

  const submit = () => {
    const id = normalizeIdInput(value);
    if (!id) {
      assetRequest.current += 1;
      setLoading(false);
      setData(null);
      setError({ key: "assets.invalidId" });
      return;
    }
    setValue(id);
    const href = window.location.pathname + "?id=" + encodeURIComponent(id);
    prepareAdminNavigation(href);
    window.history.replaceState(window.history.state, "", href);
    if (id === selectedId) void load(id);
  };

  const asset = data?.asset ?? null;
  const bindings = data?.bindings ?? [];
  const kind = asset ? previewKind(asset.mime_type) : "none";

  return (
    <div className="flex flex-col gap-4">
    <div className="order-2">
    {can(PERMISSION_ASSET_MODERATE) ? <Card>
      <SectionHeader title={t("assets.listTitle")} actions={<Button variant="ghost" onClick={() => void loadList(offset, filter)} busy={listLoading}>{t("common.refresh")}</Button>} />
      <div className="flex flex-wrap items-end gap-2">
        <div className="min-w-40 flex-1"><TextInput value={name} onChange={setName} label={t("assets.filterName")} placeholder={t("assets.filterName")} onSubmit={() => void loadList(0, { status, name: name.trim() })} /></div>
        <label className="flex flex-col gap-1 text-xs text-text-muted">{t("assets.filterStatus")}
          <select className="min-h-10 rounded-control border border-line bg-surface px-3 text-text-body" value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("assets.allStatus")}</option><option value="complete">{t("assets.status.complete")}</option><option value="pending">{t("assets.status.pending")}</option>
          </select>
        </label>
        <Button onClick={() => void loadList(0, { status, name: name.trim() })} busy={listLoading}>{t("assets.query")}</Button>
      </div>
      {listLoading ? <p role="status" className="mt-3 flex items-center gap-2 text-xs text-text-muted"><Spinner />{t("common.loading")}</p> : null}
      {listError ? <div className="mt-3"><Notice tone="error">{text(listError)}</Notice></div> : null}
      {!listLoading && rows.length === 0 && !listError ? <div className="mt-3"><EmptyState>{t("assets.listEmpty")}</EmptyState></div> : null}
      {rows.length > 0 ? <div className="mt-4 overflow-x-auto"><table className="w-full min-w-[44rem] text-left text-xs">
        <thead className="border-b border-line text-text-faint"><tr><th className="px-3 py-2">{t("assets.field.fileName")}</th><th className="px-3 py-2">{t("assets.field.status")}</th><th className="px-3 py-2">{t("assets.field.sizeBytes")}</th><th className="px-3 py-2">{t("assets.field.createdAt")}</th></tr></thead>
        <tbody>{rows.map((row) => <tr key={row.id} className="border-b border-line-subtle align-top">
          <td className="px-3 py-3"><div className="flex items-center gap-3">{row.status === "complete" && !row.blocked && previewKind(row.mime_type) === "image" ? <img src={assetContentUrl(row.id)} alt="" loading="lazy" className="h-12 w-12 shrink-0 rounded-control border border-line object-contain" /> : null}<div><a href={`${BASE_PATH}/assets?id=${encodeURIComponent(row.id)}`} className="font-medium text-primary hover:underline">{row.file_name || row.id}</a><div className="font-mono text-[11px] text-text-faint">{row.id}</div></div></div></td>
          <td className="px-3 py-3"><StatusBadge status={row.status} /> {row.blocked ? <Badge tone="bad">{t("assets.blocked.yes")}</Badge> : null}</td>
          <td className="px-3 py-3">{formatBytes(row.size_bytes || row.declared_size, locale)}</td>
          <td className="px-3 py-3">{formatDateTime(row.created_at, locale)}</td>
        </tr>)}</tbody>
      </table></div> : null}
      <div className="mt-3 flex items-center gap-2"><Button variant="ghost" disabled={offset === 0 || listLoading} onClick={() => void loadList(Math.max(0, offset - 50), filter)}>{t("moderation.previous")}</Button><span className="text-xs text-text-muted">{t("moderation.page", { page: Math.floor(offset / 50) + 1 })}</span><Button variant="ghost" disabled={!hasMore || listLoading} onClick={() => void loadList(offset + 50, filter)}>{t("moderation.next")}</Button></div>
    </Card> : null}
    </div>
    <div className="order-1">
    <Card>
      <SectionHeader
        icon="search"
        title={t("assets.title")}
        desc={t("assets.desc")}
        actions={
          asset ? (
            <Button variant="ghost" onClick={() => void load(asset.id)} busy={loading}>
              {!loading ? <Icon name="refresh" className="w-3.5 h-3.5" /> : null}
              {t("common.refresh")}
            </Button>
          ) : null
        }
      />

      <div className="flex flex-col gap-2 sm:flex-row">
        <TextInput
          value={value}
          onChange={setValue}
          label={t("assets.idLabel")}
          placeholder={t("assets.idPlaceholder")}
          onSubmit={submit}
        />
        <Button onClick={submit} busy={loading}>
          {loading ? t("assets.querying") : t("assets.query")}
        </Button>
      </div>

      {error ? (
        <div className="mt-4">
          <Notice tone={error.key === "assets.notReadable" ? "warn" : "error"}>{text(error)}</Notice>
        </div>
      ) : null}

      {loading && !asset ? (
        <p className="mt-4 inline-flex items-center gap-2 text-xs text-text-muted">
          <Spinner className="w-3.5 h-3.5" />
          {t("assets.querying")}
        </p>
      ) : null}

      {!asset && !error && !loading ? (
        <div className="mt-4">
          <EmptyState>{t("assets.emptyState")}</EmptyState>
        </div>
      ) : null}

      {asset ? (
        <div className="mt-4 space-y-4">
          <section className="rounded-card border border-line p-4">
            <h3 className="mb-3 text-xs font-semibold text-text-strong">{t("assets.fileTitle")}</h3>
            <DataList
              items={[
                { label: t("assets.field.id"), value: asset.id, mono: true },
                { label: t("assets.field.status"), value: <StatusBadge status={asset.status} /> },
                { label: t("assets.field.blocked"), value: <Badge tone={asset.blocked ? "bad" : "ok"}>{t(asset.blocked ? "assets.blocked.yes" : "assets.blocked.no")}</Badge> },
                asset.blocked_reason ? { label: t("assets.field.blockedReason"), value: asset.blocked_reason } : null,
                asset.blocked_at ? { label: t("assets.field.blockedAt"), value: formatDateTime(asset.blocked_at, locale) } : null,
                {
                  label: t("assets.field.hashVerified"),
                  value: (
                    <span className="inline-flex flex-wrap items-center gap-2">
                      <Badge tone={asset.hash_verified ? "ok" : "warn"}>
                        {asset.hash_verified ? t("assets.hashVerified.yes") : t("assets.hashVerified.no")}
                      </Badge>
                    </span>
                  ),
                },
                { label: t("assets.field.fileName"), value: asset.file_name, mono: true },
                {
                  label: t("assets.field.sizeBytes"),
                  value: formatBytes(asset.size_bytes, locale) + " (" + asset.size_bytes + ")",
                  mono: true,
                },
                {
                  label: t("assets.field.declaredSize"),
                  value: formatBytes(asset.declared_size, locale) + " (" + asset.declared_size + ")",
                  mono: true,
                },
                { label: t("assets.field.mimeType"), value: asset.mime_type, mono: true },
                { label: t("assets.field.uploader"), value: asset.uploader_id, mono: true },
                { label: t("assets.field.createdAt"), value: formatDateTime(asset.created_at, locale) },
                asset.upload_expires_at ? { label: t("assets.field.uploadExpiresAt"), value: formatDateTime(asset.upload_expires_at, locale) } : null,
                { label: t("assets.field.completedAt"), value: formatDateTime(asset.completed_at, locale) },
                asset.fail_reason
                  ? { label: t("assets.field.failReason"), value: asset.fail_reason, mono: true }
                  : null,
              ].filter(Boolean) as { label: string; value: React.ReactNode; mono?: boolean }[]}
            />
            {can(PERMISSION_ASSET_MODERATE) ? <Link replace={isAdminWorkspaceEmbedded()} onClick={(event) => prepareAdminNavigation(event.currentTarget.href, event)} href="/moderation" className="mt-4 inline-flex text-xs text-primary hover:underline">{t("nav.moderation")}</Link> : null}
            <div className="mt-4 space-y-2">
              <div>
                <div className="mb-1 text-[11px] uppercase tracking-wide text-text-faint">{t("assets.field.sha256")}</div>
                <CopyField value={asset.sha256} label={t("assets.field.sha256")} />
              </div>
              <div>
                <div className="mb-1 text-[11px] uppercase tracking-wide text-text-faint">{t("assets.field.objectKey")}</div>
                <CopyField value={asset.object_key} label={t("assets.field.objectKey")} />
              </div>
            </div>
          </section>

          <section className="rounded-card border border-line p-4">
            <h3 className="mb-1 text-xs font-semibold text-text-strong">{t("assets.previewTitle")}</h3>
            <p className="mb-3 text-[11px] leading-relaxed text-text-muted">{t("assets.previewNote")}</p>

            {asset.blocked ? (
              <Notice tone="warn">{t("assets.previewBlocked")}</Notice>
            ) : asset.status !== "complete" ? (
              <Notice tone="warn">{t("assets.previewNotComplete", { status: asset.status })}</Notice>
            ) : kind === "image" ? (
              <img
                src={assetContentUrl(asset.id)}
                alt={asset.file_name}
                className="max-h-96 w-auto rounded-card border border-line bg-surfaceSubtle object-contain"
              />
            ) : kind === "video" ? (
              <video controls src={assetContentUrl(asset.id)} className="max-h-96 w-full rounded-card border border-line" />
            ) : kind === "audio" ? (
              <audio controls src={assetContentUrl(asset.id)} className="w-full" />
            ) : kind === "pdf" ? (
              <iframe
                title={asset.file_name}
                src={assetContentUrl(asset.id)}
                className="h-96 w-full rounded-card border border-line bg-surfaceSubtle"
              />
            ) : (
              <Notice tone="info">{t("assets.previewUnsupported", { mime: asset.mime_type || t("common.unknownValue") })}</Notice>
            )}

            <div className="mt-3 space-y-2">
              <CopyField value={assetContentUrl(asset.id)} label={t("assets.previewTitle")} />
              <a
                className="inline-flex items-center gap-1.5 text-xs text-primary underline underline-offset-2"
                href={assetContentUrl(asset.id)}
                target="_blank"
                rel="noreferrer"
              >
                <Icon name="external" className="w-3.5 h-3.5" />
                {t("assets.previewOpen")}
              </a>
            </div>
          </section>

          <section className="rounded-card border border-line p-4">
            <h3 className="mb-1 text-xs font-semibold text-text-strong">{t("assets.downloadTitle")}</h3>
            <p className="mb-3 text-[11px] leading-relaxed text-text-muted">{t("assets.downloadNote")}</p>
            <CopyField value={assetDownloadUrl(asset.id)} label={t("assets.downloadTitle")} />
          </section>

          <section className="rounded-card border border-line p-4">
            <BindingTable
              rows={bindings}
              title={t("assets.bindingsTitle", { count: bindings.length })}
              emptyLabel={t("assets.bindingsEmpty")}
              onReload={() => load(asset.id)}
            />
          </section>
        </div>
      ) : null}
    </Card>
    </div>
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const { tr } = useI18n();
  const tone = status === "complete" ? "ok" : status === "pending" ? "warn" : "neutral";
  return <Badge tone={tone}>{tr("assets.status." + status, status)}</Badge>;
}
