import { ConfigEditor } from "@/components/config-editor";

export default function NodeConfigPage() {
  return (
    <div className="space-y-4">
      <div className="page-head">
        <div>
          <h1 className="page-title">节点配置</h1>
          <p className="page-desc">节点参数使用统一配置文件与保存接口，当前运行状态在节点总览核对。</p>
        </div>
      </div>
      <ConfigEditor scope="node" />
    </div>
  );
}
