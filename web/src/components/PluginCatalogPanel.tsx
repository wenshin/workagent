import { IconRefresh } from '@douyinfe/semi-icons';
import { Button, Card, Empty, Input, Modal, Select, Spin, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useMemo, useState } from 'react';

import {
  useInstallPlugin,
  usePluginCatalog,
  usePublishPlugin,
  useSpaces,
} from '../api/hooks';
import type { PluginCatalogItem, PluginCategory } from '../api/types';
import { KIND_LABEL, PluginIcon, type PluginKind } from './PluginVisual';

const { Text } = Typography;

/** 风险等级的中文名：界面上不出现 L1~L4 编号，用户看不懂那是什么意思 */
const RISK_LABEL = ['', '只读', '查询', '写入', '高危'];

const KIND_FILTERS: Array<'all' | PluginKind> = ['all', 'skill', 'tool', 'mcp', 'component', 'agent', 'source'];

export const PLUGIN_CATEGORY_LABEL: Record<PluginCategory, string> = {
  all: '全部插件',
  internal: '组织内已发布',
  pending: '组织内待发布',
  system: '平台内置',
  external: '外部（内部可见）',
};

function riskLabel(risk: number) {
  return RISK_LABEL[Math.min(Math.max(risk, 0), 4)] || '未评级';
}

function categoryOf(item: PluginCatalogItem): PluginCategory {
  if (item.pending) return 'pending';
  return item.category;
}

/**
 * 插件市场：分类由左侧二级菜单（路由）决定，这里只管列表与动作。
 *
 * 发布权在系统管理员，安装权在空间管理员；平台内置件默认对所有空间可用，
 * 没有「安装」这个动作，只展示版本与类型。
 */
export default function PluginCatalogPanel({
  defaultSpaceId,
  category = 'all',
}: {
  defaultSpaceId?: string;
  category?: PluginCategory;
} = {}) {
  const { data, isLoading, refetch, isFetching } = usePluginCatalog();
  const { data: spaces = [] } = useSpaces();
  const install = useInstallPlugin();
  const publish = usePublishPlugin();

  const items = data?.items ?? [];
  const isAdmin = data?.is_admin ?? false;
  const targets = useMemo(() => spaces.filter((s) => s.perm === 'manage'), [spaces]);
  const [spaceId, setSpaceId] = useState(defaultSpaceId ?? '');
  const [kind, setKind] = useState<'all' | PluginKind>('all');
  // 安装前先看权限：装完它能读什么、能不能写、能连哪些域名，必须先让用户看见。
  const [pendingInstall, setPendingInstall] = useState<PluginCatalogItem | null>(null);
  // 发布确认：MCP 插件绑定的应用名可能不是发布者的，必须能出示密钥
  const [pendingPublish, setPendingPublish] = useState<PluginCatalogItem | null>(null);
  const [appKey, setAppKey] = useState('');
  const target = targets.find((s) => s.id === spaceId) ?? targets[0];

  const filtered = useMemo(
    () =>
      items.filter(
        (item) =>
          (category === 'all' || categoryOf(item) === category) &&
          (kind === 'all' || item.kind === kind),
      ),
    [items, kind, category],
  );

  const doInstall = (item: PluginCatalogItem) => {
    if (!target) {
      Toast.warning('请先选择一个有管理权限的空间');
      return;
    }
    install.mutate(
      { spaceId: target.id, slug: item.slug, version: item.latest_version },
      {
        onSuccess: () => {
          setPendingInstall(null);
          Toast.success(`已安装到「${target.name}」，仅对该空间生效`);
        },
        onError: (e) => Toast.error(e instanceof Error ? e.message : '安装失败'),
      },
    );
  };

  const askInstall = (item: PluginCatalogItem) => {
    if (!target) {
      Toast.warning('请先选择一个有管理权限的空间');
      return;
    }
    setPendingInstall(item);
  };

  const doPublish = (item: PluginCatalogItem) => {
    if (!item.source_space_id) {
      Toast.error('缺少来源空间，无法发布');
      return;
    }
    // 先打开确认：MCP 插件可能绑定在别人的应用名上，发布前必须出示密钥
    setPendingPublish(item);
    setAppKey('');
  };

  const submitPublish = (item: PluginCatalogItem) => {
    if (!item.source_space_id) return;
    publish.mutate(
      { spaceId: item.source_space_id, slug: item.slug, appKey },
      {
        onSuccess: (r) => {
          setPendingPublish(null);
          Toast.success(`已发布 v${r.version}，其他空间可以安装`);
        },
        onError: (e) => Toast.error(e instanceof Error ? e.message : '发布失败'),
      },
    );
  };

  return (
    <div className="plugin-catalog">
      <div className="panel-head">
        <div className="panel-heading">
          <h2 className="panel-title">{PLUGIN_CATEGORY_LABEL[category]}</h2>
          <Text type="tertiary" size="small">
            待发布只展示元数据；发布由系统管理员负责，安装只对目标空间生效；平台内置件默认对所有空间可用
          </Text>
        </div>
        <div className="panel-actions">
          {category === 'system' ? null : (
            <Select
              value={target?.id ?? ''}
              onChange={(v) => setSpaceId(String(v))}
              style={{ width: 200 }}
              placeholder="安装到哪个空间"
              optionList={targets.map((s) => ({ value: s.id, label: s.name }))}
            />
          )}
          <Button
            theme="borderless"
            icon={<IconRefresh />}
            loading={isFetching}
            onClick={() => void refetch()}
            aria-label="刷新"
          />
        </div>
      </div>

      <div className="plugin-tabs">
        {KIND_FILTERS.map((k) => (
          <button
            key={k}
            type="button"
            className={`plugin-tab${kind === k ? ' active' : ''}`}
            onClick={() => setKind(k)}
          >
            {k === 'all' ? '全部类型' : KIND_LABEL[k]}
          </button>
        ))}
      </div>

      {isLoading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : items.length === 0 ? (
        <div className="panel-empty">
          <Empty
            title="插件市场还是空的"
            description="在空间里开发并启用插件后，会先出现在「组织内待发布」；系统管理员发布后其他空间才能安装。"
          />
        </div>
      ) : filtered.length === 0 ? (
        <div className="panel-empty">
          <Empty title="这一类还没有插件" description="换个分类或类型看看。" />
        </div>
      ) : (
        <div className="plugin-grid">
          {filtered.map((item) => {
            const pending = item.pending;
            const builtin = item.category === 'system';
            return (
              <Card key={`${item.pending ? 'p' : 'r'}-${item.slug}`} className="plugin-card" bodyStyle={{ padding: 16 }}>
                <div className="plugin-card-head">
                  <PluginIcon kind={item.kind as PluginKind} />
                  <span className="plugin-card-name">{item.name || item.slug}</span>
                  <span className="plugin-kind-label">{KIND_LABEL[item.kind as PluginKind]}</span>
                  {builtin ? (
                    <Tag size="small" color="purple">
                      平台内置
                    </Tag>
                  ) : null}
                  {item.category === 'external' ? (
                    <Tag size="small" color="cyan">
                      外部
                    </Tag>
                  ) : null}
                  {pending ? <Tag size="small" color="orange">待发布</Tag> : null}
                  <Tag size="small" color={item.risk >= 3 ? 'red' : item.risk >= 2 ? 'orange' : 'grey'}>
                    {riskLabel(item.risk)}
                  </Tag>
                  {item.visibility === 'private' ? <Tag size="small" color="grey">仅来源空间</Tag> : null}
                </div>

                <div className="plugin-card-desc">{item.description || '（没有描述）'}</div>

                <div className="plugin-card-meta">
                  <span className="plugin-slug">{item.slug}</span>
                  {item.source_space ? <span>来自「{item.source_space}」</span> : null}
                  {item.author ? <span>作者 {item.author}</span> : null}
                  <span>{pending ? '尚未发布' : `最新 v${item.latest_version || '-'}`}</span>
                </div>

                <div className="plugin-card-actions">
                  {builtin ? (
                    <Text type="tertiary" size="small">
                      全部空间默认可用，无需安装
                    </Text>
                  ) : pending ? (
                    item.can_publish ? (
                      <Button
                        size="small"
                        theme="solid"
                        type="primary"
                        loading={publish.isPending}
                        onClick={() => doPublish(item)}
                      >
                        {item.published ? '发布新版本' : '发布到组织'}
                      </Button>
                    ) : (
                      <Text type="tertiary" size="small">
                        等待系统管理员发布
                      </Text>
                    )
                  ) : item.installable ? (
                    <Button
                      size="small"
                      theme="solid"
                      type="primary"
                      loading={install.isPending}
                      disabled={!target}
                      onClick={() => askInstall(item)}
                    >
                      安装到「{target?.name ?? '未选择空间'}」
                    </Button>
                  ) : (
                    <Text type="tertiary" size="small">
                      暂不可安装
                    </Text>
                  )}
                </div>
              </Card>
            );
          })}
        </div>
      )}
      {isAdmin ? (
        <div className="panel-foot-note">
          你是系统管理员：待发布条目可以发布；安装仍需要目标空间的管理权限。
        </div>
      ) : null}

      <Modal
        title={`安装「${pendingInstall?.name || pendingInstall?.slug || ''}」`}
        visible={Boolean(pendingInstall)}
        onCancel={() => setPendingInstall(null)}
        onOk={() => {
          if (pendingInstall) doInstall(pendingInstall);
        }}
        okText={target ? `安装到「${target.name}」` : '安装'}
        cancelText="取消"
        okButtonProps={{ loading: install.isPending, disabled: !target }}
      >
        <p className="dialog-desc">
          安装后只对「{target?.name ?? '未选择空间'}」生效，插件拿到的权限不会跨空间。
        </p>
        <div className="dialog-desc" style={{ marginTop: 8 }}>
          插件声明的权限：
        </div>
        {pendingInstall?.permissions?.length ? (
          <ul className="dialog-desc" style={{ margin: '6px 0 0', paddingLeft: 18 }}>
            {pendingInstall.permissions.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        ) : (
          <p className="dialog-desc" style={{ margin: '6px 0 0' }}>
            没有可列出的权限（纯声明型插件）。
          </p>
        )}
        <p className="dialog-desc" style={{ marginTop: 10 }}>
          风险等级：{riskLabel(pendingInstall?.risk ?? 0)}。权限由清单声明、平台计算，安装后可在空间插件面板随时停用或卸载。
        </p>
      </Modal>

      <Modal
        title={`发布「${pendingPublish?.name || pendingPublish?.slug || ''}」到组织`}
        visible={Boolean(pendingPublish)}
        onCancel={() => setPendingPublish(null)}
        onOk={() => {
          if (pendingPublish) submitPublish(pendingPublish);
        }}
        okText="发布"
        cancelText="取消"
        okButtonProps={{ loading: publish.isPending }}
      >
        <p className="dialog-desc">发布后其他空间可以安装，版本内容不可再改。</p>
        {pendingPublish?.kind === 'mcp' ? (
          <div style={{ marginTop: 12 }}>
            <p className="dialog-desc">
              MCP 插件绑定在应用名（auth.app）上，一个应用名只有一把密钥。如果这个应用名不是你创建的，
              需要填写它的签名密钥确认归属——密钥在会话里通过「MCP 应用密钥」卡片获取；你自己创建的应用名可以留空。
            </p>
            <Input
              mode="password"
              value={appKey}
              onChange={setAppKey}
              placeholder="应用签名密钥（非必填，仅当应用名属于别人）"
            />
          </div>
        ) : null}
      </Modal>
    </div>
  );
}
