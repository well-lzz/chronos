import "./globals.css";

export const metadata = {
  title: "Chronos · 安全任务控制台",
  description: "Chronos Agent 实时执行观测、发现管理与操作员控制台",
};

export default function RootLayout({ children }) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}
