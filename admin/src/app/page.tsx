"use client";
// 用量总览：GET /api/storage/stats 返回完成态用量、待完成与禁发数。
import React, { useCallback, useEffect, useState } from "react";
import { Button, Card, Icon, Notice, SectionHeader, Spinner, useMessage } from "@/components/ui";
import { useSession } from "@/components/SessionProvider";
import { useI18n } from "@/shared/i18n/I18nProvider";
import { describeApiError, type Message } from "@/lib/api";
import { PERMISSION_ASSET_MODERATE } from "@/lib/session";
import { fetchStats, type StatsResponse } from "@/lib/storage";
import { formatBytes, formatInt } from "@/lib/format";

export default function OverviewPage() {
  const { t, locale } = useI18n();
  const text = useMessage();
  const { state, can } = useSession();
  const permitted = can(PERMISSION_ASSET_MODERATE);

  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [error, setError] = useState<Message | null>(null);
  const [loading, setLoading] = useState(false);
  const [fetchedAt, setFetchedAt] = useState<string>("");

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await fetchStats();
      setStats(data);
      setFetchedAt(new Date().toISOString());
    } catch (err) {
      setStats(null);
      setError(describeApiError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (state.status === "ready" && permitted) void load();
  }, [state.status, permitted, load]);

  return (
    <Card>
      <SectionHeader
        icon="gauge"
        title={t("overview.title")}
        desc={t("overview.desc")}
        actions={
          permitted ? (
            <Button variant="ghost" onClick={() => void load()} busy={loading} disabled={loading}>
              {!loading ? <Icon name="refresh" className="w-3.5 h-3.5" /> : null}
              {t("common.refresh")}
            </Button>
          ) : null
        }
      />

      {!permitted ? (
        <Notice tone="warn" title={t("common.requiredPermission", { code: PERMISSION_ASSET_MODERATE })}>
          {t("overview.forbidden")}
        </Notice>
      ) : (
        <div className="space-y-4">
          {error ? <Notice tone="error">{text(error)}</Notice> : null}

          {loading && !stats ? (
            <p className="inline-flex items-center gap-2 text-xs text-text-muted">
              <Spinner className="w-3.5 h-3.5" />
              {t("overview.loading")}
            </p>
          ) : null}

          {stats ? (
            <>
              <div className="grid grid-cols-2 gap-5 border-y border-line-subtle py-4 xl:grid-cols-4">
                <div className="min-w-0">
                  <div className="text-[11px] uppercase tracking-wide text-text-faint">{t("overview.assetsLabel")}</div>
                  <div className="mt-2 font-mono text-2xl text-text-strong">{formatInt(stats.assets, locale)}</div>
                  <div className="mt-2 text-[11px] leading-relaxed text-text-muted">{t("overview.assetsHint")}</div>
                </div>
                <div className="min-w-0">
                  <div className="text-[11px] uppercase tracking-wide text-text-faint">{t("overview.bytesLabel")}</div>
                  <div className="mt-2 font-mono text-2xl text-text-strong">{formatBytes(stats.bytes, locale)}</div>
                  <div className="mt-2 text-[11px] leading-relaxed text-text-muted">{t("overview.bytesHint")}</div>

                </div>
                <div className="min-w-0">
                  <div className="text-[11px] uppercase tracking-wide text-text-faint">{t("overview.pendingLabel")}</div>
                  <div className="mt-2 font-mono text-2xl text-text-strong">{formatInt(stats.pending, locale)}</div>
                </div>
                <div className="min-w-0">
                  <div className="text-[11px] uppercase tracking-wide text-text-faint">{t("overview.blockedLabel")}</div>
                  <div className="mt-2 font-mono text-2xl text-text-strong">{formatInt(stats.blocked, locale)}</div>
                </div>
              </div>

              {stats.assets === 0 ? <Notice tone="info">{t("overview.emptyUsage")}</Notice> : null}

              <p className="text-[11px] leading-relaxed text-text-muted">{t("overview.pendingNote")}</p>
              {fetchedAt ? (
                <p className="font-mono text-[11px] text-text-faint">
                  {t("overview.updatedAt", { time: new Intl.DateTimeFormat(locale, { timeStyle: "medium" }).format(new Date(fetchedAt)) })}
                </p>
              ) : null}
            </>
          ) : null}

        </div>
      )}
    </Card>
  );
}
