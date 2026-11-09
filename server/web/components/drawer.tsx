"use client";

import { ReactNode, useEffect, useId, useRef } from "react";

/** 右侧抽屉：详情与编辑就地完成，不跳页。 */
export function Drawer({
  open,
  title,
  subtitle,
  onClose,
  onClosed,
  children,
  footer,
  width = 460,
  variant = "drawer",
}: {
  open: boolean;
  title: ReactNode;
  subtitle?: ReactNode;
  onClose: () => void;
  onClosed?: () => void;
  children: ReactNode;
  footer?: ReactNode;
  width?: number;
  variant?: "drawer" | "dialog";
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const previousOverflow = useRef<string | null>(null);
  const afterClose = useRef(onClosed);
  afterClose.current = onClosed;
  const titleId = useId();
  const subtitleId = useId();
  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    if (open) {
      if (!element.open) {
        previousOverflow.current = document.body.style.overflow;
        document.body.style.overflow = "hidden";
        element.showModal();
      }
      return;
    }
    if (!element.open) return;
    const finish = () => {
      element.close();
      if (previousOverflow.current !== null) {
        document.body.style.overflow = previousOverflow.current;
        previousOverflow.current = null;
      }
      afterClose.current?.();
    };
    const timer = window.setTimeout(finish, window.matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 160);
    return () => window.clearTimeout(timer);
  }, [open]);
  useEffect(() => () => {
    if (previousOverflow.current !== null) document.body.style.overflow = previousOverflow.current;
  }, []);

  return (
    <dialog ref={dialog} className={`sg-drawer${variant === "dialog" ? " sg-dialog" : ""}`} style={{ width }} data-state={open ? "open" : "closed"}
      aria-labelledby={titleId} aria-describedby={subtitle ? subtitleId : undefined}
      onCancel={event => { event.preventDefault(); onClose(); }}
      onClick={event => {
        if (event.target !== event.currentTarget) return;
        const box = event.currentTarget.getBoundingClientRect();
        if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) onClose();
      }}>
      <div className="flex h-full min-h-0 flex-col">
        <header className="flex shrink-0 items-start gap-4 border-b border-border px-6 py-5">
          <div className="min-w-0 flex-1">
            <h2 id={titleId} className="text-base font-semibold tracking-tight">{title}</h2>
            {subtitle && <p id={subtitleId} className="mt-1.5 text-[13px] leading-relaxed text-muted-foreground">{subtitle}</p>}
          </div>
          <button type="button" className="btn-ghost" onClick={onClose} aria-label="关闭面板">关闭</button>
        </header>
        <div className="min-h-0 flex-1 overflow-auto px-6 py-5">{children}</div>
        {footer && <footer className="flex shrink-0 items-center justify-end gap-2 border-t border-border px-6 py-4">{footer}</footer>}
      </div>
    </dialog>
  );
}
