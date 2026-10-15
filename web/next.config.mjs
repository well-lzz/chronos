/** @type {import('next').NextConfig} */
const nextConfig = {
  // Static export: `next build` emits ./out, which Go embeds and serves — the
  // single-binary deploy is preserved.
  output: "export",
  trailingSlash: true,
  images: { unoptimized: true },
  // Dev only (ignored by the static export): proxy /api to the Go backend so
  // `next dev` on :3000 can talk to `chronos --web` on :8000.
  async rewrites() {
    return [{ source: "/api/:path*", destination: "http://127.0.0.1:8000/api/:path*" }];
  },
};
export default nextConfig;
