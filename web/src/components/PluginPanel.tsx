import { IconImage, IconKey, IconLock, IconRefresh, IconSetting } from '@douyinfe/semi-icons';
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
import { useMemo, useState } from 'react';

import {
  useAgents,
  useDryRunPlugin,
  useInstallPlugin,
  usePlugins,
  useTogglePlugin,
  useUninstallPlugin,
} from '../api/hooks';
import type { PluginDryRunResult, PluginItem, Space } from '../api/types';
import AgentAvatar from './AgentAvatar';
import AgentAvatarDialog from './AgentAvatarDialog';
import PluginConfigDialog from './PluginConfigDialog';
import { KIND_LABEL, PluginIcon, type PluginKind } from './PluginVisual';
import SecretSettingsDialog from './SecretSettingsDialog';

const { Text } = Typography;

/**
 * 风险等级的中文名。
 *
 * 界面上不出现 L1~L4 编号：编号是内部口径，用户看到「L3」只会问「所以呢」。
 * 标签只回答一件事——这个插件会做到哪一步。
 */
const RISK_LABEL = ['', '只读', '查询', '写入', '高危'];

function riskLabel(risk: number) {
  return RISK_LABEL[Math.min(Math.max(risk, 0), 4)] || '未评级';
}

/**
 * 审批档位徽标：安装方要能一眼看到这个插件会不会拦自己。
 *
 * 有权限时徽标本身就是入口——「看到了想改」和「去别处找配置按钮」不该是两件事，
 * 所以点它直接打开配置对话框（没有 onEdit 时只展示）。
 */
function ApprovalTag({
  level,
  onEdit,
}: {
  level?: PluginItem['approval_level'];
  onEdit?: () => void;
}) {
  if (!level) {
    // 技能 / 组件 / 智能体不进审批链路，没有档位可显示
    return null;
  }
  const view = {
    always: { color: 'amber' as const, text: '审批：每次确认', hint: '每次调用都先弹确认框' },
    off: { color: 'red' as const, text: '审批：不确认', hint: '直接执行，不再弹确认框' },
    declared: {
      color: 'grey' as const,
      text: '审批：按声明',
      hint: '声明了改数据、或命令命中名单时才确认',
    },
  }[level];
  if (!view) return null;
  // Semi 的 Tag 不接 title，悬停说明放在外面一层 span 上
  return (
    <span title={onEdit ? `${view.hint}；点击修改审批级别` : view.hint}>
      <Tag
        size="small"
        color={view.color}
        onClick={onEdit}
        className={onEdit ? 'plugin-tag-clickable' : undefined}
      >
        {view.text}
      </Tag>
    </span>
  );
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
  const { data, isLoading, refetch, isFetching } = usePlugins(space.id);
  const items = data?.items ?? [];
  const isAdmin = data?.is_admin ?? false;
  // 智能体头像只在 kind=agent 的卡片上出现：从会话选择器的同一份清单取，避免第二个来源
  const { data: agentResp } = useAgents(space.id);
  const agentById = useMemo(
    () => new Map((agentResp?.agents ?? []).map((a) => [a.id, a])),
    [agentResp],
  );
  const dryRun = useDryRunPlugin();
  const toggle = useTogglePlugin();
  const uninstall = useUninstallPlugin();
  const install = useInstallPlugin();

  const [runTarget, setRunTarget] = useState<PluginItem | null>(null);
  const [runInput, setRunInput] = useState('{}');
  const [runResult, setRunResult] = useState<PluginDryRunResult | null>(null);
  const [pendingUninstall, setPendingUninstall] = useState<PluginItem | null>(null);
  // 启停是共享的一个 mutation：用 slug 记住「谁在转」，否则点一张卡片所有按钮一起闪
  const [toggleSlug, setToggleSlug] = useState<string | null>(null);
  const [configTarget, setConfigTarget] = useState<PluginItem | null>(null);
  // 升级同样要确认：新版本可能多出写文件、连新域名这类权限。
  const [pendingUpgrade, setPendingUpgrade] = useState<PluginItem | null>(null);
  // 卡片上的「配置密钥」：带上缺的那条凭证，直接打开平台密钥配置弹窗
  const [secretPreset, setSecretPreset] = useState<{ scope: string; name: string } | null>(null);
  // 智能体头像：只有本地草稿能改，安装副本是只读的发布快照
  const [iconTarget, setIconTarget] = useState<PluginItem | null>(null);

  const writable = space.perm === 'write' || space.perm === 'manage';
  const canManage = space.perm === 'manage';
  // 系统空间是平台总开关：内置搜索源的启停在这里做，对全部空间生效
  const isSystemSpace = space.type === 'system';

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
            // 缺凭证的插件装上也用不了（搜索查不到 / secret.get 失败 / MCP 握手失败），
            // 卡片按「未启用」呈现，并把缺的那条凭证指出来。
            const missing = (item.secrets ?? []).filter((s) => !s.configured);
            // 只有「本该在跑却跑不起来」才算被阻塞：已停用的插件不必再提示缺凭证
            const blocked = enabled && (Boolean(item.blocked) || missing.length > 0);
            return (
              <Card key={item.slug} className="plugin-card" bodyStyle={{ padding: 16 }}>
                <div className="plugin-card-head">
                  {/* 智能体有头像就直接用头像：卡片和市场列表看到的是同一个视觉 */}
                  {item.kind === 'agent' ? (
                    <AgentAvatar
                      agent={agentById.get(item.slug) ?? { id: item.slug, name: item.name }}
                      size={28}
                    />
                  ) : (
                    <PluginIcon kind={item.kind as PluginKind} />
                  )}
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
                  {blocked ? (
                    <Tag size="small" color="orange">
                      未启用
                    </Tag>
                  ) : (
                    <Tag size="small" color={enabled ? 'green' : 'orange'}>
                      {enabled ? '已启用' : '已停用'}
                    </Tag>
                  )}
                  <Tag size="small" color={item.risk >= 3 ? 'red' : item.risk >= 2 ? 'orange' : 'grey'}>
                    {riskLabel(item.risk)}
                  </Tag>
                  <ApprovalTag
                    level={item.approval_level}
                    onEdit={
                      !local && item.configurable && canManage
                        ? () => setConfigTarget(item)
                        : undefined
                    }
                  />
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

                {blocked ? (
                  <div className="plugin-card-meta">
                    <Text type="warning" size="small">
                      {item.blocked_reason ||
                        `缺少凭证 ${missing.map((s) => `${s.scope}/${s.name}`).join('、')}，配置后才会生效`}
                    </Text>
                  </div>
                ) : null}

                <div className="plugin-card-meta">
                  <span className="plugin-slug">{item.slug}</span>
                  {item.installed ? <span>当前 v{item.version || '-'}</span> : null}
                  {item.latest_version ? <span>最新 v{item.latest_version}</span> : null}
                  {item.tools?.length ? <span>工具 {item.tools.join('、')}</span> : null}
                </div>

                <div className="plugin-card-actions">
                  {missing.length > 0 ? (
                    <Button
                      size="small"
                      theme="solid"
                      type="primary"
                      icon={<IconKey />}
                      onClick={() => {
                        if (!isAdmin) {
                          Toast.warning('平台凭证只有系统管理员能录入，把凭证名转给他即可');
                          return;
                        }
                        // 缺的第一条就是最该先补的那条（多条时按清单顺序）
                        setSecretPreset({ scope: missing[0].scope, name: missing[0].name });
                      }}
                    >
                      配置密钥
                    </Button>
                  ) : null}

                  {local && writable ? (
                    <Button size="small" theme="borderless" onClick={() => setRunTarget(item)}>
                      试运行
                    </Button>
                  ) : null}

                  {/* 头像只对智能体有意义；安装副本只读，入口只给草稿 */}
                  {local && writable && item.kind === 'agent' ? (
                    <Button
                      size="small"
                      theme="borderless"
                      icon={<IconImage />}
                      onClick={() => setIconTarget(item)}
                    >
                      头像
                    </Button>
                  ) : null}

                  {/* 停用只在空间插件页做：普通空间只能关自己的安装，系统空间是平台总开关。
                      内置件只有一份安装（系统空间），在这里停用/启用对所有空间立即生效。 */}
                  {canManage && (item.origin !== 'system' || isSystemSpace) ? (
                    <Button
                      size="small"
                      theme="borderless"
                      loading={toggleSlug === item.slug}
                      onClick={() => {
                        setToggleSlug(item.slug);
                        toggle.mutate(
                          { spaceId: space.id, slug: item.slug, enabled: !enabled },
                          {
                            onSuccess: () =>
                              Toast.success(
                                item.origin === 'system'
                                  ? enabled
                                    ? '已停用，全部空间立即不再加载它'
                                    : '已启用，全部空间立即恢复'
                                  : enabled
                                    ? '已停用，本空间不再检索/调用'
                                    : '已启用，本空间可用',
                              ),
                            onError: (e) => Toast.error(e instanceof Error ? e.message : '操作失败'),
                            onSettled: () => setToggleSlug(null),
                          },
                        );
                      }}
                    >
                      {enabled ? '停用' : '启用'}
                    </Button>
                  ) : null}

                  {!local && item.configurable && (item.origin !== 'system' || isSystemSpace) ? (
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
                      onClick={() => setPendingUpgrade(item)}
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
                      {isSystemSpace
                        ? '平台内置，全部空间默认可用；停用对全部空间生效，不可卸载'
                        : '平台内置，全部空间默认可用，不能停用或卸载'}
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

      <AgentAvatarDialog
        visible={Boolean(iconTarget)}
        spaceId={space.id}
        slug={iconTarget?.slug}
        name={iconTarget?.name || iconTarget?.slug}
        currentIcon={iconTarget ? agentById.get(iconTarget.slug)?.icon_url : undefined}
        onClose={() => setIconTarget(null)}
      />

      {/* 复用平台密钥配置弹窗：凭证是平台级的，空间内只提供入口与预填 */}
      <SecretSettingsDialog
        visible={Boolean(secretPreset)}
        preset={secretPreset}
        onClose={() => setSecretPreset(null)}
      />

      <Modal
        title={`升级到 v${pendingUpgrade?.latest_version ?? ''}`}
        visible={Boolean(pendingUpgrade)}
        onCancel={() => setPendingUpgrade(null)}
        onOk={() => {
          if (!pendingUpgrade) return;
          install.mutate(
            { spaceId: space.id, slug: pendingUpgrade.slug, version: pendingUpgrade.latest_version },
            {
              onSuccess: () => {
                Toast.success(`已升级到 v${pendingUpgrade.latest_version}`);
                setPendingUpgrade(null);
              },
              onError: (e) => Toast.error(e instanceof Error ? e.message : '升级失败'),
            },
          );
        }}
        okText="确认升级"
        cancelText="取消"
        okButtonProps={{ loading: install.isPending }}
      >
        <p className="dialog-desc">
          升级会替换本空间的只读副本并重启插件进程；正在进行的调用会失败，稍后重试即可。
        </p>
        <p className="dialog-desc" style={{ marginTop: 8 }}>
          新版本声明的权限：
        </p>
        {pendingUpgrade?.latest_permissions?.length ? (
          <ul className="dialog-desc" style={{ margin: '6px 0 0', paddingLeft: 18 }}>
            {pendingUpgrade.latest_permissions.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        ) : (
          <p className="dialog-desc" style={{ margin: '6px 0 0' }}>
            没有可列出的权限（纯声明型插件）。
          </p>
        )}
        <p className="dialog-desc" style={{ marginTop: 10 }}>
          当前版本权限：{pendingUpgrade?.permissions?.join('；') || '无'}
        </p>
      </Modal>

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
