import "./globals.css";

export const metadata = {
  title: "chronos console",
  description: "Live run visualization + operator control for the chronos scheduler",
};

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
