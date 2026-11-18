import { Button, Modal, Slider, Toast, Typography } from '@douyinfe/semi-ui';
import { useEffect, useRef, useState } from 'react';
import type { PointerEvent as ReactPointerEvent } from 'react';

import { useClearAgentIcon, useSetAgentIcon } from '../api/hooks';

const { Text } = Typography;

/** 裁剪框显示尺寸与落盘尺寸：显示小、落盘大，缩放时不至于糊 */
const VIEW = 240;
const OUTPUT = 256;
/** 原图上限：超过它先让用户自己压，避免把浏览器拖死 */
const MAX_SOURCE_BYTES = 8 << 20;

interface Props {
  visible: boolean;
  spaceId: string;
  slug?: string;
  name?: string;
  /** 当前头像地址：有才给「移除头像」 */
  currentIcon?: string;
  onClose: () => void;
}

interface Placement {
  x: number;
  y: number;
  w: number;
  h: number;
}

function place(img: HTMLImageElement, zoom: number, offset: { x: number; y: number }): Placement {
  // cover：短边先铺满裁剪框，任何缩放下都不会露白
  const base = Math.max(VIEW / img.naturalWidth, VIEW / img.naturalHeight);
  const scale = base * zoom;
  const w = img.naturalWidth * scale;
  const h = img.naturalHeight * scale;
  return { x: (VIEW - w) / 2 + offset.x, y: (VIEW - h) / 2 + offset.y, w, h };
}

/** 拖到边上时把图停住：再拖就露出画布底色了 */
function clampOffset(img: HTMLImageElement, zoom: number, offset: { x: number; y: number }) {
  const p = place(img, zoom, { x: 0, y: 0 });
  const maxX = Math.max(0, (p.w - VIEW) / 2);
  const maxY = Math.max(0, (p.h - VIEW) / 2);
  return {
    x: Math.min(maxX, Math.max(-maxX, offset.x)),
    y: Math.min(maxY, Math.max(-maxY, offset.y)),
  };
}

function loadImage(src: string) {
  return new Promise<HTMLImageElement>((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('图片解析失败，请换一张试试'));
    img.src = src;
  });
}

/**
 * 智能体头像：先在本机裁成正方形再上传。
 *
 * 为什么在浏览器里裁：原图动辄几 MB，直接传上去再让后端处理会把上传时间
 * 变成用户可感知的等待；这里按 1:1 裁剪 + PNG 压缩，落盘只有几十 KB。
 * 服务端仍会再裁一次——直接打接口也不能把大图塞进空间。
 */
export default function AgentAvatarDialog({
  visible,
  spaceId,
  slug,
  name,
  currentIcon,
  onClose,
}: Props) {
  const setIcon = useSetAgentIcon();
  const clearIcon = useClearAgentIcon();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const dragRef = useRef<{ x: number; y: number; origin: { x: number; y: number } } | null>(null);

  const [image, setImage] = useState<HTMLImageElement | null>(null);
  const [zoom, setZoom] = useState(1);
  const [offset, setOffset] = useState({ x: 0, y: 0 });

  const busy = setIcon.isPending || clearIcon.isPending;
  const target = name || slug || '智能体';

  // 每次打开都从「未选图」开始：上一张的缩放不该带进下一次
  useEffect(() => {
    if (!visible) return;
    setImage(null);
    setZoom(1);
    setOffset({ x: 0, y: 0 });
  }, [visible]);

  useEffect(() => {
    const canvas = canvasRef.current;
    const ctx = canvas?.getContext('2d');
    if (!canvas || !ctx) return;
    ctx.clearRect(0, 0, VIEW, VIEW);
    if (!image) return;
    const p = place(image, zoom, offset);
    ctx.drawImage(image, p.x, p.y, p.w, p.h);
  }, [image, zoom, offset]);

  const pick = async (file?: File) => {
    if (!file) return;
    if (!file.type.startsWith('image/')) {
      Toast.error('请选择图片文件');
      return;
    }
    if (file.size > MAX_SOURCE_BYTES) {
      Toast.error('原图不能超过 8MB，请先压缩或换一张');
      return;
    }
    // 先转成 object URL 再解码：图片一旦加载完，URL 就可以释放
    const url = URL.createObjectURL(file);
    try {
      const img = await loadImage(url);
      setImage(img);
      setZoom(1);
      setOffset({ x: 0, y: 0 });
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '图片解析失败');
    } finally {
      URL.revokeObjectURL(url);
    }
  };

  const onPointerDown = (e: ReactPointerEvent<HTMLCanvasElement>) => {
    if (!image) return;
    e.currentTarget.setPointerCapture(e.pointerId);
    dragRef.current = { x: e.clientX, y: e.clientY, origin: offset };
  };

  const onPointerMove = (e: ReactPointerEvent<HTMLCanvasElement>) => {
    const drag = dragRef.current;
    if (!drag || !image) return;
    setOffset(
      clampOffset(image, zoom, {
        x: drag.origin.x + (e.clientX - drag.x),
        y: drag.origin.y + (e.clientY - drag.y),
      }),
    );
  };

  const onPointerUp = (e: ReactPointerEvent<HTMLCanvasElement>) => {
    dragRef.current = null;
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId);
    }
  };

  const save = async () => {
    if (!slug) return;
    if (!image) {
      Toast.warning('先选择一张图片');
      return;
    }
    const canvas = document.createElement('canvas');
    canvas.width = OUTPUT;
    canvas.height = OUTPUT;
    const ctx = canvas.getContext('2d');
    if (!ctx) {
      Toast.error('当前浏览器不支持图片裁剪');
      return;
    }
    // 把显示坐标系里的裁剪结果换算到落盘尺寸，服务端再做一次同样的裁剪
    const p = place(image, zoom, offset);
    const k = OUTPUT / VIEW;
    ctx.drawImage(image, p.x * k, p.y * k, p.w * k, p.h * k);
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
    if (!blob) {
      Toast.error('图片压缩失败，请换一张试试');
      return;
    }
    try {
      await setIcon.mutateAsync({ spaceId, slug, blob });
      Toast.success('头像已更新');
      onClose();
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '头像保存失败');
    }
  };

  const remove = async () => {
    if (!slug) return;
    try {
      await clearIcon.mutateAsync({ spaceId, slug });
      Toast.success('已移除头像');
      onClose();
    } catch (err) {
      Toast.error(err instanceof Error ? err.message : '移除失败');
    }
  };

  return (
    <Modal
      title={`设置「${target}」的头像`}
      visible={visible}
      onCancel={onClose}
      footer={
        <div className="agent-avatar-actions">
          {currentIcon ? (
            <Button theme="borderless" type="danger" disabled={busy} onClick={() => void remove()}>
              移除头像
            </Button>
          ) : null}
          <div className="agent-avatar-actions-right">
            <Button disabled={busy} onClick={onClose}>
              取消
            </Button>
            <Button theme="solid" type="primary" loading={busy} onClick={() => void save()}>
              保存
            </Button>
          </div>
        </div>
      }
    >
      <div className="agent-avatar-editor">
        <div className="agent-avatar-stage">
          {image ? (
            <canvas
              ref={canvasRef}
              width={VIEW}
              height={VIEW}
              className="agent-avatar-canvas"
              onPointerDown={onPointerDown}
              onPointerMove={onPointerMove}
              onPointerUp={onPointerUp}
              onPointerCancel={onPointerUp}
            />
          ) : (
            <button
              type="button"
              className="agent-avatar-empty"
              onClick={() => fileRef.current?.click()}
            >
              选择图片
            </button>
          )}
        </div>

        <input
          ref={fileRef}
          type="file"
          accept="image/png,image/jpeg,image/webp"
          hidden
          onChange={(e) => {
            void pick(e.target.files?.[0]);
            // 允许重复选同一张：不清空的话第二次不会再触发 change
            e.target.value = '';
          }}
        />

        {image ? (
          <>
            <div className="agent-avatar-zoom">
              <Text type="tertiary" size="small">
                缩放
              </Text>
              <Slider
                value={zoom}
                min={1}
                max={3}
                step={0.01}
                onChange={(value) => {
                  const next = Array.isArray(value) ? value[0] : value;
                  setZoom(typeof next === 'number' ? next : 1);
                }}
              />
            </div>
            <Text type="tertiary" size="small">
              拖动图片调整取景范围，保存后按正方形裁切并压缩到 {OUTPUT}×{OUTPUT}。
            </Text>
            <Button theme="borderless" size="small" onClick={() => fileRef.current?.click()}>
              换一张
            </Button>
          </>
        ) : (
          <Text type="tertiary" size="small">
            支持 PNG / JPEG / WebP，建议使用正方形图片；上传后会自动裁剪与压缩。
          </Text>
        )}
      </div>
    </Modal>
  );
}
