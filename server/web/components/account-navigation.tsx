import Link from "next/link";

export function AccountNavigation({slug, active, accountsHref, checksHref, runHref}: {slug: string; active: "accounts" | "checks" | "run"; accountsHref?: string; checksHref?: string; runHref?: string}) {
  return <nav aria-label="账号工作区" className="sg-account-nav">
    <Link href={accountsHref || `/platform/${slug}/accounts/`} aria-current={active === "accounts" ? "page" : undefined}>账号管理</Link>
    <Link href={checksHref || `/platform/${slug}/checks/`} aria-current={active === "checks" ? "page" : undefined}>凭据检查</Link>
    {runHref && <Link href={runHref} aria-current={active === "run" ? "page" : undefined}>本次运行</Link>}
  </nav>;
}

export const CHECK_LABELS: Record<string,string> = {ok:"AT 有效",banned:"账号停用",invalid:"凭据无效",expired:"凭据过期",needs_refresh:"AT 待刷新",unknown:"结果待核对",relogin_required:"需重新授权",pending:"未执行",running:"检查中"};
export const checkTone = (status: string) => status === "ok" ? "ok" : ["banned","invalid","expired","relogin_required"].includes(status) ? "bad" : ["needs_refresh","unknown"].includes(status) ? "warn" : "neutral";
