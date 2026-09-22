#!/usr/bin/env python3
"""Generate internal/webui/static/js/pinyin.js — the hanzi→pinyin table the console filters with.

Why a generated file at all
---------------------------
The console is zero-build native ES modules served straight out of the binary (see
docs/design/m9-web-console.md), so it cannot `import` an npm package: whatever it needs has to
be a file in internal/webui/static/. Pinyin matching needs a character table, and a table is
data, so it is generated once and committed. `make verify` does not run this script.

Where the data comes from
-------------------------
mozillazg/pinyin-data (MIT). The header of the generated file records the source URL, the
version and the SHA-256 of the exact file used, so a regeneration is reproducible and a silent
data swap is visible in review.

Regenerate with:

    python3 scripts/gen-pinyin.py                 # downloads the pinned version
    python3 scripts/gen-pinyin.py --source FILE   # or from a local copy

Coverage
--------
The CJK Unified Ideographs block (U+4E00–U+9FFF). Everything outside it falls back to plain
case-insensitive substring matching in the console — that is a documented limitation, not a
silent one. Tones are stripped (an operator types "zhangsan", never "zhāngsān") and ü is
written as v, which is what the pinyin input methods use.
"""
import argparse
import hashlib
import re
import unicodedata
import urllib.request

SOURCE_URL = "https://raw.githubusercontent.com/mozillazg/pinyin-data/master/pinyin.txt"
SOURCE_VERSION = "0.15.0"
OUT = "internal/webui/static/js/pinyin.js"
# The same table, for the Go side (M74: the tenant name of an account is derived from the pinyin of
# its name). Two files because the two consumers cannot share one: the console is a zero-build ES
# module tree that can only import files under static/, and the release copy of static/ is minified
# by `make build`, so reading the table back out of a build artifact is not an option.
OUT_GO = "internal/pinyin/table_gen.go"

# Tone-stripped readings are compared as typed, so the table is normalised once here rather
# than on every keystroke in the browser.
RANGE_LO, RANGE_HI = 0x4E00, 0x9FFF


def strip_tone(reading: str) -> str:
    decomposed = unicodedata.normalize("NFD", reading.strip().lower())
    plain = "".join(ch for ch in decomposed if unicodedata.category(ch) != "Mn")
    # NFD splits "lü" into "lu" + a combining diaeresis, and dropping the mark leaves "lu" — 女 (nü)
    # therefore lands on the same string as 路 (lù), and the table contains no "v" at all. That is a
    # documented limitation (docs/org.md), not a silent one; the replaces below only catch a source
    # that spells ü as "u:" instead of as the character.
    return plain.replace("ü", "v").replace("u:", "v")


def parse(source: str) -> dict[int, list[str]]:
    readings: dict[int, list[str]] = {}
    for line in source.splitlines():
        match = re.match(r"^U\+([0-9A-F]+):\s*([^#]+)", line)
        if not match:
            continue
        codepoint = int(match.group(1), 16)
        values: list[str] = []
        for raw in match.group(2).split(","):
            value = strip_tone(raw)
            if value and value.isalpha() and value not in values:
                values.append(value)
        if values:
            readings[codepoint] = values
    return readings


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", help="local pinyin.txt instead of downloading")
    args = parser.parse_args()

    if args.source:
        raw = open(args.source, "rb").read()
    else:
        with urllib.request.urlopen(SOURCE_URL, timeout=60) as response:
            raw = response.read()
    digest = hashlib.sha256(raw).hexdigest()
    readings = parse(raw.decode("utf-8"))
    covered = {cp: rs for cp, rs in readings.items() if RANGE_LO <= cp <= RANGE_HI}

    # One record per codepoint in the block, newline-separated; an empty record means "no
    # reading". The console splits this once, lazily, on the first filter keystroke.
    blob = "\n".join(
        ",".join(covered.get(cp, [])) for cp in range(RANGE_LO, RANGE_HI + 1)
    )

    body = f"""// 汉字→拼音表（生成文件，请勿手改；改表请改 scripts/gen-pinyin.py 并重新生成）。
//
// 来源：mozillazg/pinyin-data（MIT 许可）
//   {SOURCE_URL}
//   版本 {SOURCE_VERSION}，SHA-256 {digest}
// 覆盖：CJK 统一汉字基本区 U+{RANGE_LO:04X}–U+{RANGE_HI:04X}（{len(covered)} 字有声母读音）。
// 表外的字符不做拼音匹配，只按字面子串匹配——这条限制写在 docs/org.md 里。
//
// 为什么是生成的文件：控制台源码是**零构建**的原生 ES 模块（见 docs/design/m9-web-console.md），
// 不能 import npm 包，需要的东西必须是 internal/webui/static/ 下的文件；而表是数据，
// 生成一次提交进来即可（`make verify` 不会重新生成它）。
// 注意：发布二进制里的副本在 `make build` 时会被压缩混淆（注释头随之消失，M50），
// 因此**本文件才是出处记录**（版本与 SHA-256 都在这里），别指望从产物里读回来。
//
// 声调已剥离；变音符号一并去掉，所以 ü 落在 u 上（女 → nu，表里没有 v）：
// 操作员打的是 "zhangsan"，不会打 "zhāngsān"。

// 读音表：每行一个字（按 U+{RANGE_LO:04X} 起顺序），多音字用逗号分隔，空行表示该字无读音。
const READINGS = {js_string(blob)};

const RANGE_START = 0x{RANGE_LO:04X};
let cache = null;

// table lazily splits the blob. It is only reached when someone types in a filter, so a
// session that never filters never pays for parsing ~{len(blob) // 1024}K of text.
function table() {{
  if (!cache) cache = READINGS.split('\\n');
  return cache;
}}

// readingsOf returns the tone-stripped readings of one character, or null when the table has
// none (a non-CJK character, or a rare ideograph outside the covered block).
export function readingsOf(ch) {{
  const cp = ch.codePointAt(0);
  if (cp < RANGE_START || cp - RANGE_START >= table().length) return null;
  const row = table()[cp - RANGE_START];
  return row ? row.split(',') : null;
}}

// candidates returns every lowercase string a query may be matched against for `text`:
// the text itself, its full pinyin, and its initials.
//
// Polyphones are handled by enumerating the readings of each character, but only up to
// MAX_COMBINATIONS of them: 长 is both chang and zhang, so both "changwei" and "zhangwei" find
// 长伟 — while a long name full of polyphones degrades to its most common reading instead of
// exploding into hundreds of strings on every keystroke.
const MAX_COMBINATIONS = 64;
export function candidates(text) {{
  const value = String(text || '').toLowerCase();
  const out = [value];
  const groups = [];
  let combinations = 1;
  for (const ch of value) {{
    const readings = readingsOf(ch);
    if (!readings) {{
      // ASCII (an English account name), a digit, a separator: it passes through unchanged so
      // "dev-key" and "研发-1" both still match by their own characters.
      groups.push([ch]);
      continue;
    }}
    groups.push(readings);
    combinations *= readings.length;
  }}
  if (!groups.some((group) => group.length > 1 || group[0].length > 1)) return out;

  const enumerate = (source) => {{
    const full = new Set();
    const initials = new Set();
    const walk = (index, word, initial) => {{
      if (full.size + initials.size > MAX_COMBINATIONS * 2) return;
      if (index === source.length) {{
        full.add(word);
        initials.add(initial);
        return;
      }}
      for (const reading of source[index]) walk(index + 1, word + reading, initial + reading[0]);
    }};
    let total = 1;
    for (const group of source) total *= group.length;
    // Only the primary reading when the name is long: the enumeration above is exponential.
    if (total <= MAX_COMBINATIONS) walk(0, '', '');
    else {{
      full.add(source.map((group) => group[0]).join(''));
      initials.add(source.map((group) => group[0][0]).join(''));
    }}
    return [full, initials];
  }};

  // Two walks, because operators type both ways for a name like 研发-张三: with the separator
  // ("dev-zs", "研发-zhangsan") and without it ("devzs", "yanfazhangsan"). Dropping the
  // separator is what makes initials usable on a hyphenated or spaced name.
  const passes = [groups];
  const flattened = groups.filter((group) => !/^[^0-9a-z]+$/.test(group[0]));
  if (flattened.length !== groups.length) passes.push(flattened);
  for (const source of passes) {{
    const [full, initials] = enumerate(source);
    for (const value of full) if (value !== '') out.push(value);
    for (const value of initials) if (value !== '') out.push(value);
  }}
  return out;
}}

// matchesQuery reports whether `text` should be shown for a filter box containing `query`.
//
// The order is deliberate: a literal match (English names, digits, or Chinese typed directly)
// answers first and cheaply; only then is the pinyin table consulted.
export function matchesQuery(text, query) {{
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return true;
  if (String(text || '').toLowerCase().includes(needle)) return true;
  return candidates(text).some((candidate) => candidate.includes(needle));
}}
"""
    with open(OUT, "w", encoding="utf-8") as handle:
        handle.write(body)
    go = go_body(blob, digest, len(covered))
    with open(OUT_GO, "w", encoding="utf-8") as handle:
        handle.write(go)
    print(f"wrote {OUT}: {len(covered)} characters, blob {len(blob)} bytes, sha256(source) {digest[:12]}")
    print(f"wrote {OUT_GO}: the same blob")
    return 0


def go_body(blob: str, digest: str, characters: int) -> str:
    """Render the same table as a Go source file.

    The blob is deliberately identical to the JS one (same line order, same codepoint range), and
    internal/pinyin/pinyin_test.go asserts byte equality: a table that is only regenerated on one
    side would otherwise let the console filter with 陈→chen while the gateway names a tenant from
    a reading that is no longer in the file.
    """
    return f"""// Code generated by scripts/gen-pinyin.py; DO NOT EDIT.
//
// 汉字→拼音表（与控制台过滤用的是同一张表：{OUT}）。
//
// 来源：mozillazg/pinyin-data（MIT 许可）
// {SOURCE_URL}
// 版本 {SOURCE_VERSION}，SHA-256 {digest}
// 覆盖：CJK 统一汉字基本区 U+{RANGE_LO:04X}–U+{RANGE_HI:04X}（{characters} 字有声母读音）；
// 每行一个字（按码点顺序），多音字用逗号分隔，空行表示该字无读音。
// 声调已剥离；变音符号一并去掉，所以 ü 落在 u 上（女 → nu）。
package pinyin

// readingsBlob is the table above, one '\\n'-separated record per codepoint from U+{RANGE_LO:04X}.
const readingsBlob = {js_string(blob)}
"""


def js_string(value: str) -> str:
    """Render the blob as a JS string literal.

    JSON's escaping is valid JavaScript, and it leaves every CJK character readable in the
    file (no \\uXXXX noise), which is what makes a spot check of the table possible at all.
    """
    import json
    return json.dumps(value, ensure_ascii=False)


if __name__ == "__main__":
    raise SystemExit(main())
