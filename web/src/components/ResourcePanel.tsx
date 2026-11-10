import { IconArrowLeft, IconDelete, IconHistory, IconPlus, IconRefresh } from '@douyinfe/semi-icons';
import { Button, Card, Empty, Modal, Select, Spin, Tag, Toast, Typography } from '@douyinfe/semi-ui';
import { useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';

import {
  useDeleteResource,
  useDeleteResourceVersion,
  useResourceDetail,
  useResources,
  useSetResourceStatus,
} from '../api/hooks';
import type { PublishedResource } from '../api/types';
import { PluginIcon } from './PluginVisual';
import PublishResourceDialog from './PublishResourceDialog';

const { Text } = Typography;

const KIND_LABEL: Record<PublishedResource['kind'], string> = {
  skill: '技能',
  tool: '工具',
  mcp: 'MCP',
  component: '组件',
  db: '数据库',
};

type Filter = 'all' | 'enabled' | 'disabled';

function formatTime(iso: string) {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

/**
 * 发布记录：空间文件的历史发布快照。
 *
 * 它不再是「资源管理」里的一个并列 Tab——快照是空间文件的发布结果，
 * 入口放在空间页里；平台级的插件安装则归到「插件市场」。
 * 只展示名称、版本与描述，不展示原始 MCP / tool / DB 配置。
 */
export default function ResourcePanel() {
  const navigate = useNavigate();
  const { data: resources, isLoading, refetch, isFetching } = useResources();
  const setStatus = useSetResourceStatus();
  const remove = useDeleteResource();
  const removeVersion = useDeleteResourceVersion();

  const [publishOpen, setPublishOpen] = useState(false);
  const [detailId, setDetailId] = useState<string | undefined>();
  const [statusFilter, setStatusFilter] = useState<Filter>('all');
  const [kindFilter, setKindFilter] = useState<string>('all');
  const [scopeFilter, setScopeFilter] = useState<string>('all');
  const [pendingDelete, setPendingDelete] = useState<PublishedResource | undefined>();
  const [pendingVersion, setPendingVersion] = useState<{ resource: PublishedResource; version: string } | undefined>();

  const detail = useResourceDetail(detailId);

  const list = useMemo(() => {
    return (resources ?? []).filter((r) => {
      if (statusFilter !== 'all' && r.status !== statusFilter) return false;
      if (kindFilter !== 'all' && r.kind !== kindFilter) return false;
      if (scopeFilter !== 'all' && r.visibility !== scopeFilter) return false;
      return true;
    });
  }, [resources, statusFilter, kindFilter, scopeFilter]);

  const toggleStatus = (r: PublishedResource) => {
    const next = r.status === 'enabled' ? 'disabled' : 'enabled';
    setStatus.mutate(
      { id: r.id, status: next },
      {
        onSuccess: () => Toast.success(next === 'enabled' ? '已启用' : '已停用，Agent 不再检索到该资源'),
        onError: (e) => Toast.error(e instanceof Error ? e.message : '操作失败'),
      },
    );
  };

  const confirmDelete = () => {
    if (!pendingDelete) return;
    remove.mutate(pendingDelete.id, {
      onSuccess: () => {
        Toast.success('已删除资源及其全部版本');
        setPendingDelete(undefined);
        if (detailId === pendingDelete.id) setDetailId(undefined);
      },
      onError: (e) => Toast.error(e instanceof Error ? e.message : '删除失败'),
    });
  };

  const confirmDeleteVersion = () => {
    if (!pendingVersion) return;
    removeVersion.mutate(
      { id: pendingVersion.resource.id, version: pendingVersion.version },
      {
        onSuccess: () => {
          Toast.success(`已删除版本 ${pendingVersion.version}`);
          setPendingVersion(undefined);
        },
        onError: (e) => Toast.error(e instanceof Error ? e.message : '删除版本失败'),
      },
    );
  };

  return (
    <div className="resource-panel">
      <div className="panel-head">
        <div className="panel-heading">
          <h2 className="panel-title">发布记录</h2>
          <Text type="tertiary" size="small">
            空间文件的历史发布快照：快照内容只读、按版本保存；Agent 按关键词检索，命中同名资源会先请你确认。停用后不再被检索。
          </Text>
        </div>
        <div className="panel-actions">
          <Button
            theme="borderless"
            icon={<IconArrowLeft />}
            onClick={() => navigate('/')}
          >
            返回工作空间
          </Button>
          <Button
            theme="borderless"
            icon={<IconRefresh />}
            loading={isFetching}
            onClick={() => void refetch()}
            aria-label="刷新"
          />
          <Button theme="solid" type="primary" icon={<IconPlus />} onClick={() => setPublishOpen(true)}>
            发布资源
          </Button>
        </div>
      </div>

      <div className="resource-filters">
        <Select
          value={kindFilter}
          onChange={(v) => setKindFilter(String(v))}
          style={{ width: 140 }}
          optionList={[
            { value: 'all', label: '全部类型' },
            { value: 'skill', label: '技能' },
            { value: 'tool', label: '工具' },
            { value: 'mcp', label: 'MCP' },
          ]}
        />
        <Select
          value={scopeFilter}
          onChange={(v) => setScopeFilter(String(v))}
          style={{ width: 140 }}
          optionList={[
            { value: 'all', label: '全部范围' },
            { value: 'public', label: '全部可见' },
            { value: 'private', label: '仅自己可见' },
          ]}
        />
        <Select
          value={statusFilter}
          onChange={(v) => setStatusFilter(v as Filter)}
          style={{ width: 140 }}
          optionList={[
            { value: 'all', label: '全部状态' },
            { value: 'enabled', label: '已启用' },
            { value: 'disabled', label: '已停用' },
          ]}
        />
      </div>

      {isLoading ? (
        <div className="panel-empty">
          <Spin />
        </div>
      ) : list.length === 0 ? (
        <div className="panel-empty">
          <Empty
            title="还没有发布记录"
            description="在空间里写好技能后，点右上角「发布资源」，指定版本与用户范围。"
          />
        </div>
      ) : (
        <div className="resource-grid">
          {list.map((r) => (
            <Card key={r.id} className="resource-card" bodyStyle={{ padding: 16 }}>
              <div className="resource-card-head">
                {r.kind !== 'db' ? <PluginIcon kind={r.kind} /> : null}
                <span className="resource-card-name">{r.name}</span>
                <span className="plugin-kind-label">{KIND_LABEL[r.kind]}</span>
                <Tag size="small" color={r.visibility === 'public' ? 'green' : 'grey'}>
                  {r.visibility === 'public' ? '全部可见' : '仅自己可见'}
                </Tag>
                {r.status === 'disabled' ? (
                  <Tag size="small" color="orange">
                    已停用
                  </Tag>
                ) : null}
              </div>

              <div className="resource-card-desc">{r.description || '（没有描述）'}</div>

              <div className="resource-card-meta">
                <span>最新版本 v{r.latest_version || '-'}</span>
                <span>{r.version_count} 个版本</span>
                <span>发布者 {r.owner_name || '-'}</span>
                {r.source_space_name ? <span>来源 {r.source_space_name}</span> : null}
                <span>更新 {formatTime(r.updated_at)}</span>
              </div>

              <div className="resource-card-actions">
                <Button size="small" theme="borderless" icon={<IconHistory />} onClick={() => setDetailId(r.id)}>
                  版本记录
                </Button>
                <Button size="small" theme="borderless" onClick={() => toggleStatus(r)} loading={setStatus.isPending}>
                  {r.status === 'enabled' ? '停用' : '启用'}
                </Button>
                <Button size="small" theme="borderless" type="danger" icon={<IconDelete />} onClick={() => setPendingDelete(r)}>
                  删除
                </Button>
              </div>
            </Card>
          ))}
        </div>
      )}

      <PublishResourceDialog visible={publishOpen} onClose={() => setPublishOpen(false)} />

      <Modal
        title="版本记录"
        visible={Boolean(detailId)}
        onCancel={() => setDetailId(undefined)}
        footer={null}
        width={560}
      >
        {detail.isLoading ? (
          <div className="panel-empty">
            <Spin />
          </div>
        ) : (
          <div className="resource-versions">
            <Text type="tertiary" size="small">
              {detail.data?.resource.name} · 共 {detail.data?.versions.length ?? 0} 个版本，版本快照不可修改
            </Text>
            {(detail.data?.versions ?? []).map((v) => (
              <div key={v.version} className="resource-version-item">
                <div className="resource-version-head">
                  <span className="resource-version-no">v{v.version}</span>
                  {v.version === detail.data?.resource.latest_version ? (
                    <Tag size="small" color="green">
                      最新
                    </Tag>
                  ) : null}
                  <span className="resource-version-time">{formatTime(v.created_at)}</span>
                  <Button
                    size="small"
                    theme="borderless"
                    type="danger"
                    disabled={(detail.data?.versions.length ?? 0) <= 1}
                    onClick={() =>
                      detail.data && setPendingVersion({ resource: detail.data.resource, version: v.version })
                    }
                  >
                    删除该版本
                  </Button>
                </div>
                <div className="resource-version-desc">{v.notes || '（没有发布说明）'}</div>
                <div className="resource-version-meta">
                  {v.file_count} 个文件 · {formatBytes(v.total_bytes)}
                </div>
              </div>
            ))}
            {(detail.data?.versions.length ?? 0) <= 1 ? (
              <Text type="tertiary" size="small">
                只剩一个版本时不能单独删除，如需下架请直接删除整个资源。
              </Text>
            ) : null}
          </div>
        )}
      </Modal>

      <Modal
        title="删除版本"
        visible={Boolean(pendingVersion)}
        onCancel={() => setPendingVersion(undefined)}
        onOk={confirmDeleteVersion}
        okText="删除"
        cancelText="取消"
        okButtonProps={{ type: 'danger', loading: removeVersion.isPending }}
      >
        <p className="dialog-desc">
          将删除「{pendingVersion?.resource.name}」的版本 {pendingVersion?.version} 及其文件，删除后不可恢复。
        </p>
      </Modal>

      <Modal
        title="删除资源"
        visible={Boolean(pendingDelete)}
        onCancel={() => setPendingDelete(undefined)}
        onOk={confirmDelete}
        okText="删除"
        cancelText="取消"
        okButtonProps={{ type: 'danger', loading: remove.isPending }}
      >
        <p className="dialog-desc">
          将删除「{pendingDelete?.name}」及其全部 {pendingDelete?.version_count} 个版本，删除后不可恢复。
        </p>
      </Modal>
    </div>
  );
}
