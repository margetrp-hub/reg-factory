import { TaskGroupView } from "@/components/task-group-view";

// 静态导出：分类 slug 在这里显式声明（client 组件的常量不能直接用于 generateStaticParams）
const GROUPS = ["register", "authorize", "data", "helper"];

export function generateStaticParams() {
  return GROUPS.map((group) => ({ group }));
}

export default async function TaskGroupPage({ params }: { params: Promise<{ group: string }> }) {
  const { group } = await params;
  return <TaskGroupView group={group} />;
}
