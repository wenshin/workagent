import { Select as SemiSelect } from '@douyinfe/semi-ui';
import { useEffect } from 'react';
import type { ReactNode } from 'react';

import type { UiAction, UiCond, UiNode } from '../api/types';

/**
 * 组件 DSL 渲染器。
 *
 * 组件只有「声明的节点 + 数据绑定」，没有 JS、没有 HTML、没有样式注入：
 * 所有节点都是宿主自己的 React 组件，文本走 React 转义。
 * 这也是不用 iframe / Shadow DOM 的前提——没有可执行内容需要隔离。
 */
export interface UiBindings {
  state: Record<string, unknown>;
  props: Record<string, unknown>;
  data: Record<string, unknown>;
}

export interface UiRuntime extends UiBindings {
  setState: (name: string, value: unknown) => void;
  run: (action: UiAction) => void;
  loading: Record<string, boolean>;
  errors: Record<string, string>;
}

const PATH_SPLIT = /\./;
const TEMPLATE = /\{\{\s*([^{}]+?)\s*\}\}/g;

export function lookupPath(path: string, ctx: UiBindings): unknown {
  const parts = path.split(PATH_SPLIT);
  let cur: unknown;
  switch (parts[0]) {
    case 'state':
      cur = ctx.state;
      break;
    case 'props':
      cur = ctx.props;
      break;
    case 'data':
      cur = ctx.data;
      break;
    default:
      return undefined;
  }
  for (const part of parts.slice(1)) {
    if (cur === null || cur === undefined) return undefined;
    if (typeof cur !== 'object') return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
}

/** 整个字符串就是一个 `{{path}}` 时保留原始类型（数组/数字/对象），否则做字符串插值 */
export function resolveValue(value: unknown, ctx: UiBindings): unknown {
  if (typeof value === 'string') {
    const whole = /^\{\{\s*([^{}]+?)\s*\}\}$/.exec(value.trim());
    if (whole) return lookupPath(whole[1].trim(), ctx);
    return value.replace(TEMPLATE, (_, path: string) => asText(lookupPath(path.trim(), ctx)));
  }
  if (Array.isArray(value)) return value.map((item) => resolveValue(item, ctx));
  if (value && typeof value === 'object') {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = resolveValue(v, ctx);
    }
    return out;
  }
  return value;
}

export function asText(value: unknown): string {
  if (value === null || value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export function checkWhen(cond: UiCond | undefined, ctx: UiBindings): boolean {
  if (!cond) return true;
  const actual = lookupPath(cond.path, ctx);
  switch (cond.op) {
    case 'exists':
      return actual !== undefined && actual !== null && actual !== '';
    case 'truthy':
      return Boolean(actual);
    case 'eq':
      return looseEqual(actual, cond.value);
    case 'ne':
      return !looseEqual(actual, cond.value);
    case 'gt':
      return Number(actual) > Number(cond.value);
    case 'lt':
      return Number(actual) < Number(cond.value);
    case 'in':
      return Array.isArray(cond.value) && cond.value.some((v) => looseEqual(actual, v));
    case 'empty':
      return (
        actual === undefined ||
        actual === null ||
        actual === '' ||
        (Array.isArray(actual) && actual.length === 0)
      );
    default:
      return true;
  }
}

function looseEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a === null || a === undefined || b === null || b === undefined) return false;
  return String(a) === String(b);
}

function text(node: UiNode, key: string, rt: UiRuntime): string {
  return asText(resolveValue(node[key], rt));
}

function tone(node: UiNode, fallback = 'default'): string {
  const t = asText(node.tone);
  return t || fallback;
}

function children(node: UiNode, rt: UiRuntime): ReactNode {
  const kids = Array.isArray(node.children) ? (node.children as UiNode[]) : [];
  return kids.map((kid, i) => <UiView key={i} node={kid} rt={rt} />);
}

/** 渲染一个节点；`when` 不成立时整块不出现。 */
export function UiView({ node, rt }: { node: UiNode; rt: UiRuntime }) {
  if (!checkWhen(node.when, rt)) return null;
  return renderNode(node, rt);
}

function renderNode(node: UiNode, rt: UiRuntime): ReactNode {
  const gap = asText(node.gap) || 'md';
  switch (node.type) {
    case 'Stack':
      return <div className={`ui-stack ui-gap-${gap}`}>{children(node, rt)}</div>;
    case 'Row':
      return (
        <div className={`ui-row ui-gap-${gap} ui-align-${asText(node.align) || 'start'}`}>
          {children(node, rt)}
        </div>
      );
    case 'Grid': {
      const cols = Math.min(Math.max(Number(node.columns) || 2, 1), 4);
      return (
        <div className={`ui-grid ui-gap-${gap}`} style={{ gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` }}>
          {children(node, rt)}
        </div>
      );
    }
    case 'Card': {
      const title = text(node, 'title', rt);
      return (
        <section className="ui-card">
          {title ? <header className="ui-card-title">{title}</header> : null}
          <div className={`ui-stack ui-gap-${gap}`}>{children(node, rt)}</div>
        </section>
      );
    }
    case 'Divider':
      return <hr className="ui-divider" />;
    case 'Heading': {
      const level = Math.min(Math.max(Number(node.level) || 3, 1), 4);
      return <div className={`ui-heading ui-heading-${level}`}>{text(node, 'text', rt)}</div>;
    }
    case 'Text':
      return (
        <p className={`ui-text ui-size-${asText(node.size) || 'md'} ui-tone-${tone(node, 'default')}`}>
          {text(node, 'text', rt)}
        </p>
      );
    case 'Badge':
      return <span className={`ui-badge ui-tone-${tone(node)}`}>{text(node, 'text', rt)}</span>;
    case 'Alert': {
      const title = text(node, 'title', rt);
      return (
        <div className={`ui-alert ui-tone-${tone(node, 'info')}`} role="alert">
          {title ? <strong>{title}</strong> : null}
          <span>{text(node, 'text', rt)}</span>
        </div>
      );
    }
    case 'Stat': {
      const delta = text(node, 'delta', rt);
      return (
        <div className="ui-stat">
          <span className="ui-stat-label">{text(node, 'label', rt)}</span>
          <span className={`ui-stat-value ui-tone-${tone(node, 'default')}`}>{text(node, 'value', rt)}</span>
          {delta ? <span className="ui-stat-delta">{delta}</span> : null}
        </div>
      );
    }
    case 'KeyValue': {
      const items = Array.isArray(node.items) ? (node.items as Array<Record<string, unknown>>) : [];
      return (
        <dl className="ui-kv">
          {items.map((item, i) => (
            <div className="ui-kv-row" key={i}>
              <dt>{asText(resolveValue(item.label, rt))}</dt>
              <dd>{asText(resolveValue(item.value, rt))}</dd>
            </div>
          ))}
        </dl>
      );
    }
    case 'List':
      return renderList(node, rt);
    case 'Table':
      return renderTable(node, rt);
    case 'Progress': {
      const value = Number(resolveValue(node.value, rt)) || 0;
      const max = Number(resolveValue(node.max, rt)) || 100;
      const pct = Math.min(Math.max(max > 0 ? (value / max) * 100 : 0, 0), 100);
      const label = text(node, 'label', rt);
      return (
        <div className="ui-progress">
          <div className="ui-progress-head">
            <span>{label}</span>
            <span>{`${Math.round(pct)}%`}</span>
          </div>
          <div className="ui-progress-track">
            <div className={`ui-progress-bar ui-tone-${tone(node, 'info')}`} style={{ width: `${pct}%` }} />
          </div>
        </div>
      );
    }
    case 'Button': {
      const label = text(node, 'text', rt);
      const busy = Object.values(rt.loading).some(Boolean);
      const action = node.action as UiAction | undefined;
      return (
        <button
          type="button"
          className={`ui-button ui-tone-${tone(node, 'default')}`}
          disabled={!action || busy}
          onClick={() => action && rt.run(action)}
        >
          {label}
        </button>
      );
    }
    case 'Input': {
      const name = asText(node.name);
      const label = text(node, 'label', rt);
      const kind = asText(node.input_type) || 'text';
      return (
        <label className="ui-field">
          {label ? (
            <span className="ui-field-label">
              {label}
              {node.required === true ? <span className="ui-field-required">*</span> : null}
            </span>
          ) : null}
          <input
            className="ui-input"
            type={kind === 'number' ? 'number' : kind === 'date' ? 'date' : 'text'}
            value={asText(rt.state[name])}
            placeholder={text(node, 'placeholder', rt)}
            onChange={(e) => rt.setState(name, e.target.value)}
          />
        </label>
      );
    }
    case 'Select':
      return <SelectNode node={node} rt={rt} />;
    case 'Tabs': {
      const name = asText(node.name);
      const active = asText(rt.state[name]);
      const options = Array.isArray(node.options) ? (node.options as Array<Record<string, unknown>>) : [];
      return (
        <div className="ui-tabs" role="tablist">
          {options.map((opt, i) => {
            const value = asText(opt.value);
            return (
              <button
                key={i}
                type="button"
                role="tab"
                aria-selected={active === value}
                className={`ui-tab${active === value ? ' is-active' : ''}`}
                onClick={() => rt.setState(name, value)}
              >
                {asText(opt.label) || value}
              </button>
            );
          })}
        </div>
      );
    }
    default:
      // 后端已按白名单校验，这里只兜底提示，不执行任何内容
      return <div className="ui-block-error">不支持的节点类型：{node.type}</div>;
  }
}

function renderList(node: UiNode, rt: UiRuntime): ReactNode {
  const rows = resolveValue(node.source, rt);
  const empty = text(node, 'empty', rt) || '暂无数据';
  if (!Array.isArray(rows) || rows.length === 0) return <div className="ui-empty">{empty}</div>;
  return (
    <ul className="ui-list">
      {rows.map((row, i) => {
        if (row === null || typeof row !== 'object') {
          return (
            <li className="ui-list-item" key={i}>
              <span className="ui-list-title">{asText(row)}</span>
            </li>
          );
        }
        const item = row as Record<string, unknown>;
        const badge = asText(item.badge);
        return (
          <li className="ui-list-item" key={i}>
            <div className="ui-list-main">
              <span className="ui-list-title">{asText(item.title ?? item.name ?? item.label)}</span>
              {item.description ? <span className="ui-list-desc">{asText(item.description)}</span> : null}
            </div>
            {badge ? <span className={`ui-badge ui-tone-${asText(item.tone) || 'default'}`}>{badge}</span> : null}
          </li>
        );
      })}
    </ul>
  );
}

interface UiOptionFrom {
  source: string;
  items?: string;
  label: string;
  value: string;
  /** 下拉自带搜索框写入的 state 键，配合 data.watch 做远程搜索 */
  search?: string;
}

interface UiOption {
  label: string;
  value: string;
}

/** 按 `a.b.c` 取值；数组下标写成数字段即可（rows.0.name）。 */
function readField(record: unknown, path: string): unknown {
  let cur = record;
  for (const part of path.split('.')) {
    if (cur === null || cur === undefined || typeof cur !== 'object') return undefined;
    cur = (cur as Record<string, unknown>)[part];
  }
  return cur;
}

function asList(value: unknown): string[] {
  if (Array.isArray(value)) return value.map((v) => asText(v));
  const single = asText(value);
  return single ? [single] : [];
}

function literalOptions(node: UiNode): UiOption[] {
  const raw = Array.isArray(node.options) ? (node.options as Array<Record<string, unknown>>) : [];
  return raw.map((opt) => ({
    label: asText(opt.label) || asText(opt.value),
    value: asText(opt.value),
  }));
}

/** 候选来自数据源：items 是数组所在路径，label / value 是每一项里的字段名。 */
function dynamicOptions(rt: UiRuntime, from: UiOptionFrom): UiOption[] {
  const payload = rt.data[from.source];
  const container = from.items ? readField(payload, from.items) : Array.isArray(payload) ? payload : readField(payload, 'items');
  if (!Array.isArray(container)) return [];
  const out: UiOption[] = [];
  for (const item of container) {
    const value = asText(readField(item, from.value));
    if (!value) continue;
    out.push({ value, label: asText(readField(item, from.label)) || value });
  }
  return out;
}

/**
 * Select 的候选有两种来源：字面量 options，或 data 里某个只读数据源的 items。
 * 后者不自己发请求——取数只走 UiBlock 的 data 管线，这样刷新、错误、loading 只有一份。
 */
function SelectNode({ node, rt }: { node: UiNode; rt: UiRuntime }) {
  const name = asText(node.name);
  const label = text(node, 'label', rt);
  const multiple = node.multiple === true;
  const max = Number(node.max) || 0;
  const from = node.options_from as UiOptionFrom | undefined;
  const options = from ? dynamicOptions(rt, from) : literalOptions(node);
  const current = multiple ? asList(rt.state[name]) : asText(rt.state[name]);
  const labelName = asText(node.label_name);
  const optionsKey = JSON.stringify(options);
  const valueKey = JSON.stringify(current);

  // label_name 存展示名：模型提交时要 id 拼 SQL、要名称复述，反查只在这里做一次。
  useEffect(() => {
    if (!labelName) return;
    const picked = multiple ? (current as string[]) : current ? [current as string] : [];
    const labels = picked.map((v) => options.find((o) => o.value === v)?.label ?? v);
    const next = multiple ? labels : labels[0] ?? '';
    if (JSON.stringify(next) !== JSON.stringify(rt.state[labelName])) rt.setState(labelName, next);
  }, [labelName, multiple, optionsKey, valueKey]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <label className="ui-field">
      {label ? (
        <span className="ui-field-label">
          {label}
          {node.required === true ? <span className="ui-field-required">*</span> : null}
        </span>
      ) : null}
      <SemiSelect
        style={{ width: '100%' }}
        value={multiple ? current : current || undefined}
        optionList={options.map((o) => ({ label: o.label, value: o.value }))}
        multiple={multiple}
        filter
        showClear
        maxTagCount={6}
        placeholder={text(node, 'placeholder', rt) || '请选择'}
        emptyContent={<div className="ui-empty">{text(node, 'empty', rt) || '暂无可选项'}</div>}
        onSearch={(keyword: string) => {
          if (from?.search) rt.setState(from.search, keyword);
        }}
        onChange={(value: unknown) => {
          if (multiple) {
            let picked = Array.isArray(value) ? value.map((v) => asText(v)) : [];
            if (max > 0 && picked.length > max) picked = picked.slice(0, max);
            rt.setState(name, picked);
            return;
          }
          rt.setState(name, asText(value));
        }}
      />
    </label>
  );
}

function renderTable(node: UiNode, rt: UiRuntime): ReactNode {
  const rows = resolveValue(node.source, rt);
  const empty = text(node, 'empty', rt) || '暂无数据';
  const columns = Array.isArray(node.columns) ? (node.columns as Array<Record<string, unknown>>) : [];
  if (!Array.isArray(rows) || rows.length === 0) return <div className="ui-empty">{empty}</div>;
  return (
    <div className="ui-table-wrap">
      <table className="ui-table">
        <thead>
          <tr>
            {columns.map((col, i) => (
              <th key={i} style={col.align ? { textAlign: col.align as 'left' | 'center' | 'right' } : undefined}>
                {asText(col.label)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, ri) => {
            const record = (row && typeof row === 'object' ? row : {}) as Record<string, unknown>;
            return (
              <tr key={ri}>
                {columns.map((col, ci) => (
                  <td key={ci} style={col.align ? { textAlign: col.align as 'left' | 'center' | 'right' } : undefined}>
                    {asText(record[asText(col.key)])}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
