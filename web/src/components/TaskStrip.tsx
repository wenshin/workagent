import { Button, Tag, Toast } from '@douyinfe/semi-ui';
import { useCallback, useEffect, useState } from 'react';

import { cancelTask, listTasks, remindTask } from '../api/client';
import type { SessionTask } from '../api/types';

/** 进行中的状态：终态任务不再占用这块位置 */
const ACTIVE: SessionTask['status'][] = ['pending', 'running', 'waiting_external'];

const STATUS_LABEL: Record<SessionTask['status'], string> = {
  pending: '待开始',
  running: '进行中',
  waiting_external: '等待回复',
  succeeded: '已完成',
  failed: '已失败',
  cancelled: '已取消',
};

/** 等待时长：3 分钟 / 2 小时 15 分 / 1 天 4 小时 */
function formatWait(since: string | null | undefined, now: number): string {
  if (!since) return '';
  const start = new Date(since).getTime();
  if (Number.isNaN(start)) return '';
  const minutes = Math.max(0, Math.floor((now - start) / 60000));
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时 ${minutes % 60} 分`;
  return `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`;
}

/**
 * 会话里的异步任务条：谁在等、等了多久、能不能催/取消。
 *
 * 平台不做超时判断（§15.12.1），所以这里只展示「卡了多久」并交回用户的判断权。
 * 每 30 秒刷新一次既是为了时钟走字，也是为了拿到别人刚回复的状态。
 */
export default function TaskStrip({ sessionId }: { sessionId: string }) {
  const [tasks, setTasks] = useState<SessionTask[]>([]);
  const [now, setNow] = useState(() => Date.now());
  const [busy, setBusy] = useState('');

  const reload = useCallback(async () => {
    try {
      const res = await listTasks(sessionId);
      setTasks(res.tasks ?? []);
    } catch {
      // 任务列表是辅助信息，拉不到就不显示，不打扰用户
    }
  }, [sessionId]);

  useEffect(() => {
    setTasks([]);
    void reload();
    const tick = window.setInterval(() => setNow(Date.now()), 30_000);
    const poll = window.setInterval(() => void reload(), 30_000);
    return () => {
      window.clearInterval(tick);
      window.clearInterval(poll);
    };
  }, [reload]);

  const active = tasks.filter((t) => ACTIVE.includes(t.status));
  if (active.length === 0) return null;

  const run = async (id: string, action: () => Promise<unknown>) => {
    setBusy(id);
    try {
      await action();
      await reload();
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '操作失败，请重试');
    } finally {
      setBusy('');
    }
  };

  return (
    <div className="task-strip">
      {active.map((t) => (
        <div key={t.id} className="task-strip-item">
          <Tag size="small" color={t.status === 'waiting_external' ? 'orange' : 'blue'}>
            {STATUS_LABEL[t.status]}
          </Tag>
          <span className="task-strip-title" title={t.title}>
            {t.title}
          </span>
          <span className="task-strip-meta">
            {t.assignee_name || '待处理'} · 已等待 {formatWait(t.waiting_since, now)}
            {t.dispatch_count > 1 ? ` · 已催办 ${t.dispatch_count - 1} 次` : ''}
          </span>
          <span className="task-strip-actions">
            <Button
              size="small"
              theme="borderless"
              disabled={busy === t.id}
              onClick={() => void run(t.id, () => remindTask(t.id))}
            >
              再次咨询
            </Button>
            <Button
              size="small"
              theme="borderless"
              type="danger"
              disabled={busy === t.id}
              onClick={() => void run(t.id, () => cancelTask(t.id))}
            >
              取消任务
            </Button>
          </span>
        </div>
      ))}
    </div>
  );
}
