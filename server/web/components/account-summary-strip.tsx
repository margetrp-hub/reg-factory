import type { AccountSummary } from "@/lib/api";

export function AccountSummaryStrip({summary}: {summary: AccountSummary | null}) {
  const items = [
    {key:"available", label:"可用", tone:"text-success"},
    {key:"abnormal", label:"需处理", tone:"text-destructive"},
    {key:"unknown", label:"待核验", tone:"text-muted-foreground"},
    {key:"leased", label:"处理中", tone:"text-warning"},
    {key:"used", label:"已使用", tone:"text-muted-foreground"},
  ] as const;
  return <section aria-label="账号分类统计" className="flex flex-wrap items-center gap-x-7 gap-y-3 border-y border-border px-4 py-4 text-[13px]">
    <div><span className="text-muted-foreground">账号</span><strong className="ml-2 text-lg font-medium tabular-nums">{summary?.total ?? "—"}</strong><span className="ml-3 text-xs text-muted-foreground">{summary ? `${summary.source_count} 份来源` : "正在读取"}</span></div>
    {items.map(item=><div key={item.key} className="flex items-center gap-2"><span className="text-muted-foreground">{item.label}</span><strong className={`font-medium tabular-nums ${item.tone}`}>{summary?.[item.key] ?? "—"}</strong></div>)}
  </section>;
}
