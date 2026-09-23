// 控制台的行内样式必须走 CSSOM：控制台 CSP 的 `style-src 'self'` 会把 style **属性**整条丢掉。
//
// 现场事故（2026-09-23，用户截图「为什么这个组织树没有缩进？」）：tree.js 的缩进写成
// `style: 'padding-left:' + …`，而 el() 用 setAttribute('style', …) 落地这个属性。控制台发的是
//
//   default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; …
//
// 而 style **属性**归 `style-src-attr` 管（没有它时回落到 `style-src`）：没有 'unsafe-inline'，
// 浏览器就把声明整条丢弃。表现是最难查的那种——DOM 里 row 上写着 `style="padding-left:52px"`，
// `getComputedStyle(row).paddingLeft` 却是 `0px`，每一层都贴着左边缘。所以 d14d0a4 那次「子节点
// 缩进 14px→22px」在线上不可能有肉眼变化：丢的不是距离，是整条样式。
//
// 这个文件在**没有浏览器**的地方守住三件事（几何本身由 scripts/ui-harness 的 `csp` 视图在真实
// 策略下量，本环境跑不了浏览器，见 README「前三个坑」）：
//
//   ① el() 处理 `style` 键时必须走 `node.style.cssText`（CSSOM），而不是 setAttribute；
//   ② 控制台静态资源里不允许再出现任何写 style 属性的写法（`setAttribute('style', …)`）；
//   ③ 控制台策略仍然没有 'unsafe-inline'，且 harness 里那份副本与它**逐字相同** ——
//      harness 是几何断言的执行环境；它的策略一松、一漂移，那些断言就不再证明线上行为。
//
// ①/② 是这次事故的直接形状。③ 是"为什么以前全绿"的答案：harness 从不发 CSP。
import assert from 'node:assert/strict';
import { readdir, readFile } from 'node:fs/promises';

const root = new URL('../../..', import.meta.url); // 仓库根
const read = (rel) => readFile(new URL(rel, root), 'utf8');

// --- ① el()：style 走 CSSOM ---------------------------------------------------------------

const uiSrc = await read('internal/webui/static/js/ui.js');
assert.match(uiSrc, /else if \(key === 'style'\) node\.style\.cssText = value;/,
  "el() 必须用 node.style.cssText 落 style：setAttribute('style', …) 在 style-src 'self' 下会被整条丢弃");
assert.doesNotMatch(uiSrc, /setAttribute\(\s*['"]style['"]/,
  'el() 不允许再有写 style 属性的路径：那正是组织树丢缩进的方式');

// --- ② 整个控制台都不许写 style 属性 ------------------------------------------------------

// 递归列出静态资源；`static/` 之外的东西（如 Go 里的 CSP 头）不在这一条的范围内。
async function jsFiles(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const url = new URL(entry.name + (entry.isDirectory() ? '/' : ''), dir);
    if (entry.isDirectory()) out.push(...await jsFiles(url));
    else if (entry.name.endsWith('.js')) out.push(url);
  }
  return out;
}

const staticRoot = new URL('internal/webui/static/', root);
const offenders = [];
for (const url of await jsFiles(new URL('js/', staticRoot))) {
  const src = await readFile(url, 'utf8');
  if (/setAttribute\(\s*['"]style['"]/.test(src)) offenders.push(new URL(url).pathname);
}
assert.deepEqual(offenders, [],
  '控制台里不允许出现写 style 属性的代码（CSP style-src \'self\' 会丢弃它）：应该交给 el() 的 style 键');
// 样式本身归 app.css：这条只保证"行内样式只从 el() 这一条路进 DOM"，不评价有多少处行内样式。
assert.match(uiSrc, /\{ class: 'modal', style: /,
  '弹窗宽度等行内样式仍应经 el() 的 style 键（它们与控制台的严格 CSP 是同一条通路）');
// 另有两处"属性透传"辅助函数**不认** style 键：router.js 的 el2（建侧边栏链接）与 chart.js 的 svg
// （建 SVG 几何），它们把 attrs 直接 setAttribute 下去。给它们传 style 会同样被 CSP 丢弃，所以 SVG 的
// 样式必须走 CSSOM（chart.js 现在正是 `element.style.background` / `style.display`）。这条钉住
// "svg() 不接 style 键"，免得将来有人把它当成 el() 用。
const chartSrc = await read('internal/webui/static/js/chart.js');
assert.doesNotMatch(chartSrc, /style:\s*['"`]/,
  'chart.js 的 svg() 不认 style 键：SVG 样式请走 element.style.*（CSSOM），否则会被 CSP 丢弃');

// --- ③ 控制台策略与 harness 副本：没有 'unsafe-inline'，且逐字相同 ------------------------

const embed = await read('internal/webui/embed.go');
const policies = [...embed.matchAll(/Content-Security-Policy", "([^"]+)"/g)].map((m) => m[1]);
assert.ok(policies.length >= 2, 'embed.go 里应当同时给 shell 与 .html 资源发同一条策略');
for (const policy of policies) {
  assert.equal(policy, policies[0], '两处策略头必须逐字相同：一处松一处紧会让行为按响应类型漂移');
  assert.match(policy, /style-src 'self'/,
    "策略里必须保留 style-src 'self'（本次修法选择修 CSSOM 通路，而不是放开策略）");
}
assert.doesNotMatch(policies[0], /unsafe-inline/,
  "控制台策略里不允许出现 'unsafe-inline'：style 属性会被它放回来，脚本与样式的注入面一起变宽");

const harnessServer = await read('scripts/ui-harness/server.py');
const harnessPolicy = harnessServer.match(/CONSOLE_CSP = "([^"]+)"/);
assert.ok(harnessPolicy, 'harness 的 server.py 必须有一份控制台策略的副本（csp 视图靠它才有意义）');
assert.equal(harnessPolicy[1], policies[0],
  'harness 里的策略副本必须与控制台的逐字相同：漂移了，csp 视图就在量另一条策略下的行为');

// --- ④ harness 的 csp 页面自身必须能在严格策略下跑 -----------------------------------------

// 那一页由 server.py 带着真实策略送出：内联脚本/样式在它下面本来就会被拦掉，所以页面里不能有
// （拦掉之后报告发不出来，视图会以"no report"失败——看起来像页面坏了，其实是页面写错了）。
const cspPage = await read('scripts/ui-harness/csp.page.html');
assert.doesNotMatch(cspPage, /<script(?![^>]*\ssrc=)/i, 'csp 页面不能有内联脚本');
assert.doesNotMatch(cspPage, /<style[\s>]/i, 'csp 页面不能有内联 <style>');
assert.doesNotMatch(cspPage, /\sstyle="/i, 'csp 页面不能有 style 属性');
assert.match(cspPage, /<script type="module" src="\/csp\.js"><\/script>/,
  'csp 页面的脚本必须是同源外部模块（script-src \'self\' 才放行）');

// 视图名必须在 runner 的清单里，否则它永远不会被跑到（"加了断言但没人执行"的经典失效）。
const runSh = await read('scripts/ui-harness/run.sh');
assert.match(runSh, /(^|\s)csp(\s|$)/m, 'csp 视图必须挂进 run.sh 的 VIEWS 清单');
assert.match(runSh, /csp\) echo "csp\.html"/, 'csp 视图必须映射到 csp.html');

console.log('Console inline-style (CSSOM) checks passed.');
