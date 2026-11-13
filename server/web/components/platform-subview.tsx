"use client";

import { AccountWorkspace } from "@/components/account-workspace";
import { TaskGroupView } from "@/components/task-group-view";

export function PlatformSubview({slug,tab}: {slug:string;tab:"tasks"|"accounts"}) {
  return tab==="tasks" ? <TaskGroupView key={slug} platform={slug}/> : <AccountWorkspace key={slug} platformScope={slug}/>;
}
