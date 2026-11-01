/** @type {import('next').NextConfig} */
const nextConfig = {
  // 静态导出：产物直接由 Go 侧 go:embed 托管，单二进制部署
  output: "export",
  trailingSlash: true,
  images: { unoptimized: true },
  reactStrictMode: true,
  eslint: { ignoreDuringBuilds: true },
};

export default nextConfig;
