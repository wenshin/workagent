import { IconComment } from '@douyinfe/semi-icons';
import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';

import type { ChatSession } from '../api/types';

/**
 * 头像兜底色的色板（只定身份色本身）。
 *
 * 底色与字色都由样式表从这个色算：底色混白（浅底才看得见），字色混黑
 * （品牌色本身在浅底上只有 3:1 左右，读不清）。本应用固定暗色主题，
 * 任何「暗底」都会和侧栏糊在一起。
 */
const TINTS = [
  'var(--semi-color-primary)',
  'var(--semi-color-success)',
  'var(--semi-color-warning)',
  'var(--semi-color-danger)',
];

function tintOf(seed: string) {
  let h = 0;
  for (let i = 0; i < seed.length; i += 1) {
    h = (h * 31 + seed.charCodeAt(i)) % 9973;
  }
  return TINTS[h % TINTS.length];
}

export interface AgentLike {
  id: string;
  name?: string;
  icon_url?: string;
}

interface Props {
  /** 为空表示普通会话：没有智能体可展示 */
  agent?: AgentLike;
  /** 图标边长（px） */
  size?: number;
  className?: string;
}

/**
 * 智能体头像。
 *
 * 三种形态，每种都能回答「这是谁」：
 * - 上传过头像：直接显示图片；
 * - 智能体没上传头像：首字徽标 + 按 slug 固定的颜色，和普通会话一眼分得开；
 * - 没有智能体：普通会话的评论图标。
 *
 * 图片 404（清单声明了但文件被删）时回落到徽标，不给用户留一个破图。
 */
export default function AgentAvatar({ agent, size = 16, className }: Props) {
  const [failed, setFailed] = useState(false);
  const url = agent?.icon_url;

  // 换智能体要清掉失败态：否则上一个的 404 会连累下一个
  useEffect(() => setFailed(false), [url]);

  if (agent && url && !failed) {
    return (
      <img
        className={`agent-avatar ${className ?? ''}`}
        src={url}
        alt=""
        width={size}
        height={size}
        onError={() => setFailed(true)}
        aria-hidden
      />
    );
  }

  if (agent) {
    const label = (agent.name || agent.id).trim();
    // 身份色以自定义属性传下去：字色与底色都要在它上面做混色，内联 color 会挡住
    const style = {
      width: size,
      height: size,
      fontSize: Math.max(10, Math.round(size * 0.58)),
      ['--agent-tint' as string]: tintOf(agent.id),
    } as CSSProperties;
    return (
      <span
        className={`agent-avatar agent-avatar-fallback ${className ?? ''}`}
        style={style}
        title={label}
        aria-hidden
      >
        {label.slice(0, 1).toUpperCase()}
      </span>
    );
  }

  return <IconComment className={className} />;
}

/**
 * 会话图标：智能体头像 + 右下角一枚会话角标。
 *
 * 只用头像，列表里会读成「一条智能体条目」而不是「一次对话」；只用评论图标，
 * 又看不出是谁在回答。角标是两者的最小并集：头像回答「谁」，角标回答「这是会话」。
 * 未绑定智能体（或默认 base）时没有要表达的身份，保持原来的评论图标。
 */
export function SessionAgentIcon({
  session,
  size = 16,
}: {
  session: Pick<ChatSession, 'agent_id' | 'agent_name' | 'agent_icon_url'>;
  size?: number;
}) {
  const bound = Boolean(session.agent_id) && session.agent_id !== 'base';
  if (!bound && !session.agent_icon_url) {
    // 普通会话的评论图标也要跟着一起缩：SVG 字形占满整格，和同尺寸的方形头像
    // 摆在一起会显得更粗，所以再收 2px，视觉上才和头像一样大。
    return (
      <span className="session-agent-icon" style={{ width: size, height: size }}>
        <IconComment style={{ fontSize: Math.max(10, size - 2) }} />
      </span>
    );
  }
  // 角标只是「这是会话」的提示：16px 头像配 10px，再小就看不出是个会话
  const badge = Math.max(10, Math.round(size * 0.62));
  return (
    <span className="session-agent-icon" style={{ width: size, height: size }}>
      <AgentAvatar
        agent={{
          id: session.agent_id ?? 'base',
          name: session.agent_name,
          icon_url: session.agent_icon_url,
        }}
        size={size}
      />
      {/* 图标尺寸由样式表按角标百分比定（见 .session-agent-icon-badge）：只靠
          font-size 传不进 Semi 的 svg，字形会顶满甚至溢出这枚小圆 */}
      <span
        className="session-agent-icon-badge"
        style={{ width: badge, height: badge }}
        aria-hidden
      >
        <IconComment />
      </span>
    </span>
  );
}
