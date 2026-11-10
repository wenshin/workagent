import { IconBook, IconComponent, IconLink, IconSearch, IconUserGroup, IconWrench } from '@douyinfe/semi-icons';

/**
 * 插件的统一视觉标识。
 *
 * 四类插件在列表里只靠文字标签区分太弱：技能是「照着做」、工具是「做一件事」、
 * MCP 是「连上外部服务」、组件是「在会话里渲染界面」，用户的预期完全不同。
 * 这里给每类固定一个图标 + 一种颜色，所有出现插件的地方共用，避免各处自造 badge。
 */
export type PluginKind = 'skill' | 'tool' | 'mcp' | 'component' | 'agent' | 'source';

export const KIND_LABEL: Record<PluginKind, string> = {
  skill: '技能',
  tool: '工具',
  mcp: 'MCP',
  component: '组件',
  agent: '智能体',
  source: '搜索源',
};

function KindIcon({ kind }: { kind: PluginKind }) {
  switch (kind) {
    case 'tool':
      return <IconWrench />;
    case 'mcp':
      return <IconLink />;
    case 'component':
      return <IconComponent />;
    case 'agent':
      return <IconUserGroup />;
    case 'source':
      return <IconSearch />;
    default:
      return <IconBook />;
  }
}

export function PluginIcon({
  kind,
  size = 'md',
}: {
  kind: PluginKind;
  size?: 'xs' | 'sm' | 'md' | 'lg';
}) {
  const label = KIND_LABEL[kind] ?? '插件';
  return (
    <span
      className={`plugin-icon plugin-icon-${size} plugin-icon-${kind}`}
      title={label}
      aria-label={label}
      role="img"
    >
      <KindIcon kind={kind} />
    </span>
  );
}
