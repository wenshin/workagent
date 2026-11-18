import { Button, Modal, Spin, Typography } from '@douyinfe/semi-ui';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { api } from '../api/client';
import type { PluginComponentView, UiAction } from '../api/types';
import { PluginIcon } from './PluginVisual';
import { UiView, asText, resolveValue, type UiRuntime } from './UiView';

const { Text } = Typography;

interface Props {
  slug: string;
  version: string;
  props?: Record<string, unknown>;
  /** 组件数据请求必须落到「哪个空间」的已安装插件上，由会话所属空间决定 */
  spaceId?: string;
}

interface PluginSpec {
  slug: string;
  version: string;
  props?: Record<string, unknown>;
}

/** 解析 ```ui-block 围栏里的 JSON；坏数据不渲染，避免把错误当成组件 */
export function parseUiBlock(body: string): PluginSpec | null {
  try {
    const raw = JSON.parse(body) as Partial<PluginSpec>;
    if (!raw.slug) return null;
    return { slug: String(raw.slug), version: String(raw.version ?? ''), props: raw.props };
  } catch {
    return null;
  }
}

/** 收集必填但为空的字段名，提交前拦一道（required 不能只是画个星号） */
function missingRequired(node: unknown, state: Record<string, unknown>): string[] {
  const out: string[] = [];
  const walk = (n: unknown) => {
    if (!n || typeof n !== 'object') return;
    const item = n as Record<string, unknown>;
    if (item.required === true && item.name) {
      const value = state[asText(item.name)];
      const empty =
        value === undefined ||
        value === null ||
        value === '' ||
        (Array.isArray(value) && value.length === 0);
      if (empty) out.push(asText(item.label) || asText(item.name));
    }
    if (Array.isArray(item.children)) item.children.forEach(walk);
  };
  walk(node);
  return out;
}

/** submit 的 omit_empty：空串、空数组、null 不写进参数，模型不用猜空值含义 */
function omitEmpty(payload: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(payload)) {
    if (value === null || value === undefined || value === '') continue;
    if (Array.isArray(value) && value.length === 0) continue;
    out[key] = value;
  }
  return out;
}

/** 工具结果是文本：能直接解析成 JSON 就按结构化数据处理，否则当字符串展示 */
function parseOutput(raw: string): unknown {
  const text = raw.trim();
  if (!text) return '';
  const structured = (text.startsWith('{') && text.endsWith('}')) || (text.startsWith('[') && text.endsWith(']'));
  if (!structured) return raw;
  try {
    return JSON.parse(text);
  } catch {
    return raw;
  }
}

/**
 * 组件渲染：版本固定（同一消息永远同一版本），组件本身是一份 DSL。
 *
 * 前端只按 DSL 里的节点渲染宿主自己的组件，不加载也不执行任何组件代码。
 * 取数一律走 `POST /spaces/{id}/plugins/{slug}/data`，由后端用当前用户身份
 * 经 Invoker 调用依赖插件；写数据源必须由用户按钮手势触发。
 */
export default function UiBlock({ slug, version, props, spaceId }: Props) {
  const [view, setView] = useState<PluginComponentView | null>(null);
  const [error, setError] = useState('');
  const [state, setState] = useState<Record<string, unknown>>({});
  const [data, setData] = useState<Record<string, unknown>>({});
  const [loading, setLoading] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});

  const propsKey = useMemo(() => JSON.stringify(props ?? {}), [props]);

  useEffect(() => {
    if (!spaceId) {
      setError('当前场景没有绑定空间，组件不能渲染');
      return;
    }
    let cancelled = false;
    setView(null);
    setError('');
    const query = version ? `?version=${encodeURIComponent(version)}` : '';
    api<PluginComponentView>(
      `/spaces/${spaceId}/plugins/${encodeURIComponent(slug)}/ui${query}`,
    )
      .then((res) => {
        if (!cancelled) setView(res);
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : '组件加载失败');
      });
    return () => {
      cancelled = true;
    };
  }, [slug, version, spaceId]);

  // 同一数据源可能被连续触发（快速输入搜索词），只接受最后一次的响应。
  const seqRef = useRef<Record<string, number>>({});

  const load = useCallback(
    async (source: string, params: Record<string, unknown>, gesture = false) => {
      if (!spaceId) return;
      const seq = (seqRef.current[source] ?? 0) + 1;
      seqRef.current[source] = seq;
      const stale = () => seqRef.current[source] !== seq;
      setLoading((prev) => ({ ...prev, [source]: true }));
      setErrors((prev) => ({ ...prev, [source]: '' }));
      try {
        const res = await api<{ data: string }>(
          `/spaces/${spaceId}/plugins/${encodeURIComponent(slug)}/data`,
          { method: 'POST', body: { source, params, gesture } },
        );
        if (stale()) return;
        setData((prev) => ({ ...prev, [source]: parseOutput(res.data) }));
      } catch (e) {
        if (stale()) return;
        const message = e instanceof Error ? e.message : '数据请求失败';
        setErrors((prev) => ({ ...prev, [source]: message }));
      } finally {
        if (!stale()) setLoading((prev) => ({ ...prev, [source]: false }));
      }
    },
    [slug, spaceId],
  );

  // 初值只允许引用 props：模型可以在渲染时把用户原话里的条件预填进来。
  const initialRef = useRef<Record<string, unknown>>({});
  const prevState = useRef<Record<string, unknown> | null>(null);

  useEffect(() => {
    if (!view) return;
    const raw = (view.dsl.state ?? {}) as Record<string, unknown>;
    const initial: Record<string, unknown> = {};
    const seed = { state: initial, props: (props ?? {}) as Record<string, unknown>, data: {} };
    for (const [key, value] of Object.entries(raw)) initial[key] = resolveValue(value, seed);
    initialRef.current = initial;
    prevState.current = null;
    setState(initial);
    setData({});
    setErrors({});
    for (const [source, query] of Object.entries(view.dsl.data ?? {})) {
      if (query.auto === false) continue;
      const params = (resolveValue(query.params ?? {}, seed) ?? {}) as Record<string, unknown>;
      void load(source, params);
    }
    // props 只在内容变化时重新取数，避免父组件重渲染触发刷新
  }, [view, load, propsKey]); // eslint-disable-line react-hooks/exhaustive-deps

  const bindings = { state, props: (props ?? {}) as Record<string, unknown>, data };
  const resolveParams = (params: unknown) =>
    (resolveValue(params ?? {}, bindings) ?? {}) as Record<string, unknown>;

  // watch：声明的 state 键变化后防抖重取（搜索框、省市级联），可顺带清空下游选择。
  const watchPlan = useMemo(
    () =>
      Object.entries(view?.dsl.data ?? {})
        .filter(([, query]) => (query.watch ?? []).length > 0)
        .map(([source, query]) => ({
          source,
          watch: query.watch ?? [],
          reset: query.reset ?? [],
          params: query.params ?? {},
        })),
    [view],
  );

  useEffect(() => {
    if (!view) return;
    const before = prevState.current;
    prevState.current = state;
    if (!before) return; // 首屏由上面的 effect 负责
    const changed = new Set<string>();
    for (const key of Object.keys(state)) {
      if (JSON.stringify(state[key]) !== JSON.stringify(before[key])) changed.add(key);
    }
    if (changed.size === 0) return;
    const hits = watchPlan.filter((plan) => plan.watch.some((key) => changed.has(key)));
    if (hits.length === 0) return;
    const timer = window.setTimeout(() => {
      const resets = new Set<string>();
      for (const plan of hits) {
        for (const key of plan.reset) if (!changed.has(key)) resets.add(key);
      }
      if (resets.size > 0) {
        setState((cur) => {
          const next = { ...cur };
          for (const key of resets) next[key] = initialRef.current[key] ?? '';
          return next;
        });
      }
      for (const plan of hits) {
        void load(plan.source, resolveParams(plan.params));
      }
    }, 300);
    return () => window.clearTimeout(timer);
  }, [state, view, watchPlan]); // eslint-disable-line react-hooks/exhaustive-deps

  const reloadReads = useCallback(() => {
    if (!view) return;
    const bindings = { state, props: (props ?? {}) as Record<string, unknown>, data };
    for (const [source, query] of Object.entries(view.dsl.data ?? {})) {
      if (query.auto === false) continue;
      const params = (resolveValue(query.params ?? {}, bindings) ?? {}) as Record<string, unknown>;
      void load(source, params);
    }
  }, [view, state, data, props, load]);

  const run = (action: UiAction) => {
    if (action.type === 'reload') {
      void load(action.target, resolveParams(action.params));
      return;
    }
    if (action.type === 'message') {
      const text = asText(resolveValue(action.text, bindings));
      window.dispatchEvent(new CustomEvent('wa-ui-message', { detail: { text } }));
      return;
    }
    if (action.type === 'submit') {
      // 结构化提交：摘要给人看，```filter-params 给模型读。不自动发送，
      // 由用户确认——组件不能替用户按下发送键。
      if (!view) return;
      const missing = missingRequired(view.dsl.view, state);
      if (missing.length > 0) {
        Modal.warning({ title: '还有必填条件没填', content: missing.join('、') });
        return;
      }
      const raw = (resolveValue(action.payload, bindings) ?? {}) as Record<string, unknown>;
      const payload = action.omit_empty ? omitEmpty(raw) : raw;
      const summary = action.text ? asText(resolveValue(action.text, bindings)) : '';
      const block = '```filter-params\n' + JSON.stringify(payload) + '\n```';
      const text = summary ? `${summary}\n\n${block}` : block;
      const submit = () => window.dispatchEvent(new CustomEvent('wa-ui-message', { detail: { text } }));
      if (action.confirm) {
        Modal.confirm({
          title: '确认提交条件',
          content: action.confirm,
          okText: '填入输入框',
          cancelText: '取消',
          onOk: () => {
            submit();
          },
        });
        return;
      }
      submit();
      return;
    }
    const exec = async () => {
      await load(action.source, resolveParams(action.params), true);
      reloadReads();
    };
    if (action.confirm) {
      Modal.confirm({
        title: '确认操作',
        content: action.confirm,
        okText: '继续',
        cancelText: '取消',
        onOk: () => {
          void exec();
        },
      });
      return;
    }
    void exec();
  };

  const rt: UiRuntime = {
    state,
    props: (props ?? {}) as Record<string, unknown>,
    data,
    setState: (name, value) => setState((prev) => ({ ...prev, [name]: value })),
    run,
    loading,
    errors,
  };

  const failed = Object.entries(errors).filter(([, message]) => message);
  const busy = Object.values(loading).some(Boolean);

  if (error) {
    return (
      <div className="ui-block ui-block-error">
        <Text type="danger" size="small">
          组件 {slug}
          {version ? `@${version}` : ''} 渲染失败：{error}
        </Text>
      </div>
    );
  }
  if (!view) {
    return (
      <div className="ui-block ui-block-loading">
        <Spin size="small" />
      </div>
    );
  }

  return (
    <div className="ui-block" style={{ minHeight: view.height }}>
      <div className="ui-block-head">
        <div className="ui-block-title">
          <PluginIcon kind="component" size="sm" />
          <span className="ui-block-name">
            {view.manifest.name}
            <span className="ui-block-version">v{view.version}</span>
          </span>
        </div>
        <Button size="small" theme="borderless" disabled={busy} onClick={reloadReads}>
          刷新
        </Button>
      </div>
      {failed.length > 0 ? (
        <div className="ui-alert ui-tone-danger" role="alert">
          {failed.map(([source, message]) => (
            <span key={source}>
              {source}：{message}
            </span>
          ))}
        </div>
      ) : null}
      <UiView node={view.dsl.view} rt={rt} />
    </div>
  );
}
