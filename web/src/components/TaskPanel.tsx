import { Button, Tag, Toast } from '@douyinfe/semi-ui';
import { useCallback, useEffect, useRef, useState } from 'react';

import { cancelTask, listTasks, remindTask } from '../api/client';
import type { MessagePart, SessionTask, StreamItem } from '../api/types';

/** 进行中的状态：终态任务不再需要催办 / 取消 */
const ACTIVE: SessionTask['status'][] = ['pending', 'running', 'waiting_external'];

const STATUS_LABEL: Record<SessionTask['status'], string> = {
  pending: '待开始',
  running: '进行中',
  waiting_external: '等待回复',
  succeeded: '已完成',
  failed: '已失败',
  cancelled: '已取消',
};

/** 状态色只表达状态：等待回复最需要人注意，所以单独用橙色 */
function statusColor(status: SessionTask['status']): 'blue' | 'orange' | 'green' | 'red' | 'grey' {
  if (status === 'waiting_external') return 'orange';
  if (status === 'pending' || status === 'running') return 'blue';
  if (status === 'succeeded') return 'green';
  if (status === 'failed') return 'red';
  return 'grey';
}

/** 等待时长：3 分钟 / 2 小时 15 分 / 1 天 4 小时 */
function formatWait(since: string | null | undefined, now = Date.now()): string {
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
 * 从一条工具回执里认出 task_create 派发的任务 id。
 *
 * 回执正常是 JSON；超限被截断时退回正则，task_id 排在返回体第一位。
 * 只认 task_create：task_remind 也会回 task_id，但那是催办，不该再多挂一张卡片。
 */
function createdTaskId(name: string, result?: string): string | null {
  if (name !== 'task_create' || !result) return null;
  try {
    const parsed = JSON.parse(result) as { task_id?: unknown };
    return typeof parsed.task_id === 'string' && parsed.task_id ? parsed.task_id : null;
  } catch {
    const matched = /"task_id"\s*:\s*"([^"]+)"/.exec(result);
    return matched ? matched[1] : null;
  }
}

/** 一条历史消息派发出去的任务：任务卡片挂在派发它的那一轮后面 */
export function messageTaskIds(parts?: MessagePart[]): string[] {
  const out: string[] = [];
  for (const part of parts ?? []) {
    if (part.type !== 'tool') continue;
    const id = createdTaskId(part.name, part.result);
    if (id && !out.includes(id)) out.push(id);
  }
  return out;
}

/** 流式轨迹里的任务：这一轮还没落库时，卡片先跟着实时气泡出现 */
export function streamTaskIds(items?: StreamItem[]): string[] {
  const out: string[] = [];
  for (const item of items ?? []) {
    if (item.kind !== 'tool') continue;
    const id = createdTaskId(item.tool.name, item.tool.result);
    if (id && !out.includes(id)) out.push(id);
  }
  return out;
}

/**
 * 会话派发出去的任务。
 *
 * 平台不做超时判断（§15.12.1）：等待时长只是事实，催办还是取消由用户决定。
 * 每 30 秒刷一次既让等待时长走字，也为了拿到别人刚确认的状态。
 */
export function useSessionTasks(sessionId: string, refreshKey?: string | number) {
  const [tasks, setTasks] = useState<SessionTask[]>([]);

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
    const poll = window.setInterval(() => void reload(), 30_000);
    return () => window.clearInterval(poll);
  }, [reload]);

  // 一轮结束时回执 / 派发结果都可能已经落库：跟着消息区变化补一次，不必等下一轮轮询
  const keyRef = useRef(refreshKey);
  useEffect(() => {
    if (keyRef.current === refreshKey) return;
    keyRef.current = refreshKey;
    void reload();
  }, [refreshKey, reload]);

  return { tasks, reload };
}

/** 一条任务：状态、标题、被派发人与等待时长、回执、催办/取消 */
function TaskRow({
  task,
  busy,
  onRemind,
  onCancel,
}: {
  task: SessionTask;
  busy: boolean;
  onRemind: (task: SessionTask) => void;
  onCancel: (task: SessionTask) => void;
}) {
  const running = ACTIVE.includes(task.status);
  // 元信息只说这一条任务的实情：名字查不到时不编一个「待处理」上去，
  // 已取消 / 已完成的任务更不该再显示「待处理」——那是「还没人接」的意思。
  const meta = [
    task.assignee_name?.trim() || (running ? '待处理' : ''),
    running && task.waiting_since ? `已等待 ${formatWait(task.waiting_since)}` : '',
    task.dispatch_count > 1 ? `已催办 ${task.dispatch_count - 1} 次` : '',
  ]
    .filter(Boolean)
    .join(' · ');

  return (
    <div className="task-panel-item">
      <Tag size="small" color={statusColor(task.status)}>
        {STATUS_LABEL[task.status]}
      </Tag>
      <div className="task-panel-item-main">
        <span className="task-panel-item-title" title={task.title}>
          {task.title}
        </span>
        {meta ? <span className="task-panel-item-meta">{meta}</span> : null}
        {task.result ? <span className="task-panel-item-result">{task.result}</span> : null}
      </div>
      {running ? (
        <span className="task-panel-actions">
          <Button size="small" theme="borderless" disabled={busy} onClick={() => onRemind(task)}>
            再次咨询
          </Button>
          <Button
            size="small"
            theme="borderless"
            type="danger"
            disabled={busy}
            onClick={() => onCancel(task)}
          >
            取消任务
          </Button>
        </span>
      ) : null}
    </div>
  );
}

/**
 * 任务卡片：跟在派发它的那一轮消息后面。
 *
 * 一条任务就直接摊开（标题、状态、等待时长、催办/取消都在这一行里，不再加汇总头——
 * 头一行和正文重复同一句标题是噪音）；一轮派发多条时才折成一行计数，展开后逐条看，
 * 汇总头只报条数，不重复任何单条信息。终态任务保留在原轮次里，翻回历史还能看出
 * 「当时派了谁、结果是什么」。
 */
export default function TaskPanel({
  tasks,
  onReload,
}: {
  tasks: SessionTask[];
  onReload?: () => Promise<void> | void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState('');

  if (tasks.length === 0) return null;

  const active = tasks.filter((t) => ACTIVE.includes(t.status));

  const run = async (task: SessionTask, action: () => Promise<unknown>) => {
    setBusy(task.id);
    try {
      await action();
      await onReload?.();
    } catch (e) {
      Toast.error(e instanceof Error ? e.message : '操作失败，请重试');
    } finally {
      setBusy('');
    }
  };
  const remind = (task: SessionTask) => void run(task, () => remindTask(task.id));
  const cancel = (task: SessionTask) => void run(task, () => cancelTask(task.id));

  if (tasks.length === 1) {
    return (
      <div className="task-panel">
        <TaskRow
          task={tasks[0]}
          busy={busy === tasks[0].id}
          onRemind={remind}
          onCancel={cancel}
        />
      </div>
    );
  }

  // 收起时补一句「主要是什么事」；展开后由下面的行自己说，不重复
  const headline = active[0] ?? tasks[0];
  const headlineWait =
    active.length > 0 && headline.waiting_since ? formatWait(headline.waiting_since) : '';

  return (
    <details
      className="task-panel"
      open={open}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary className="task-panel-head">
        <span className="task-panel-label">任务 {tasks.length}</span>
        <Tag size="small" color={active.length > 0 ? 'orange' : 'green'}>
          {active.length > 0 ? `${active.length} 个进行中` : '全部结束'}
        </Tag>
        {open ? null : (
          <>
            <span className="task-panel-headline" title={headline.title}>
              {headline.title}
            </span>
            {headlineWait ? (
              <span className="task-panel-headline-wait">已等待 {headlineWait}</span>
            ) : null}
          </>
        )}
      </summary>
      <div className="task-panel-body">
        {tasks.map((task) => (
          <TaskRow
            key={task.id}
            task={task}
            busy={busy === task.id}
            onRemind={remind}
            onCancel={cancel}
          />
        ))}
      </div>
    </details>
  );
}
