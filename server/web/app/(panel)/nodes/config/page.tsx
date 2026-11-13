import { ConfigEditor } from "@/components/config-editor";

export default function NodeConfigPage() {
  return (
    <div className="space-y-4">
      <div className="page-head">
        <div>
          <h1 className="page-title">节点配置</h1>
        </div>
      </div>
      <ConfigEditor scope="node" />
    </div>
  );
}
