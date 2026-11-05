import { MaterialsView } from "@/components/materials-view";

const KINDS = ["mail", "proxy", "sms", "captcha"] as const;
type Kind = (typeof KINDS)[number];

export function generateStaticParams() {
  return KINDS.map((kind) => ({ kind }));
}

export default async function MaterialsKindPage({ params }: { params: Promise<{ kind: string }> }) {
  const { kind } = await params;
  const safe = (KINDS as readonly string[]).includes(kind) ? (kind as Kind) : "mail";
  return <MaterialsView kind={safe} />;
}
