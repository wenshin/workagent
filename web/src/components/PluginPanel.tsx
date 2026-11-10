import { IconLock, IconRefresh, IconSetting } from '@douyinfe/semi-icons';
import {
  Button,
  Card,
  Empty,
  Modal,
  Spin,
  Tag,
  TextArea,
  Toast,
  Typography,
} from '@douyinfe/semi-ui';
import { useState } from 'react';

import {
  useDryRunPlugin,
  useInstallPlugin,
  usePlugins,
  useTogglePlugin,
  useUninstallPlugin,
} from '../api/hooks';
import type { PluginDryRunResult, PluginItem, Space } from '../api/types';
import PluginConfigDialog from './PluginConfigDialog';
import { KIND_LABEL, PluginIcon, type PluginKind } from './PluginVisual';

const { Text } = Typography;

const RISK_LABEL = ['', 'L1 免审', 'L2 只读', 'L3 写操作', 'L4 高敏'];

function riskLabel(risk: number) {
  return RISK_LABEL[Math.min(Math.max(risk, 0), 4)] || `L${risk}`;
}

interface Props {
  space: Space;
}

/**
 * 空间插件面板：只看本空间。
 *
 * - 本地插件：可编辑、必须启用才可用；启用后自动进入市场「待发布」；
 * - 已安装插件：只读副本，只能启用/停用/卸载/配置；
 * - 发布是系统管理员在市场里做的，这里不提供发布入口。
 */
export default function PluginPanel({ space }: Props) {
  const { data: items = [], isLoading, refetch, isFetching } = usePlugins(space.id);
  const dryRun = useDryRunPlugin();
  const toggle = useTogglePlugin();
  const uninstall = useUninstallPlugin();
  const install = useInstallPlugin();

  const [runTarget, setRunTarget] = useState<PluginItem | null>(null);
  const [runInput, setRunInput] = useState('{}');
  const [runResult, setRunResult] = useState<PluginDryRunResult | null>(null);
  const [pendingUninstall, setPendingUninstall] = useState<PluginItem | null>(null);
  const [configTarget, setConfigTarget] = useState<PluginItem | null>(null);

  const writable = space.perm === 'write' || space.perm === 'manage';
  const canManage = space.perm === 'manage';

  const confirmDryRun = () => {
    if (!runTarget) return;
    let parsed: unknown = {};
    try {
      parsed = runInput.trim() ? JSON.parse(runInput) : {};
    } catch {
      Toast.error('样例输入不是合法 JSON');
      return;
    }
    setRunResult(null);
    dryRun.mutate(
      { spaceId: space.id, slug: runTarget.slug, input: parsed },
      {
        onSuccess: (res) => setRunResult(res),
        onError: (e) => Toast.error(e instanceof Error ? e.message : '试运行失败'),
      },
    );
  };

  const confirmUninstall = () => {
    if (!pendingUninstall) return;
    uninstall.mutate(
      { spaceId: space.id, slug: pendingUninstall.slug },
      {
        onSuccess: () => {
          Toast.success('已卸载（审计记录保留）');
          setPendingUninstall(null);
        },
        onError: (e) => Toast.error(e instanceof Error ? e.message : '卸载失败'),
      },
    );
  };

  return (
    <div className="plugin-panel">
      <div className="plugin-panel-head">
        <div>
          <h3 className="plugin-panel-title">本空间插件</h3>
          <Text type="tertiary" size="small">
            本地插件必须启用才可用；已安装插件是只读副本，只能启停、配置和卸载。发布由系统管理员在市场操作。
          </Text>
        </div>
        <Button
          theme="borderless"
          icon={<IconRefresh />}
          loading={isFetching}
          onClick={() => void refetch()}
          aria-label="刷新插件"
        />
      </div>

      {isLoading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : items.length === 0 ? (
        <div className="plugin-empty">
          <Empty
            title="这个空间还没有插件"
            description="让 Agent 帮你做一个本地插件并启用，或去插件市场安装到本空间。"
          />
        </div>
      ) : (
        <div className="plugin-grid">
          {items.map((item) => {
            const local = item.local;
            const enabled = item.enabled;
            return (
              <Card key={item.slug} className="plugin-card" bodyStyle={{ padding: 16 }}>
                <div className="plugin-card-head">
                  <PluginIcon kind={item.kind as PluginKind} />
                  <span className="plugin-card-name">{item.name || item.slug}</span>
                  <span className="plugin-kind-label">{KIND_LABEL[item.kind as PluginKind]}</span>
                  {local ? (
                    <Tag size="small" color="blue">
                      本地
                    </Tag>
                  ) : (
                    <Tag size="small" color="green">
                      已安装
                    </Tag>
                  )}
                  {item.origin === 'system' ? (
                    <Tag size="small" color="purple">
                      平台内置
                    </Tag>
                  ) : null}
                  {!local && !item.mutable ? (
                    <Tag size="small" color="grey" prefixIcon={<IconLock />}>
                      只读
                    </Tag>
                  ) : null}
                  <Tag size="small" color={enabled ? 'green' : 'orange'}>
                    {enabled ? '已启用' : '已停用'}
                  </Tag>
                  <Tag size="small" color={item.risk >= 3 ? 'red' : item.risk >= 2 ? 'orange' : 'grey'}>
                    {riskLabel(item.risk)}
                  </Tag>
                  {local && item.published && item.update_available ? (
                    <Tag size="small" color="cyan">
                      有改动待发布
                    </Tag>
                  ) : null}
                  {local && item.published && !item.update_available ? (
                    <Tag size="small" color="grey">
                      已发布 v{item.latest_version}
                    </Tag>
                  ) : null}
                  {!local && item.update_available ? (
                    <Tag size="small" color="cyan">
                      有新版本
                    </Tag>
                  ) : null}
                </div>

                <div className="plugin-card-desc">{item.description || '（没有描述）'}</div>

                <div className="plugin-card-meta">
                  <span className="plugin-slug">{item.slug}</span>
                  {item.installed ? <span>当前 v{item.version || '-'}</span> : null}
                  {item.latest_version ? <span>最新 v{item.latest_version}</span> : null}
                  {item.tools?.length ? <span>工具 {item.tools.join('、')}</span> : null}
                </div>

                <div className="plugin-card-actions">
                  {local && writable ? (
                    <Button size="small" theme="borderless" onClick={() => setRunTarget(item)}>
                      试运行
                    </Button>
                  ) : null}

                  {canManage && item.origin !== 'system' ? (
                    <Button
                      size="small"
                      theme="borderless"
                      loading={toggle.isPending}
                      onClick={() =>
                        toggle.mutate(
                          { spaceId: space.id, slug: item.slug, enabled: !enabled },
                          {
                            onSuccess: () =>
                              Toast.success(enabled ? '已停用，本空间不再检索/调用' : '已启用，本空间可用'),
                            onError: (e) => Toast.error(e instanceof Error ? e.message : '操作失败'),
                          },
                        )
                      }
                    >
                      {enabled ? '停用' : '启用'}
                    </Button>
                  ) : null}

                  {!local && item.kind === 'mcp' ? (
                    <Button
                      size="small"
                      theme="borderless"
                      icon={<IconSetting />}
                      onClick={() => setConfigTarget(item)}
                    >
                      配置
                    </Button>
                  ) : null}

                  {!local && canManage && item.update_available ? (
                    <Button
                      size="small"
                      theme="borderless"
                      loading={install.isPending}
                      onClick={() =>
                        install.mutate(
                          { spaceId: space.id, slug: item.slug, version: item.latest_version },
                          {
                            onSuccess: () => Toast.success(`已升级到 v${item.latest_version}`),
                            onError: (e) => Toast.error(e instanceof Error ? e.message : '升级失败'),
                          },
                        )
                      }
                    >
                      升级到 v{item.latest_version}
                    </Button>
                  ) : null}

                  {!local && canManage && item.origin !== 'system' ? (
                    <Button
                      size="small"
                      theme="borderless"
                      type="danger"
                      onClick={() => setPendingUninstall(item)}
                    >
                      卸载
                    </Button>
                  ) : null}

                  {item.origin === 'system' ? (
                    <Text type="tertiary" size="small">
                      平台内置，全部空间默认可用，不能停用或卸载
                    </Text>
                  ) : null}

                  {local && !canManage ? (
                    <Text type="tertiary" size="small">
                      启用/停用需要空间管理权限
                    </Text>
                  ) : null}
                </div>
              </Card>
            );
          })}
        </div>
      )}

      <Modal
        title={`试运行 ${runTarget?.slug ?? ''}`}
        visible={Boolean(runTarget)}
        onCancel={() => {
          setRunTarget(null);
          setRunResult(null);
        }}
        onOk={confirmDryRun}
        okText="运行"
        cancelText="关闭"
        okButtonProps={{ loading: dryRun.isPending && !runResult }}
        width={600}
      >
        <p className="dialog-desc">用样例输入走一次真实调用：出网走白名单、写操作仍受限，不产生业务副作用。</p>
        <TextArea rows={4} value={runInput} onChange={setRunInput} placeholder='{"query":"示例"}' />
        {runResult ? (
          <div className="plugin-run-result">
            <div className="plugin-card-meta">
              <span>类型 {KIND_LABEL[runResult.kind as PluginKind]}</span>
              <span>{riskLabel(runResult.risk)}</span>
              <span>耗时 {runResult.duration}</span>
            </div>
            {runResult.source ? (
              <div className="plugin-run-result">
                <div className="plugin-card-meta">
                  <span>
                    {runResult.source.method ?? 'POST'} {runResult.source.url ?? ''}
                  </span>
                  <span>HTTP {runResult.source.status ?? '-'}</span>
                  <span>命中 {runResult.source.hit}</span>
                  {runResult.source.dropped > 0 ? (
                    <span>丢弃 {runResult.source.dropped}（无 URL）</span>
                  ) : null}
                </div>
                {runResult.source.top_keys?.length ? (
                  <div className="plugin-card-meta">
                    <span>响应顶层键 {runResult.source.top_keys.join(' / ')}</span>
                  </div>
                ) : null}
                {(runResult.source.samples ?? []).map((s, i) => (
                  <div key={i} className="plugin-card-meta">
                    <span>{s.title}</span>
                    <span>{s.url}</span>
                  </div>
                ))}
                {(runResult.source.warnings ?? []).map((w, i) => (
                  <div key={i} className="plugin-card-meta">
                    <span>{w}</span>
                  </div>
                ))}
              </div>
            ) : null}
            <pre className="plugin-run-pre">{runResult.output || runResult.detail || '（没有输出）'}</pre>
          </div>
        ) : null}
      </Modal>

      <PluginConfigDialog
        visible={Boolean(configTarget)}
        spaceId={space.id}
        slug={configTarget?.slug}
        name={configTarget?.name}
        onClose={() => setConfigTarget(null)}
      />

      <Modal
        title="卸载插件"
        visible={Boolean(pendingUninstall)}
        onCancel={() => setPendingUninstall(null)}
        onOk={confirmUninstall}
        okText="卸载"
        cancelText="取消"
        okButtonProps={{ type: 'danger', loading: uninstall.isPending }}
      >
        <p className="dialog-desc">
          将摘除「{pendingUninstall?.name}」的运行时并删除空间内只读副本；会话绑定或组件数据源仍在引用时会拒绝卸载。
          卸载不会删除审计记录。
        </p>
      </Modal>
    </div>
  );
}
