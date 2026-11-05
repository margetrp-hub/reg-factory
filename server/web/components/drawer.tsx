"use client";

import { ReactNode, useEffect } from "react";

/** 右侧抽屉：详情与编辑就地完成，不跳页。 */
export function Drawer({
  open,
  title,
  subtitle,
  onClose,
  children,
  footer,
  width = 460,
}: {
  open: boolean;
  title: ReactNode;
  subtitle?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <div className="absolute inset-0 bg-background/60 backdrop-blur-[1px]" onClick={onClose} />
      <aside
        style={{ width }}
        className="relative flex h-full max-w-[92vw] flex-col border-l border-border bg-card shadow-2xl"
      >
        <header className="flex items-start gap-3 border-b border-border px-4 py-3">
          <div className="min-w-0 flex-1">
            <div className="truncate text-[13.5px] font-semibold">{title}</div>
            {subtitle && <div className="mt-0.5 truncate text-[11px] text-muted-foreground">{subtitle}</div>}
          </div>
          <button className="btn-ghost px-2 py-0.5 text-[11px]" onClick={onClose}>关闭</button>
        </header>
        <div className="min-h-0 flex-1 overflow-auto px-4 py-3">{children}</div>
        {footer && <footer className="flex items-center justify-end gap-2 border-t border-border px-4 py-2.5">{footer}</footer>}
      </aside>
    </div>
  );
}
