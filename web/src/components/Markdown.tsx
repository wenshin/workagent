import type { ReactNode } from 'react';

/**
 * 轻量 Markdown 渲染：只覆盖文档类文件常用的语法（标题、列表、代码块、引用、表格、链接），
 * 不引入额外依赖，也不用 dangerouslySetInnerHTML，避免外部文件带 HTML 注入风险。
 * 需要完整 CommonMark 时再替换为 marked/markdown-it。
 */
interface Props {
  content: string;
}

/** 行内语法：`code`、**粗体**、[链接](url) */
function renderInline(text: string, prefix: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  const pattern = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\[[^\]]+\]\([^)]+\))/g;
  let last = 0;
  let match: RegExpExecArray | null;
  let index = 0;

  while ((match = pattern.exec(text)) !== null) {
    if (match.index > last) nodes.push(text.slice(last, match.index));
    const token = match[0];
    const key = `${prefix}-${index}`;
    if (token.startsWith('`')) {
      nodes.push(<code key={key}>{token.slice(1, -1)}</code>);
    } else if (token.startsWith('**')) {
      nodes.push(<strong key={key}>{token.slice(2, -2)}</strong>);
    } else {
      const link = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(token);
      if (link) {
        nodes.push(
          <a key={key} href={link[2]} target="_blank" rel="noreferrer">
            {link[1]}
          </a>,
        );
      } else {
        nodes.push(token);
      }
    }
    last = match.index + token.length;
    index += 1;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}

function isTableDivider(line: string) {
  return /^\|?[\s:-]*-[\s|:-]*\|?$/.test(line.trim());
}

function splitRow(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, '')
    .replace(/\|$/, '')
    .split('|')
    .map((cell) => cell.trim());
}

export default function MarkdownView({ content }: Props) {
  const lines = content.replace(/\r\n/g, '\n').split('\n');
  const blocks: ReactNode[] = [];
  let i = 0;
  let key = 0;

  while (i < lines.length) {
    const line = lines[i];

    // 代码块
    if (line.trimStart().startsWith('```')) {
      const lang = line.trim().slice(3).trim();
      const code: string[] = [];
      i += 1;
      while (i < lines.length && !lines[i].trimStart().startsWith('```')) {
        code.push(lines[i]);
        i += 1;
      }
      i += 1; // 跳过结尾 ```
      blocks.push(
        <pre key={key++} className="md-code">
          {lang ? <span className="md-code-lang">{lang}</span> : null}
          <code>{code.join('\n')}</code>
        </pre>,
      );
      continue;
    }

    // 标题
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      const level = heading[1].length;
      const text = heading[2];
      const Tag = (`h${Math.min(level + 1, 6)}`) as 'h2' | 'h3' | 'h4' | 'h5' | 'h6';
      blocks.push(<Tag key={key++}>{renderInline(text, `h${key}`)}</Tag>);
      i += 1;
      continue;
    }

    // 分割线
    if (/^\s*(-{3,}|\*{3,})\s*$/.test(line)) {
      blocks.push(<hr key={key++} />);
      i += 1;
      continue;
    }

    // 表格：表头行 + 分隔行
    if (line.trim().startsWith('|') && i + 1 < lines.length && isTableDivider(lines[i + 1])) {
      const header = splitRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].trim().startsWith('|')) {
        rows.push(splitRow(lines[i]));
        i += 1;
      }
      blocks.push(
        <div key={key++} className="md-table-wrap">
          <table className="md-table">
            <thead>
              <tr>
                {header.map((cell, ci) => (
                  <th key={ci}>{renderInline(cell, `th${ci}`)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row, ri) => (
                <tr key={ri}>
                  {header.map((_, ci) => (
                    <td key={ci}>{renderInline(row[ci] ?? '', `td${ri}-${ci}`)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      continue;
    }

    // 引用：连续的 > 行合并
    if (line.trimStart().startsWith('>')) {
      const quote: string[] = [];
      while (i < lines.length && lines[i].trimStart().startsWith('>')) {
        quote.push(lines[i].trimStart().replace(/^>\s?/, ''));
        i += 1;
      }
      blocks.push(
        <blockquote key={key++}>{renderInline(quote.join(' '), `q${key}`)}</blockquote>,
      );
      continue;
    }

    // 列表：无序与有序各自成块
    const bullet = /^\s*[-*+]\s+(.*)$/.exec(line);
    const ordered = /^\s*\d+\.\s+(.*)$/.exec(line);
    if (bullet || ordered) {
      const isOrdered = Boolean(ordered);
      const items: string[] = [];
      while (i < lines.length) {
        const b = /^\s*[-*+]\s+(.*)$/.exec(lines[i]);
        const o = /^\s*\d+\.\s+(.*)$/.exec(lines[i]);
        if (isOrdered ? !o : !b) break;
        items.push((isOrdered ? o![1] : b![1]) as string);
        i += 1;
      }
      const ListTag = isOrdered ? 'ol' : 'ul';
      blocks.push(
        <ListTag key={key++}>
          {items.map((item, ii) => (
            <li key={ii}>{renderInline(item, `li${key}-${ii}`)}</li>
          ))}
        </ListTag>,
      );
      continue;
    }

    // 空行
    if (line.trim() === '') {
      i += 1;
      continue;
    }

    // 段落：连续非空行合并
    const para: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !lines[i].trimStart().startsWith('```') &&
      !lines[i].trimStart().startsWith('>') &&
      !/^(#{1,6})\s+/.test(lines[i]) &&
      !/^\s*[-*+]\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s+/.test(lines[i]) &&
      !lines[i].trim().startsWith('|')
    ) {
      para.push(lines[i].trim());
      i += 1;
    }
    // 兜底：行内看不出版块归属时按单行输出，保证 i 一定前进
    if (para.length === 0) {
      blocks.push(<p key={key++}>{renderInline(line, `p${key}`)}</p>);
      i += 1;
      continue;
    }
    blocks.push(<p key={key++}>{renderInline(para.join(' '), `p${key}`)}</p>);
  }

  return <div className="md-body">{blocks}</div>;
}
