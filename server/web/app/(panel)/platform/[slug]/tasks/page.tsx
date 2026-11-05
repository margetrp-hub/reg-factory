import { PlatformSubview } from "@/components/platform-subview";

const SLUGS = ["claude", "chatgpt", "grok", "outlook"];

export function generateStaticParams() {
  return SLUGS.map((slug) => ({ slug }));
}

export default async function Page({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  return <PlatformSubview slug={slug} tab="tasks" />;
}
