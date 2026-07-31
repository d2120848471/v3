#!/usr/bin/env node
/**
 * 从明确指定的公开 pe.*.js 中抽取 32-hex prefix VM。
 *
 * 该 bridge 不加载页面、不发送请求，也不执行整个 bundle；它只编译目标版本
 * 的通用 VM 解释器、prefix 字节码与常量池。不同 pe 分片必须分别提取，
 * 禁止假设 prefix VM 跨版本稳定。
 */

import fs from "node:fs";
import { webcrypto } from "node:crypto";
import { pathToFileURL } from "node:url";


function extractBalanced(source, start, open, close) {
  if (source[start] !== open) {
    throw new Error(`位置 ${start} 不是 ${open}`);
  }
  let depth = 0;
  let quote = null;
  let escaped = false;
  let lineComment = false;
  let blockComment = false;

  for (let index = start; index < source.length; index += 1) {
    const char = source[index];
    const next = source[index + 1];
    if (lineComment) {
      if (char === "\n" || char === "\r") lineComment = false;
      continue;
    }
    if (blockComment) {
      if (char === "*" && next === "/") {
        blockComment = false;
        index += 1;
      }
      continue;
    }
    if (quote !== null) {
      if (escaped) {
        escaped = false;
      } else if (char === "\\") {
        escaped = true;
      } else if (char === quote) {
        quote = null;
      }
      continue;
    }
    if (char === "/" && next === "/") {
      lineComment = true;
      index += 1;
      continue;
    }
    if (char === "/" && next === "*") {
      blockComment = true;
      index += 1;
      continue;
    }
    if (char === "'" || char === '"' || char === "`") {
      quote = char;
      continue;
    }
    if (char === open) depth += 1;
    if (char === close) {
      depth -= 1;
      if (depth === 0) {
        return {
          expression: source.slice(start, index + 1),
          end: index + 1,
        };
      }
    }
  }
  throw new Error(`从 ${start} 开始的 ${open}${close} 表达式未闭合`);
}


function findVmInterpreter(source) {
  // 混淆器会在不同 pe 分片中改变量名：pe.091 是 U，pe.094 是 L。
  // 因此用“赋值给变量的六参数函数 + VM 特征语句”识别，不依赖符号名。
  const pattern =
    /(?:^|[,;])\s*([A-Za-z_$][\w$]*)\s*=\s*(function(?:\s+[A-Za-z_$][\w$]*)?\s*\(([^)]*)\)\s*\{)/g;
  for (const match of source.matchAll(pattern)) {
    const parameters = match[3]
      .split(",")
      .map((item) => item.trim())
      .filter(Boolean);
    if (parameters.length !== 6) continue;
    const functionStart = match.index + match[0].indexOf("function");
    const braceStart = source.indexOf("{", functionStart);
    const body = extractBalanced(source, braceStart, "{", "}");
    const expression =
      source.slice(functionStart, braceStart) + body.expression;
    if (
      expression.includes("Object.create") &&
      expression.includes('"__proto__"')
    ) {
      return { expression, end: body.end };
    }
  }
  throw new Error("未找到 pe checksum 使用的 U VM 解释器");
}


function findFollowingArrayAssignments(source, start) {
  const pattern =
    /(?:^|[,;])\s*([A-Za-z_$][\w$]*)\s*=\s*\[/g;
  pattern.lastIndex = start;
  const arrays = [];

  while (arrays.length < 12) {
    const match = pattern.exec(source);
    if (!match || match.index - start > 120_000) break;
    const arrayStart = source.indexOf("[", match.index);
    const array = extractBalanced(source, arrayStart, "[", "]");
    arrays.push({
      name: match[1],
      expression: array.expression,
      start: arrayStart,
      end: array.end,
    });
    // 跳过整个数组，避免把常量表达式里的嵌套数组误认为同级赋值。
    pattern.lastIndex = array.end;
  }
  return arrays;
}


export function extractChecksumVm(source) {
  const vm = findVmInterpreter(source);
  const arrays = findFollowingArrayAssignments(source, vm.end);
  if (arrays.length < 2) {
    throw new Error("VM 解释器后没有找到 program/constants 数组");
  }

  // 同一解释器先承载 64-state，再承载 checksum；checksum program 是紧随
  // 其后的最大字节码数组。符号名在 pe.091 为 q/X，在 pe.094 为 Z/q。
  let programIndex = 0;
  for (let index = 1; index < arrays.length; index += 1) {
    if (
      arrays[index].expression.length >
      arrays[programIndex].expression.length
    ) {
      programIndex = index;
    }
  }
  const program = arrays[programIndex];
  const constants = arrays[programIndex + 1];
  if (!constants) {
    throw new Error("checksum program 后缺少 constants 数组");
  }
  return {
    interpreterSource: vm.expression,
    programSource: program.expression,
    constantsSource: constants.expression,
  };
}


function browserPrimitives() {
  const atobCompat =
    globalThis.atob ||
    ((value) => Buffer.from(String(value), "base64").toString("binary"));
  const btoaCompat =
    globalThis.btoa ||
    ((value) => Buffer.from(String(value), "binary").toString("base64"));
  if (
    typeof globalThis.escape !== "function" ||
    typeof globalThis.unescape !== "function"
  ) {
    throw new Error("当前 Node 缺少 checksum VM 需要的 escape/unescape");
  }
  const primitives = {
    Object,
    String,
    Number,
    Boolean,
    Array,
    Math,
    // 浏览器优先走 crypto.getRandomValues；缺失时 VM 会进入一个带固定尾字节
    // 的兼容分支，生成结果与真实页面不同。
    crypto: webcrypto,
    Uint8Array,
    encodeURIComponent,
    decodeURIComponent,
    escape: globalThis.escape,
    unescape: globalThis.unescape,
    atob: atobCompat,
    btoa: btoaCompat,
  };
  // 浏览器中 window.window === window；某些分片会探测这一关系。
  primitives.window = primitives;
  return primitives;
}


export function checksumFromPeSource(source, jsonText, seed = "0000") {
  if (typeof jsonText !== "string") {
    throw new TypeError("jsonText 必须是字符串");
  }
  if (typeof seed !== "string" || seed.length === 0) {
    throw new TypeError("seed 必须是非空字符串");
  }
  const extracted = extractChecksumVm(source);
  const factory = new Function(
    "window",
    `"use strict";
     const U=${extracted.interpreterSource};
     const q=${extracted.programSource};
     const X=${extracted.constantsSource};
     return {U,q,X};`,
  );
  const { U, q, X } = factory(browserPrimitives());
  const checksum = U(0, [], q, X, { r: 1 }, [jsonText, seed]);
  if (!/^[0-9a-f]{32}$/i.test(checksum)) {
    throw new Error("checksum VM 未返回 32-hex");
  }
  return checksum.toLowerCase();
}


// 更准确的新名称；旧导出为兼容已写好的 Python bridge 保留。
export const prefixFromPeSource = checksumFromPeSource;


function parseArguments(argv) {
  let pePath = null;
  let seed = "0000";
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--pe") {
      pePath = argv[++index];
    } else if (argument === "--seed") {
      seed = argv[++index];
    } else {
      throw new Error(`未知参数：${argument}`);
    }
  }
  if (!pePath) throw new Error("缺少 --pe /path/to/pe.*.js");
  return { pePath, seed };
}


async function main() {
  const { pePath, seed } = parseArguments(process.argv.slice(2));
  const source = fs.readFileSync(pePath, "utf8");
  // stdin 必须是 checksum 所针对的原样紧凑 JSON；不能 trim 或重新 stringify。
  const jsonText = fs.readFileSync(0, "utf8");
  process.stdout.write(`${checksumFromPeSource(source, jsonText, seed)}\n`);
}


if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  main().catch((error) => {
    process.stderr.write(`${error?.message || error}\n`);
    process.exitCode = 1;
  });
}
