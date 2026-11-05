"use client";

import { ReactNode } from "react";

export type TabItem = { id: string; label: string; count?: number };

/** 轻量标签页：一次只渲染一组内容，避免整页平铺。 */
export function Tabs({
  items,
  value,
  onChange,
  right,
}: {
  items: TabItem[];
  value: string;
  onChange: (id: string) => void;
  right?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-border pb-2">
      <div className="flex flex-1 flex-wrap gap-1">
        {items.map((item) => {
          const active = value === item.id;
          return (
            <button
              key={item.id}
              onClick={() => onChange(item.id)}
              className={`rounded-md px-2.5 py-1 text-[12px] transition-colors ${
                active
                  ? "bg-brand/15 font-medium text-brand"
                  : "text-muted-foreground hover:bg-accent/60 hover:text-foreground"
              }`}
            >
              {item.label}
              {item.count != null && (
                <span className={`ml-1 font-mono text-[10px] ${active ? "text-brand/80" : "text-muted-foreground/70"}`}>
                  {item.count}
                </span>
              )}
            </button>
          );
        })}
      </div>
      {right}
    </div>
  );
}
