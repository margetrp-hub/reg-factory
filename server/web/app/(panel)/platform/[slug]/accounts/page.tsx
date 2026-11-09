import { PlatformSubview } from "@/components/platform-subview";

import { PLATFORMS } from "@/lib/task-catalog";

export function generateStaticParams() {
  return PLATFORMS.map(({slug}) => ({ slug }));
}

export default async function Page({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  return <PlatformSubview slug={slug} tab="accounts" />;
}
