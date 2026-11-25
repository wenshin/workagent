import hljs from 'highlight.js/lib/core';
import bash from 'highlight.js/lib/languages/bash';
import c from 'highlight.js/lib/languages/c';
import clojure from 'highlight.js/lib/languages/clojure';
import cmake from 'highlight.js/lib/languages/cmake';
import cpp from 'highlight.js/lib/languages/cpp';
import csharp from 'highlight.js/lib/languages/csharp';
import css from 'highlight.js/lib/languages/css';
import dart from 'highlight.js/lib/languages/dart';
import diff from 'highlight.js/lib/languages/diff';
import dockerfile from 'highlight.js/lib/languages/dockerfile';
import elixir from 'highlight.js/lib/languages/elixir';
import erlang from 'highlight.js/lib/languages/erlang';
import go from 'highlight.js/lib/languages/go';
import graphql from 'highlight.js/lib/languages/graphql';
import groovy from 'highlight.js/lib/languages/groovy';
import haskell from 'highlight.js/lib/languages/haskell';
import ini from 'highlight.js/lib/languages/ini';
import java from 'highlight.js/lib/languages/java';
import javascript from 'highlight.js/lib/languages/javascript';
import json from 'highlight.js/lib/languages/json';
import julia from 'highlight.js/lib/languages/julia';
import kotlin from 'highlight.js/lib/languages/kotlin';
import latex from 'highlight.js/lib/languages/latex';
import less from 'highlight.js/lib/languages/less';
import lua from 'highlight.js/lib/languages/lua';
import makefile from 'highlight.js/lib/languages/makefile';
import markdown from 'highlight.js/lib/languages/markdown';
import nginx from 'highlight.js/lib/languages/nginx';
import objectivec from 'highlight.js/lib/languages/objectivec';
import perl from 'highlight.js/lib/languages/perl';
import php from 'highlight.js/lib/languages/php';
import plaintext from 'highlight.js/lib/languages/plaintext';
import powershell from 'highlight.js/lib/languages/powershell';
import protobuf from 'highlight.js/lib/languages/protobuf';
import python from 'highlight.js/lib/languages/python';
import r from 'highlight.js/lib/languages/r';
import ruby from 'highlight.js/lib/languages/ruby';
import rust from 'highlight.js/lib/languages/rust';
import scala from 'highlight.js/lib/languages/scala';
import scss from 'highlight.js/lib/languages/scss';
import sql from 'highlight.js/lib/languages/sql';
import swift from 'highlight.js/lib/languages/swift';
import typescript from 'highlight.js/lib/languages/typescript';
import vbnet from 'highlight.js/lib/languages/vbnet';
import xml from 'highlight.js/lib/languages/xml';
import yaml from 'highlight.js/lib/languages/yaml';

/**
 * 全局唯一的语法高亮实例。
 *
 * 只注册工作空间里真正会出现的语言（core + 按需），不引全量 bundle。
 * 对话里的代码块与文件预览共用这一份注册表：同一个语言注册两遍会互相覆盖，
 * 而高亮缓存的键里带语言名，两处各自缓存同一段源码也只会浪费内存。
 */
for (const [name, lang] of Object.entries({
  bash,
  c,
  clojure,
  cmake,
  cpp,
  csharp,
  css,
  dart,
  diff,
  dockerfile,
  elixir,
  erlang,
  go,
  graphql,
  groovy,
  haskell,
  ini,
  java,
  javascript,
  json,
  julia,
  kotlin,
  latex,
  less,
  lua,
  makefile,
  markdown,
  nginx,
  objectivec,
  perl,
  php,
  plaintext,
  powershell,
  protobuf,
  python,
  r,
  ruby,
  rust,
  scala,
  scss,
  sql,
  swift,
  typescript,
  vbnet,
  xml,
  yaml,
})) {
  hljs.registerLanguage(name, lang);
}

/** 文件扩展名 → 高亮语言。没有列出的扩展名按纯文本处理，不猜语言 */
const EXT_LANGUAGES: Record<string, string> = {
  ts: 'typescript',
  tsx: 'typescript',
  mts: 'typescript',
  cts: 'typescript',
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  vue: 'xml',
  svelte: 'xml',
  html: 'xml',
  htm: 'xml',
  xhtml: 'xml',
  xml: 'xml',
  svg: 'xml',
  xaml: 'xml',
  csproj: 'xml',
  css: 'css',
  scss: 'scss',
  sass: 'scss',
  less: 'less',
  json: 'json',
  jsonc: 'json',
  json5: 'json',
  yaml: 'yaml',
  yml: 'yaml',
  // highlight.js 没有独立的 toml 语法，ini 覆盖了 [section] + key = value 这套写法
  toml: 'ini',
  ini: 'ini',
  cfg: 'ini',
  conf: 'ini',
  properties: 'ini',
  env: 'ini',
  md: 'markdown',
  markdown: 'markdown',
  mdx: 'markdown',
  py: 'python',
  pyi: 'python',
  go: 'go',
  rs: 'rust',
  java: 'java',
  kt: 'kotlin',
  kts: 'kotlin',
  swift: 'swift',
  c: 'c',
  h: 'c',
  cc: 'cpp',
  cpp: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  hh: 'cpp',
  hxx: 'cpp',
  cs: 'csharp',
  php: 'php',
  rb: 'ruby',
  sh: 'bash',
  bash: 'bash',
  zsh: 'bash',
  ps1: 'powershell',
  psm1: 'powershell',
  sql: 'sql',
  lua: 'lua',
  dart: 'dart',
  scala: 'scala',
  sc: 'scala',
  pl: 'perl',
  pm: 'perl',
  r: 'r',
  proto: 'protobuf',
  graphql: 'graphql',
  gql: 'graphql',
  diff: 'diff',
  patch: 'diff',
  vb: 'vbnet',
  clj: 'clojure',
  cljs: 'clojure',
  edn: 'clojure',
  ex: 'elixir',
  exs: 'elixir',
  erl: 'erlang',
  hrl: 'erlang',
  hs: 'haskell',
  m: 'objectivec',
  tex: 'latex',
  jl: 'julia',
  groovy: 'groovy',
  gradle: 'groovy',
  cmake: 'cmake',
  nginx: 'nginx',
  txt: 'plaintext',
  log: 'plaintext',
  csv: 'plaintext',
  tsv: 'plaintext',
};

/** 没有扩展名、靠文件名识别的文件（构建脚本 / 容器清单 / 隐藏配置） */
const NAME_LANGUAGES: Record<string, string> = {
  dockerfile: 'dockerfile',
  containerfile: 'dockerfile',
  makefile: 'makefile',
  gnumakefile: 'makefile',
  'cmakelists.txt': 'cmake',
  '.gitignore': 'ini',
  '.gitattributes': 'ini',
  '.dockerignore': 'ini',
  '.editorconfig': 'ini',
  '.npmrc': 'ini',
  '.prettierrc': 'json',
  '.eslintrc': 'json',
};

/** 文件名 → 高亮语言；识别不出时返回 undefined，调用方按纯文本渲染 */
export function languageForFile(name: string): string | undefined {
  const base = name.split('/').pop()?.toLowerCase() ?? '';
  if (!base) return undefined;
  const byName = NAME_LANGUAGES[base];
  if (byName) return byName;
  // 隐藏配置（.env.local 这类）没有常规扩展名：path.extname 的口径是「最后一个点之后」
  const dot = base.lastIndexOf('.');
  if (dot <= 0 || dot === base.length - 1) return undefined;
  return EXT_LANGUAGES[base.slice(dot + 1)];
}

/**
 * 流式输出时每来一个增量整篇 Markdown 都会重解析，已结束的代码块没必要重复高亮。
 * 只做定长 FIFO：键里带语言，避免同一段源码在两种语言下互相命中。
 */
const HIGHLIGHT_CACHE_MAX = 80;
const highlightCache = new Map<string, string>();

/**
 * 按语言上色，返回可安全注入的 HTML（源码已转义，只含 <span class="hljs-*">）。
 *
 * 语言没注册或解析失败时返回 null，由调用方退回纯文本：猜错语言比不上色更糟。
 */
export function highlightCode(source: string, lang: string): string | null {
  const name = lang.split(/\s+/)[0]?.toLowerCase() ?? '';
  if (!name || !hljs.getLanguage(name)) return null;
  const cacheKey = `${name}\u0000${source}`;
  const cached = highlightCache.get(cacheKey);
  if (cached !== undefined) return cached;
  try {
    const value = hljs.highlight(source, { language: name, ignoreIllegals: true }).value;
    if (highlightCache.size >= HIGHLIGHT_CACHE_MAX) {
      const oldest = highlightCache.keys().next().value;
      if (oldest !== undefined) highlightCache.delete(oldest);
    }
    highlightCache.set(cacheKey, value);
    return value;
  } catch {
    return null;
  }
}

/**
 * 文件正文的上色上限：整篇一次性解析是同步的，几百 KB 的代码文件会把主线程卡住。
 * 超过就按纯文本展示——读得下去比配色重要。
 */
const MAX_HIGHLIGHT_CHARS = 200_000;

/** 按文件名上色：识别不出语言、或正文过大时返回 null（调用方按纯文本渲染） */
export function highlightFile(name: string, source: string): string | null {
  if (source.length > MAX_HIGHLIGHT_CHARS) return null;
  const lang = languageForFile(name);
  if (!lang) return null;
  return highlightCode(source, lang);
}
