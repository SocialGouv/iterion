#!/usr/bin/env node
// validate_syntax.js — Extract inline <script> blocks from HTML files and
// check them for JavaScript syntax errors using Node's vm.compileFunction.
// Usage: node validate_syntax.js <directory>
// Output: JSON { valid: bool, errors: string[], file_count: int }

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const dir = process.argv[2];
if (!dir) {
  console.log(JSON.stringify({ valid: false, errors: ["usage: node validate_syntax.js <directory>"], file_count: 0 }));
  process.exit(0);
}

const files = fs.readdirSync(dir).filter((f) => f.endsWith(".html"));
const errors = [];

for (const file of files) {
  const html = fs.readFileSync(path.join(dir, file), "utf8");
  const re = /<script[^>]*>([\s\S]*?)<\/script>/gi;
  let match;
  let blockIndex = 0;

  while ((match = re.exec(html)) !== null) {
    blockIndex++;
    const code = match[1].trim();
    if (!code) continue;

    // Compute the line number where this <script> block starts.
    const lineNumber = html.substring(0, match.index).split("\n").length;

    try {
      vm.compileFunction(code);
    } catch (e) {
      errors.push(`${file}:${lineNumber} (script block #${blockIndex}): ${e.message}`);
    }
  }
}

console.log(JSON.stringify({ valid: errors.length === 0, errors, file_count: files.length }));
