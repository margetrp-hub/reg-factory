import { PlatformView } from "@/components/platform-view";

// 静态导出：平台 slug 在这里显式声明（client 组件的常量不能直接用于 generateStaticParams）
const SLUGS = ["claude", "chatgpt", "grok", "outlook"];

export function generateStaticParams() {
  return SLUGS.map((slug) => ({ slug }));
}

export default async function PlatformPage({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  return <PlatformView slug={slug} />;
}
