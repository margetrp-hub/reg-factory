// Only account-list URLs may be used as the return target of a task page.
export function accountReturn(value: string | null | undefined, fallback: string): string {
  if (!value || !/^\/(?:accounts\/|platform\/[a-z0-9-]+\/accounts\/)(?:\?[^#]*)?$/.test(value)) return fallback;
  return value;
}

export function accountDate(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", {timeZone:"Asia/Shanghai", year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false}).format(date);
}
