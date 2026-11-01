import type { Metadata, Viewport } from "next";
import { cookies } from "next/headers";
import { AppShell } from "@/components/AppShell";
import { SessionProvider } from "@/components/SessionProvider";
import { getMessages } from "@/shared/i18n/getMessages";
import { I18nProvider } from "@/shared/i18n/I18nProvider";
import { localeCookieName, normalizeLocale } from "@/shared/i18n/routing";
import "@/shared/theme/globals.css";
import { ThemeProvider } from "@/lib/themeContext";
import { DEFAULT_ACCENT, DEFAULT_TONE, DEFAULT_PATTERN, DEFAULT_BG_DARK } from "@/lib/theme.generated";
import { themeBootScript } from "@/shared/theme/boot.generated";

export const viewport: Viewport = { themeColor: DEFAULT_BG_DARK };

// 语言在服务端就定下来（读 NEXT_LOCALE cookie），避免首屏先按默认语言渲染再跳到用户语言。
//
// robots 必须写在这里，不能只靠 public/robots.txt：本应用 basePath 是 /admin/storage，
// 那份 robots.txt 会被服务在 /admin/storage/robots.txt，而爬虫只读源站根的 /robots.txt
// （由主前端作答），等于没有任何收录防护。账号与社区两个管理台同样是靠这条 meta 收敛的。
// cookies() 是异步请求 API（Next 15 起返回 Promise，Next 16 不再接受同步取值）。
export async function generateMetadata(): Promise<Metadata> {
  const locale = normalizeLocale((await cookies()).get(localeCookieName)?.value);
  const messages = getMessages(locale);
  return {
    title: messages["meta.title"],
    description: messages["meta.description"],
    robots: { index: false, follow: false },
  };
}

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  const locale = normalizeLocale((await cookies()).get(localeCookieName)?.value);
  return (
    <html lang={locale} className="dark" data-theme-mode="dark" data-theme-accent={DEFAULT_ACCENT} data-theme-tone={DEFAULT_TONE} data-theme-pattern={DEFAULT_PATTERN} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeBootScript }} />
      </head>
      <body className="font-sans antialiased">
        <ThemeProvider>
        <I18nProvider initialLocale={locale}>
          <SessionProvider>
            <AppShell>{children}</AppShell>
          </SessionProvider>
        </I18nProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
