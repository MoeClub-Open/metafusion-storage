"use client";
// 绑定解绑工作台：存储服务只提供"删绑定"这一个绑定类端点（没有按 id 读绑定的接口），
// 因此入口按能"看到绑定"的三种方式来组织：按资产、按实体、直接按绑定 id。
// 前两种能在解绑后重新取数，第三种只能报结果——没有列表可刷新，界面上照实写明。
import React, { useCallback, useEffect, useState } from "react";
import { BindingTable } from "@/components/BindingTable";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  Icon,
  Notice,
  SectionHeader,
  Spinner,
  TextInput,
  useMessage,
} from "@/components/ui";
import { ApiError, describeApiError, type Message } from "@/lib/api";
import { extractUuid, formatBytes } from "@/lib/format";
import { deleteBinding, fetchAsset, fetchBindings, fetchEntityFiles, type AssetResponse, type EntityFilesResponse, type FileBinding } from "@/lib/storage";
import { useI18n } from "@/shared/i18n/I18nProvider";
import { useSession } from "@/components/SessionProvider";
import { PERMISSION_ASSET_MODERATE } from "@/lib/session";

type Mode = "asset" | "entity" | "binding";

export default function BindingsPage() {
  const { t, locale } = useI18n();
  const text = useMessage();
  const { can } = useSession();
  const [list, setList] = useState<FileBinding[]>([]);
  const [listOffset, setListOffset] = useState(0);
  const [listMore, setListMore] = useState(false);
  const [listLoading, setListLoading] = useState(false);
  const [listError, setListError] = useState<Message | null>(null);

  const loadList = useCallback(async (offset: number) => {
    setListLoading(true);
    setListError(null);
    try {
      const result = await fetchBindings(50, offset);
      setList(result.bindings);
      setListMore(result.has_more);
      setListOffset(offset);
    } catch (err) {
      setList([]);
      setListError(describeApiError(err));
    } finally { setListLoading(false); }
  }, []);

  useEffect(() => { if (can(PERMISSION_ASSET_MODERATE)) void loadList(0); }, [can, loadList]);

  const [mode, setMode] = useState<Mode>("asset");

  const [assetValue, setAssetValue] = useState("");
  const [assetData, setAssetData] = useState<AssetResponse | null>(null);
  const [assetError, setAssetError] = useState<Message | null>(null);
  const [assetLoading, setAssetLoading] = useState(false);

  const [entityValue, setEntityValue] = useState("");
  const [entityData, setEntityData] = useState<EntityFilesResponse | null>(null);
  const [entityError, setEntityError] = useState<Message | null>(null);
  const [entityLoading, setEntityLoading] = useState(false);

  const [bindingValue, setBindingValue] = useState("");
  const [bindingError, setBindingError] = useState<Message | null>(null);
  const [bindingNotice, setBindingNotice] = useState<{ tone: "ok" | "warn" | "error"; message: Message } | null>(null);
  const [bindingBusy, setBindingBusy] = useState(false);
  const [confirmBinding, setConfirmBinding] = useState<string | null>(null);

  const loadAsset = useCallback(async (id: string) => {
    setAssetLoading(true);
    setAssetError(null);
    try {
      setAssetData(await fetchAsset(id));
    } catch (err) {
      setAssetData(null);
      setAssetError(describeApiError(err, { notFound: { key: "assets.notReadable" } }));
    } finally {
      setAssetLoading(false);
    }
  }, []);

  const loadEntity = useCallback(async (id: string) => {
    setEntityLoading(true);
    setEntityError(null);
    try {
      setEntityData(await fetchEntityFiles(id));
    } catch (err) {
      setEntityData(null);
      setEntityError(describeApiError(err, { notFound: { key: "bindings.entityNotFound" } }));
    } finally {
      setEntityLoading(false);
    }
  }, []);

  const submitAsset = () => {
    const id = extractUuid(assetValue);
    if (!id) {
      setAssetData(null);
      setAssetError({ key: "bindings.invalidId" });
      return;
    }
    setAssetValue(id);
    void loadAsset(id);
  };

  const submitEntity = () => {
    const id = extractUuid(entityValue);
    if (!id) {
      setEntityData(null);
      setEntityError({ key: "bindings.invalidId" });
      return;
    }
    setEntityValue(id);
    void loadEntity(id);
  };

  const submitBinding = () => {
    const id = extractUuid(bindingValue);
    if (!id) {
      setBindingNotice(null);
      setBindingError({ key: "bindings.invalidId" });
      return;
    }
    setBindingError(null);
    setBindingNotice(null);
    setBindingValue(id);
    setConfirmBinding(id); // 破坏性动作一律二次确认
  };

  const unbindDirect = async (id: string) => {
    setBindingBusy(true);
    setBindingNotice(null);
    try {
      await deleteBinding(id);
      setBindingNotice({ tone: "ok", message: { key: "bindings.unbindDone" } });
    } catch (err) {
      if (err instanceof ApiError && (err.status === 404 || err.code === "not_found")) {
        // 解绑不幂等：第二次删同一个 id 就是 404，讲成"已不存在"而不是失败。
        setBindingNotice({ tone: "warn", message: { key: "bindings.unbindGone" } });
      } else if (err instanceof ApiError && (err.status === 403 || err.code === "forbidden")) {
        setBindingNotice({ tone: "error", message: { key: "bindings.unbindForbidden" } });
      } else {
        setBindingNotice({ tone: "error", message: describeApiError(err) });
      }
    } finally {
      setBindingBusy(false);
      setConfirmBinding(null);
    }
  };

  return (
    <div className="space-y-3">
      <Card>
        <SectionHeader icon="unlink" title={t("bindings.title")} />

        <div className="flex flex-wrap gap-1" role="tablist" aria-label={t("bindings.title")}>
          {(
            [
              ["asset", "bindings.modeAsset", "search"],
              ["entity", "bindings.modeEntity", "file"],
              ["binding", "bindings.modeBinding", "unlink"],
            ] as const
          ).map(([key, label, icon]) => (
            <button
              key={key}
              type="button"
              role="tab"
              aria-selected={mode === key}
              onClick={() => setMode(key)}
              className={
                "inline-flex items-center gap-1.5 rounded-control border px-3 py-2 text-xs transition-colors " +
                (mode === key
                  ? "border-primary/40 bg-primary/10 text-primary"
                  : "border-line text-text-muted hover:bg-surfaceHover")
              }
            >
              <Icon name={icon} className="w-3.5 h-3.5" />
              {t(label)}
            </button>
          ))}
        </div>
      </Card>

      {mode === "asset" ? (
        <Card>
          <div className="flex flex-col gap-2 sm:flex-row">
            <TextInput
              value={assetValue}
              onChange={setAssetValue}
              label={t("bindings.assetLabel")}
              placeholder={t("bindings.assetPlaceholder")}
              onSubmit={submitAsset}
            />
            <Button onClick={submitAsset} busy={assetLoading}>
              {assetLoading ? t("bindings.loading") : t("bindings.load")}
            </Button>
          </div>

          {assetError ? (
            <div className="mt-3">
              <Notice tone="warn">{text(assetError)}</Notice>
            </div>
          ) : null}

          {assetLoading ? (
            <p className="mt-3 inline-flex items-center gap-2 text-xs text-text-muted">
              <Spinner className="w-3.5 h-3.5" />
              {t("bindings.loading")}
            </p>
          ) : null}

          {assetData ? (
            <div className="mt-4 space-y-3">
              <div className="flex flex-wrap items-center gap-2 text-[11px] text-text-muted">
                <span className="font-mono text-text-body">{assetData.asset.file_name}</span>
                <span>·</span>
                <span className="font-mono">{formatBytes(assetData.asset.size_bytes, locale)}</span>
                <span>·</span>
                <span className="font-mono text-text-faint">{assetData.asset.id}</span>
              </div>
              <BindingTable
                rows={assetData.bindings ?? []}
                title={t("assets.bindingsTitle", { count: (assetData.bindings ?? []).length })}
                emptyLabel={t("bindings.assetEmpty")}
                onReload={() => loadAsset(assetData.asset.id)}
              />
            </div>
          ) : null}
        </Card>
      ) : null}

      {mode === "entity" ? (
        <Card>
          <div className="flex flex-col gap-2 sm:flex-row">
            <TextInput
              value={entityValue}
              onChange={setEntityValue}
              label={t("bindings.entityLabel")}
              placeholder={t("bindings.entityPlaceholder")}
              onSubmit={submitEntity}
            />
            <Button onClick={submitEntity} busy={entityLoading}>
              {entityLoading ? t("bindings.loading") : t("bindings.load")}
            </Button>
          </div>

          {entityError ? (
            <div className="mt-3">
              <Notice tone="warn">{text(entityError)}</Notice>
            </div>
          ) : null}

          {entityLoading ? (
            <p className="mt-3 inline-flex items-center gap-2 text-xs text-text-muted">
              <Spinner className="w-3.5 h-3.5" />
              {t("bindings.loading")}
            </p>
          ) : null}

          {entityData ? (
            <div className="mt-4 space-y-3">
              <div className="flex flex-wrap items-center gap-2 text-[11px] text-text-muted">
                <span className="font-mono text-text-body">{entityData.target_kind}</span>
                <span>·</span>
                <span className="font-mono text-text-faint">{entityData.target_entity_id}</span>
              </div>
              {(entityData.files ?? []).length === 0 ? (
                <EmptyState>{t("bindings.entityEmpty")}</EmptyState>
              ) : (
                <BindingTable
                  rows={entityData.files ?? []}
                  title={t("assets.bindingsTitle", { count: (entityData.files ?? []).length })}
                  emptyLabel={t("bindings.entityEmpty")}
                  onReload={() => loadEntity(entityData.target_entity_id)}
                  showAsset
                />
              )}
            </div>
          ) : null}
        </Card>
      ) : null}

      {mode === "binding" ? (
        <Card>
          <div className="flex flex-col gap-2 sm:flex-row">
            <TextInput
              value={bindingValue}
              onChange={setBindingValue}
              label={t("bindings.bindingLabel")}
              placeholder={t("bindings.bindingPlaceholder")}
              onSubmit={submitBinding}
            />
            <Button variant="danger" onClick={submitBinding}>
              <Icon name="unlink" className="w-3.5 h-3.5" />
              {t("bindings.unbind")}
            </Button>
          </div>

          {bindingError ? (
            <div className="mt-3">
              <Notice tone="warn">{text(bindingError)}</Notice>
            </div>
          ) : null}

          {bindingNotice ? (
            <div className="mt-3">
              <Notice tone={bindingNotice.tone}>{text(bindingNotice.message)}</Notice>
            </div>
          ) : null}

          <div className="mt-3">
            <Notice tone="info" title={t("bindings.notIdempotentTitle")}>
              {t("bindings.notIdempotentBody")}
            </Notice>
          </div>
        </Card>
      ) : null}

      {can(PERMISSION_ASSET_MODERATE) ? <Card>
        <div className="mb-3 flex justify-end"><Button variant="ghost" onClick={() => void loadList(listOffset)} busy={listLoading}>{t("common.refresh")}</Button></div>
        {listError ? <Notice tone="error">{text(listError)}</Notice> : null}
        <BindingTable rows={list} title={t("bindings.listTitle")} emptyLabel={t("bindings.listEmpty")} loading={listLoading} onReload={() => loadList(listOffset)} showAsset />
        <div className="mt-3 flex items-center gap-2"><Button variant="ghost" disabled={listOffset === 0 || listLoading} onClick={() => void loadList(Math.max(0, listOffset - 50))}>{t("moderation.previous")}</Button><span className="text-xs text-text-muted">{t("moderation.page", { page: Math.floor(listOffset / 50) + 1 })}</span><Button variant="ghost" disabled={!listMore || listLoading} onClick={() => void loadList(listOffset + 50)}>{t("moderation.next")}</Button></div>
      </Card> : null}

      <ConfirmDialog
        open={confirmBinding !== null}
        title={t("bindings.unbindTitle")}
        body={t("bindings.unbindDirectBody", { id: confirmBinding ?? "" })}
        confirmLabel={t("bindings.unbind")}
        busy={bindingBusy}
        onConfirm={() => {
          if (confirmBinding) void unbindDirect(confirmBinding);
        }}
        onCancel={() => setConfirmBinding(null)}
      />
    </div>
  );
}
