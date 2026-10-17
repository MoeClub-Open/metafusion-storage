# syntax=docker/dockerfile:1
# ==============================================================================
# MetaFusion 存储管理台（storage-admin）
#
# 冻结契约（主仓库 deploy/docker-compose.yml 的 storage-admin 服务与 deploy/nginx.conf）：
#   build.context = 存储仓库根（${MF_STORAGE_DIR:-../../metafusion-storage}），
#   build.dockerfile = admin/Dockerfile —— 因此下面所有 COPY 路径都相对**仓库根**，不是 admin/；
#   容器内端口 3000；网关把 /admin/storage/ 反代到这里；健康端点 /admin/storage/api/health。
#
# 多阶段 standalone 模式照抄主仓库 frontend/Dockerfile（样板），构建器用 Bun：
# Next 的 standalone 产物自带最小 server.js，运行时不需要 node_modules。
# ==============================================================================
FROM oven/bun:1.4.3-alpine AS base

FROM base AS deps
WORKDIR /app
# 按提交的 bun.lock 安装；锁文件缺失或漂移时构建失败。
COPY admin/package.json admin/bun.lock ./
RUN --mount=type=cache,target=/root/.bun \
    bun install --frozen-lockfile

FROM base AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY admin/ ./
ENV NEXT_TELEMETRY_DISABLED 1
RUN --mount=type=cache,target=/app/.next/cache \
    bun run build

FROM base AS runner
WORKDIR /app
ENV NODE_ENV production
ENV NEXT_TELEMETRY_DISABLED 1
RUN addgroup --system --gid 1001 nodejs && adduser --system --uid 1001 nextjs
COPY --from=builder /app/public ./public
RUN mkdir -p .next && chown nextjs:nodejs .next
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static
USER nextjs
EXPOSE 3000
ENV PORT 3000
ENV HOSTNAME "0.0.0.0"
CMD ["bun", "server.js"]
