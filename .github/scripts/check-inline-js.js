#!/usr/bin/env node
'use strict';

// Checks the inline <script> blocks of the HTML files whose paths are given as
// command line arguments, in two passes:
//
//   1. Syntax: each block is written to a temp file and run through
//      `node --check`. This catches parse errors only.
//
//   2. Undefined identifiers: all blocks of a file are concatenated (classic
//      scripts share one global scope, so a function declared in block 0 is
//      legitimately callable from block 1) and run through ESLint with ONLY
//      the `no-undef` rule, browser globals, and the small allowlist below.
//      This is the pass that catches the #5516 class of bug — a call to a
//      helper that was never defined (`apiFetch is not defined`) parses fine
//      and shipped to a release because pass 1 was the only gate.
//
// Pass 2 requires `npm ci --ignore-scripts` to have been run in
// .github/scripts (eslint + globals are exact-pinned in package.json there).
// If the dependency is missing the script FAILS rather than silently skipping
// the pass: a checker that quietly stops checking is how #5388 happened.
//
// Paths are never interpolated into a shell or JavaScript string, so any
// filename (quotes, spaces, metacharacters) is handled safely.

const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const SCRIPT_BLOCK_RE = /<script[^>]*>([\s\S]*?)<\/script>/gi;
const SCRIPT_CLOSE_TAG_LEN = '</script>'.length;

// ECMAScript level the shipped dashboard is written against. In ESLint flat
// config this sets both the parser level and the set of built-in globals
// (Promise, Map, structuredClone, ...) that `no-undef` accepts.
const ECMA_VERSION = 2022;

// Identifiers the page legitimately gets from somewhere other than its own
// inline scripts. Neither HTML file has an external `<script src>` tag or a
// dynamic script loader, so this list is empty on purpose. Add an entry ONLY
// with a comment naming where the global comes from, and verify by grep that
// the source really provides it — an allowlist entry is a hole in this check.
const EXTRA_GLOBALS = {
  // Example (do not uncomment without a real provider):
  //   marked: 'readonly', // provided by <script src="marked.min.js">
};

function loadEslint() {
  try {
    const { Linter } = require('eslint');
    const globals = require('globals');
    return { Linter, globals };
  } catch (err) {
    console.error(
      'ERROR: eslint/globals are not installed for the no-undef pass.\n' +
      '       Run `npm ci --ignore-scripts` in .github/scripts first.\n' +
      `       (${err.message})`
    );
    return null;
  }
}

function lineOf(text, offset) {
  let line = 1;
  for (let i = 0; i < offset; i++) {
    if (text.charCodeAt(i) === 10) line++;
  }
  return line;
}

// Returns [{ js, htmlLine }] for every non-empty <script> block. `js` is the
// raw block contents (not trimmed) so that line N of `js` is HTML line
// htmlLine + N - 1; the opening tag and the first JS line share a line.
function extractBlocks(html) {
  const blocks = [];
  let match;
  SCRIPT_BLOCK_RE.lastIndex = 0;
  while ((match = SCRIPT_BLOCK_RE.exec(html)) !== null) {
    const js = match[1];
    if (!js.trim()) continue;
    const jsStart = match.index + match[0].length - js.length - SCRIPT_CLOSE_TAG_LEN;
    blocks.push({ js, htmlLine: lineOf(html, jsStart) });
  }
  return blocks;
}

function syntaxCheck(blocks, tmpDir) {
  let failed = false;
  blocks.forEach((block, idx) => {
    const outFile = path.join(tmpDir, `block_${idx}.js`);
    fs.writeFileSync(outFile, block.js.trim());
    console.log(`  Parsing ${path.basename(outFile)}...`);
    try {
      execFileSync(process.execPath, ['--check', outFile], { stdio: 'inherit' });
    } catch (err) {
      failed = true;
    }
    fs.rmSync(outFile, { force: true });
  });
  return !failed;
}

// Maps a line in the concatenated script back to (block index, HTML line).
function buildLineMap(blocks) {
  const starts = [];
  let cumulative = 0;
  for (const block of blocks) {
    starts.push(cumulative);
    cumulative += block.js.split('\n').length;
  }
  return (concatLine) => {
    let idx = 0;
    while (idx + 1 < starts.length && starts[idx + 1] < concatLine) idx++;
    const lineInBlock = concatLine - starts[idx];
    return { block: idx, htmlLine: blocks[idx].htmlLine + lineInBlock - 1 };
  };
}

// `no-undef` reports every reference to an unknown name, including one that
// is guarded on the same line by a feature test such as
//   if (typeof openPlanReview === 'function') openPlanReview(id);
// That pattern is safe at runtime (the call only happens when the name
// exists) and is how the dashboard wires optional navigation hooks. Exempt a
// reference only when its OWN line carries a `typeof NAME` comparison, so an
// unguarded call to the same name elsewhere still fails. This is deliberately
// narrower than adding the name to EXTRA_GLOBALS.
const NOT_DEFINED_RE = /^'([^']+)' is not defined\.$/;

function isTypeofGuardedOnLine(message, lines) {
  const m = NOT_DEFINED_RE.exec(message.message);
  if (!m) return false;
  const name = m[1];
  const line = lines[message.line - 1] || '';
  const guard = new RegExp('typeof\\s+' + name + '\\s*[!=]==?\\s*[\'"]');
  return guard.test(line);
}

function noUndefCheck(htmlFile, blocks, eslint) {
  const { Linter, globals } = eslint;
  const code = blocks.map((b) => b.js).join('\n');
  const toHtml = buildLineMap(blocks);

  // Flat-config matching only applies to files under the Linter's cwd, so
  // anchor cwd at the HTML file's own directory: the checker then works on
  // any path (repo-relative in CI, absolute in local runs), and a fatal
  // "No matching configuration" can never masquerade as a clean pass.
  const absHtml = path.resolve(htmlFile);
  const linter = new Linter({ cwd: path.dirname(absHtml) });
  const messages = linter.verify(
    code,
    [{
      files: ['**/*.js'],
      languageOptions: {
        ecmaVersion: ECMA_VERSION,
        sourceType: 'script',
        globals: { ...globals.browser, ...EXTRA_GLOBALS },
      },
      rules: { 'no-undef': 'error' },
    }],
    { filename: `${path.basename(absHtml)}.inline.js` }
  );

  const lines = code.split('\n');
  const findings = messages.filter((m) => !isTypeofGuardedOnLine(m, lines));
  const exempted = messages.length - findings.length;

  console.log(`  no-undef over ${blocks.length} concatenated block(s), ${lines.length} lines` +
    (exempted ? ` (${exempted} typeof-guarded reference(s) exempted)` : '') + '...');
  for (const m of findings) {
    const { block, htmlLine } = toHtml(m.line);
    console.log(`  ${htmlFile}:${htmlLine}:${m.column} (block ${block}) ${m.message} [${m.ruleId || 'fatal'}]`);
  }
  return findings.length === 0;
}

function checkFile(htmlFile, tmpDir, eslint) {
  const html = fs.readFileSync(htmlFile, 'utf8');
  const blocks = extractBlocks(html);
  console.log(`Extracted ${blocks.length} script block(s)`);
  if (blocks.length === 0) return true;

  const syntaxOk = syntaxCheck(blocks, tmpDir);
  // A parse error would make the no-undef pass report the same problem less
  // clearly; only run it on syntactically valid input.
  if (!syntaxOk) return false;
  return noUndefCheck(htmlFile, blocks, eslint);
}

function main() {
  const files = process.argv.slice(2);
  if (files.length === 0) {
    console.log('No HTML files to check.');
    return 0;
  }

  const eslint = loadEslint();
  if (!eslint) return 1;

  const tmpRoot = process.env.RUNNER_TEMP || os.tmpdir();
  const tmpDir = fs.mkdtempSync(path.join(tmpRoot, 'inline-js-'));
  let ok = true;
  try {
    for (const htmlFile of files) {
      console.log(`Checking ${htmlFile}...`);
      if (!checkFile(htmlFile, tmpDir, eslint)) ok = false;
    }
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }

  if (!ok) {
    console.error('Inline script check failed (syntax error or undefined identifier).');
    return 1;
  }
  console.log('All script blocks passed syntax and no-undef checks.');
  return 0;
}

process.exit(main());
