import assert from "node:assert/strict";
import { mkdtemp, rm, mkdir, writeFile, copyFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build, loadConfigFromFile, preview } from "vite";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
process.env.PLAYWRIGHT_BROWSERS_PATH = !process.env.PLAYWRIGHT_BROWSERS_PATH || process.env.PLAYWRIGHT_BROWSERS_PATH === ".pw-browsers"
  ? path.join(root, ".pw-browsers") : process.env.PLAYWRIGHT_BROWSERS_PATH;
const { chromium, _electron } = await import("playwright");
const electronEngine = process.argv.includes("--electron");
if (electronEngine) process.env.REASONIX_SHELL = "electron";
const output = await mkdtemp(path.join(tmpdir(), "reasonix-layout-build-"));
const evidence = process.env.REASONIX_LAYOUT_ARTIFACTS;
const samples = [];
let server;
let browser;
let electronApp;
try {
  const loaded = await loadConfigFromFile({ command: "build", mode: "production" }, path.join(root, "vite.config.ts"));
  // Build the real component fixture separately; do not alter the application's
  // dist, sourcemap archive or placeholder while running a regression test.
  const config = loaded.config;
  await build({ ...config, configFile: false, root, logLevel: "error",
    plugins: config.plugins.filter(plugin => !["archive-hidden-sourcemaps", "keep-dist-placeholder"].includes(plugin?.name)),
    build: { ...config.build, outDir: output, minify: false, sourcemap: false,
      rolldownOptions: { ...config.build.rolldownOptions, input: path.join(root, "bench/transcript-layout.html") } },
  });
  server = await preview({ configFile: false, root, logLevel: "error", build: { outDir: output },
    preview: { host: "127.0.0.1", port: 0 } });
  const address = server.httpServer.address();
  const url = `http://127.0.0.1:${address.port}/bench/transcript-layout.html`;
  let page;
  if (electronEngine) {
    const main = path.join(output, "main.cjs");
    await copyFile(path.join(root, "bench/transcript-layout-electron.cjs"), main);
    const electronRequire = createRequire(path.join(root, "../electron/package.json"));
    electronApp = await _electron.launch({
      executablePath: electronRequire("electron"), args: [main],
      env: { ...process.env, REASONIX_LAYOUT_URL: url },
    });
    page = await electronApp.firstWindow();
  } else {
    browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH } : {}) });
    page = await browser.newPage({ viewport: { width: 1920, height: 1080 } });
  }
  const setViewport = async size => {
    if (electronApp) await electronApp.evaluate(({ BrowserWindow }, size) => BrowserWindow.getAllWindows()[0].setContentSize(size.width, size.height), size);
    else await page.setViewportSize(size);
    await page.waitForFunction(size => Math.abs(innerWidth - size.width) <= 1 && Math.abs(innerHeight - size.height) <= 1, size);
  };
  const errors = [];
  page.on("pageerror", error => { errors.push(error.message); console.error(error.message); });
  await page.goto(url);
  await page.waitForSelector(".transcript__row .code").catch(async error => {
    console.error((await page.locator("body").innerText()).slice(0, 1200));
    throw error;
  });
  await page.evaluate(() => document.fonts.ready);
  const configure = async options => {
    const revision = await page.evaluate(options => {
      const fixture = window.transcriptLayoutFixture;
      const next = fixture.revision + 1;
      fixture.configure(options);
      return next;
    }, options);
    await page.waitForFunction(revision => window.transcriptLayoutFixture.revision === revision, revision);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  };
  const measure = async label => {
    const sample = await page.evaluate(() => {
      const rect = element => { const r = element.getBoundingClientRect(); return { left: r.left, right: r.right, width: r.width }; };
      const chat = document.querySelector(".chat-pane");
      const content = document.querySelector(".transcript-navigation-content");
      const transcript = document.querySelector(".transcript");
      const launcher = document.querySelector(".dock-launcher");
      return { viewport: innerWidth, chat: rect(chat), content: rect(content),
        documentWidth: document.documentElement.scrollWidth,
        dockOpen: document.querySelector(".layout").classList.contains("layout--workspace-open"),
        clientWidth: transcript.clientWidth, scrollWidth: transcript.scrollWidth,
        userBubbles: [...document.querySelectorAll(".msg--user .msg__body")].map(rect),
        launcher: launcher ? rect(launcher) : null };
    });
    samples.push({ label, ...sample });
    assert.ok(sample.content.right <= sample.chat.right + 1, label + ": content column fits chat");
    assert.ok(sample.documentWidth <= sample.viewport + 1, label + ": document stays inside viewport");
    assert.ok(sample.scrollWidth <= sample.clientWidth + 1, label + ": transcript has no horizontal overflow");
    for (const bubble of sample.userBubbles) {
      assert.ok(bubble.left >= sample.content.left - 1 && bubble.right <= sample.content.right + 1, label + ": complete user bubble fits");
    }
    if (sample.launcher) {
      assert.ok(sample.launcher.right <= sample.chat.right + 1, label + ": launcher fits");
      if (!sample.dockOpen) assert.ok(sample.content.right <= sample.launcher.left + 1, label + ": launcher and history do not overlap");
    }
    return sample;
  };
  // This first case is the original full-width disappearing-message regression.
  await measure("full-width long code");
  for (const layout of ["creation", "workbench"]) {
    for (const width of ["standard", "full"]) {
      for (const viewport of [760, 820, 1000, 1280, 1920]) {
        await setViewport({ width: viewport, height: 1080 });
        await configure({ layout, width, sidebar: true, dock: false, launcher: false });
        await measure(`${layout}/${width}/${viewport}`);
      }
    }
  }
  await setViewport({ width: 1920, height: 1080 });
  for (const sidebar of [false, true]) for (const dock of [false, true]) {
    await configure({ sidebar, dock, launcher: true });
    await measure(`sidebar=${sidebar}/dock=${dock}`);
  }
  await configure({ layout: "creation", sidebar: false, dock: false, launcher: true });
  for (const width of [1009, 1010, 1011]) {
    await setViewport({ width, height: 1080 });
    await page.waitForFunction(hidden => Boolean(document.querySelector(".dock-launcher")) !== hidden, width < 1010);
    await measure("launcher threshold " + width);
  }
  await configure({ launcher: false });
  await page.evaluate(() => {
    window.layoutMotion = { active: true, widths: [] };
    const sample = () => {
      const element = document.querySelector(".transcript-navigation-content");
      window.layoutMotion.widths.push(element.getBoundingClientRect().width);
      if (window.layoutMotion.active) requestAnimationFrame(sample);
    };
    requestAnimationFrame(sample);
  });
  for (let index = 0; index < 4; index++) {
    await configure({ long: index % 2 === 0 });
    await measure("content replacement " + index);
  }
  const motion = await page.evaluate(() => { window.layoutMotion.active = false; return window.layoutMotion.widths; });
  assert.ok(motion.length > 4 && Math.max(...motion) - Math.min(...motion) <= 1, "content changes never shift the column between frames");
  await configure({ turns: 120, long: true });
  await page.waitForSelector('[data-transcript-render-mode="windowed"]');
  await measure("windowed long session");
  await page.locator(".transcript").hover();
  let sawLongHistory = false;
  for (let index = 0; index < 16; index++) {
    await page.mouse.wheel(0, -1800);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await measure("windowed history scroll " + index);
    sawLongHistory ||= await page.locator(".transcript code").evaluateAll(elements => elements.some(element => element.textContent.includes("abcdefghij".repeat(60))));
    if (await page.locator(".transcript").evaluate(element => element.scrollTop <= 1)) break;
  }
  assert.ok(sawLongHistory, "long cold content enters the mounted window");
  await page.locator(".transcript__jump-bottom").click();
  await page.waitForFunction(() => {
    const transcript = document.querySelector(".transcript");
    return transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight <= 4;
  });
  await page.waitForFunction(() => ![...document.querySelectorAll(".transcript code")].some(element => element.textContent.includes("abcdefghij".repeat(60))));
  await measure("long cold content leaves the window");
  await configure({ turns: 1, text: "prefix ", streaming: true });
  await page.waitForSelector(".msg--assistant .md");
  const prose = "prefix " + "abcdefghij".repeat(60);
  await configure({ text: prose });
  await page.waitForSelector(".md--stream-tail");
  await measure("streaming long prose");
  await configure({ streaming: false });
  await page.waitForSelector(".md[data-markdown-blocks] p");
  await measure("completed long prose");
  const paragraph = await page.locator(".msg--assistant .md p").evaluate(element => ({
    width: element.clientWidth, scrollWidth: element.scrollWidth, text: element.textContent,
  }));
  assert.ok(paragraph.scrollWidth <= paragraph.width + 1, "completed prose wraps inside its own column");
  assert.equal(paragraph.text, prose, "wrapping never changes source text");
  await page.reload();
  await page.waitForSelector(".transcript__row .code");
  await configure({ turns: 1, text: prose });
  await page.waitForSelector(".md[data-markdown-blocks] p");
  await measure("fresh history long prose");
  await configure({ text: "| Key | Value |\n|---|---|\n| " + "abcdefghij".repeat(60) + " | data |\n\n$$\\sum_{n=1}^{10}n$$\n\n\`\`\`text\n" + "abcdefghij".repeat(60) + "\n\`\`\`" });
  await page.waitForSelector(".md .katex");
  await measure("wide table and math");
  const code = await page.locator(".md pre").first().evaluate(element => ({
    width: element.clientWidth, scrollWidth: element.scrollWidth, whitespace: getComputedStyle(element).whiteSpace,
  }));
  assert.equal(code.whitespace, "pre", "ordinary code retains its original lines");
  assert.ok(code.scrollWidth > code.width, "long code remains locally scrollable");
  assert.equal(await page.locator(".katex").first().evaluate(element => getComputedStyle(element).overflowWrap), "normal");
  await configure({ turns: 1, text: null, shell: true });
  // Creation groups completed shell calls. Open the visible parent first;
  // Playwright can otherwise scroll a clipped descendant inside a closed fold.
  const closedGroup = page.locator('.tool-group__head[aria-expanded="false"]');
  if (await closedGroup.count()) {
    await closedGroup.click();
    await page.locator(".tool-group__body").evaluate(async element => {
      await Promise.allSettled(element.getAnimations().map(animation => animation.finished));
    });
  }
  await page.waitForSelector(".tool__head");
  assert.equal(await page.locator(".tool__command").count(), 0, "collapsed command does not mount a viewer");
  await page.locator(".tool__head").click();
  await page.waitForSelector(".tool__command .code");
  await page.locator(".tool__command").evaluate(async element => {
    const animations = [];
    for (let parent = element; parent; parent = parent.parentElement) {
      animations.push(...parent.getAnimations().filter(animation => Number.isFinite(animation.effect?.getComputedTiming().endTime)));
    }
    await Promise.allSettled(animations.map(animation => animation.finished));
  });
  await measure("expanded shell command");
  const commandBox = await page.locator(".tool__command pre").evaluate(element => ({
    width: element.clientWidth, scrollWidth: element.scrollWidth, height: element.clientHeight,
    whitespace: getComputedStyle(element).whiteSpace, text: element.textContent,
  }));
  assert.equal(commandBox.whitespace, "pre-wrap");
  assert.ok(commandBox.scrollWidth <= commandBox.width + 1 && commandBox.height <= 240, "complete command wraps in a bounded viewport");
  assert.ok(commandBox.text.includes("END_OF_COMMAND"), "command tail is mounted without truncation");
  const commandAncestors = await page.locator(".tool__command pre").evaluate(element => {
    const rows = [];
    for (let parent = element; parent; parent = parent.parentElement) {
      const rect = parent.getBoundingClientRect();
      rows.push({ className: parent.className, top: rect.top, bottom: rect.bottom, height: rect.height,
        clientHeight: parent.clientHeight, scrollHeight: parent.scrollHeight, scrollTop: parent.scrollTop,
        overflowY: getComputedStyle(parent).overflowY, style: parent.getAttribute("style") });
    }
    return rows;
  });
  if (evidence) { await mkdir(evidence, { recursive: true }); await writeFile(path.join(evidence, "command-ancestors.json"), JSON.stringify(commandAncestors, null, 2)); }
  for (const ancestor of commandAncestors.slice(1)) {
    if (ancestor.overflowY !== "visible") {
      assert.ok(ancestor.top <= commandAncestors[0].top + 1 && ancestor.bottom >= commandAncestors[0].bottom - 1,
        "expanded command is not clipped by " + ancestor.className);
    }
  }
  const outputView = page.locator(".tool__body .code-block").filter({ hasText: "OUTPUT_STAYS_VISIBLE" });
  assert.equal(await outputView.locator("pre").evaluate(element => getComputedStyle(element).whiteSpace), "pre", "output preserves its original lines");
  await page.locator(".tool__command pre").hover();
  await page.mouse.wheel(0, 800);
  await page.waitForFunction(() => {
    const pre = document.querySelector(".tool__command pre");
    return pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 2;
  });
  const tailVisible = await page.locator(".tool__command pre").evaluate(pre => {
    const walker = document.createTreeWalker(pre.querySelector("code"), NodeFilter.SHOW_TEXT);
    const nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    let remaining = "END_OF_COMMAND".length;
    const range = document.createRange();
    range.setEnd(nodes.at(-1), nodes.at(-1).textContent.length);
    for (const node of nodes.toReversed()) {
      if (node.textContent.length >= remaining) { range.setStart(node, node.textContent.length - remaining); break; }
      remaining -= node.textContent.length;
    }
    const bounds = pre.getBoundingClientRect();
    return range.toString() === "END_OF_COMMAND" && [...range.getClientRects()].every(rect =>
      rect.left >= bounds.left - 1 && rect.right <= bounds.right + 1 && rect.top >= bounds.top - 1 && rect.bottom <= bounds.bottom + 1);
  });
  assert.ok(tailVisible, "native nested scrolling exposes the complete command's last characters");
  await page.locator(".tool__command").hover();
  await page.locator(".tool__command .copybtn").click();
  await page.waitForFunction(() => window.transcriptLayoutFixture.copied.length === 1);
  assert.equal(await page.evaluate(() => window.transcriptLayoutFixture.copied[0]), await page.evaluate(() => window.transcriptLayoutFixture.command), "copy preserves the original command");
  if (evidence) { await mkdir(evidence, { recursive: true }); await page.screenshot({ path: path.join(evidence, "command.png") }); }
  await page.locator(".tool__head").click();
  assert.equal(await page.locator(".tool__command").count(), 0);
  if (electronApp) {
    await setViewport({ width: 1280, height: 900 });
    await configure({ shell: false, text: null, turns: 2, long: true });
    for (const factor of [0.8, 1, 1.25]) {
      await electronApp.evaluate(({ BrowserWindow }, factor) => BrowserWindow.getAllWindows()[0].webContents.setZoomFactor(factor), factor);
      await page.waitForFunction(factor => Math.abs(innerWidth - 1280 / factor) <= 1, factor);
      await measure("native Electron zoom " + factor);
    }
  }
  assert.deepEqual(errors, []);
  console.log(`PASS transcript width (${electronEngine ? "Electron" : "Chromium"}): ${samples.length} geometry scenarios`);
  if (evidence) {
    await mkdir(evidence, { recursive: true });
    await writeFile(path.join(evidence, "environment.json"), JSON.stringify({
      engine: electronEngine ? "electron" : "chromium", platform: process.platform,
      versions: electronApp ? await electronApp.evaluate(() => process.versions) : { chromium: browser.version() },
      sourceCommit: JSON.parse(config.define.__BUILD_COMMIT__),
    }, null, 2));
  }
  if (evidence) { await mkdir(evidence, { recursive: true }); await page.screenshot({ path: path.join(evidence, "layout.png") }); }
} finally {
  if (evidence) { await mkdir(evidence, { recursive: true }); await writeFile(path.join(evidence, "layout.json"), JSON.stringify(samples, null, 2)); }
  await browser?.close();
  await electronApp?.close();
  await server?.httpServer.close();
  await rm(output, { recursive: true, force: true });
}
