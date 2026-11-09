import { Suspense } from "react";
import { CredentialChecks } from "@/components/credential-checks";
import { PLATFORMS } from "@/lib/task-catalog";

export function generateStaticParams() { return PLATFORMS.map(({slug})=>({slug})); }

export default async function Page({params}: {params: Promise<{slug: string}>}) {
  const {slug}=await params;
  return <Suspense fallback={<div className="sg-table-skeleton">正在读取本次运行…</div>}><CredentialChecks slug={slug} view="run"/></Suspense>;
}
