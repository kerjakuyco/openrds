// Run against an installed test workspace with playwright-core on NODE_PATH.
const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { randomBytes } = require("node:crypto");
const { createServer } = require("node:net");
const path = require("node:path");
const { chromium } = require("playwright-core");

const compose = (...args) => execFileSync("docker", ["compose", ...args], { encoding: "utf8" }).trim();
const base = `http://${compose("port", "openrds", "8080")}`;
const token = compose("exec", "-T", "openrds", "cat", "/var/lib/openrds/admin-token");
const name = `ui-${randomBytes(5).toString("hex")}`;
async function api(resource, method = "GET") {
  const res = await fetch(`${base}/api/${resource}`, { method, headers: { Authorization: `Bearer ${token}` } });
  assert.ok(res.ok, `cleanup ${method} ${resource} returned ${res.status}`);
  return res.status === 204 ? null : res.json();
}
async function freePort() {
  const s = createServer();
  await new Promise((resolve, reject) => s.once("error", reject).listen(0, "127.0.0.1", resolve));
  const port = s.address().port;
  await new Promise(resolve => s.close(resolve));
  return port;
}
async function run() {
  const browser = await chromium.launch({ channel: process.env.BROWSER_CHANNEL || "chrome" });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 960 } });
    const errors = [];
    page.on("pageerror", err => errors.push(err.message));
    await page.goto(base);
    await page.getByLabel("Admin token", { exact: true }).fill(token);
    await page.getByRole("button", { name: "Sign in" }).click();
    await page.getByText("Host connected", { exact: true }).waitFor();
    await page.locator("#new-db").click();
    await page.getByLabel("Instance name").fill(name);
    await page.getByLabel("Host port", { exact: true }).fill(String(await freePort()));
    await page.getByText("Resource limits", { exact: false }).first().click();
    await page.getByLabel("CPU cores").fill("0.5");
    await page.getByLabel("Memory (MiB)").fill("256");
    const quotas = (await api("host")).volumeQuota.state === "supported";
    const disk = page.getByLabel("Data volume (GiB)", { exact: true });
    assert.equal(await disk.isEnabled(), true);
    await disk.fill("20");
    assert.equal(await disk.inputValue(), "20");
    await disk.fill(quotas ? "2" : "");
    await page.locator("#editor-submit").click();
    const detail = page.locator("#detail");
    await detail.waitFor({ state: "visible", timeout: 360000 });
    await detail.getByText("0.5 cores").waitFor();
    await detail.getByText("Data volume limit", { exact: true }).waitFor();
    await detail.getByRole("button", { name: "Measure volume usage" }).click();
    await detail.getByRole("status").filter({ hasText: quotas ? "used of 2 GiB" : "used · uncapped" }).waitFor();
    await detail.getByText("Memory / limit").waitFor();
    await detail.getByRole("button", { name: "Connection", exact: true }).click();
    await detail.getByRole("button", { name: "Reveal connection URI" }).click();
    assert.ok(/@127\.0\.0\.1:\d+\/app/.test(await detail.locator("pre").filter({ hasText: "@127.0.0.1:" }).innerText()), "Expected a loopback connection URI");
    await detail.getByText("Connect an application container", { exact: true }).click();
    await detail.locator("pre").filter({ hasText: `@openrds-${name}:5432/app` }).waitFor();
    await detail.getByRole("button", { name: "Logs", exact: true }).click();
    await detail.locator("pre").filter({ hasText: "ready to accept connections" }).waitFor();
    await detail.getByRole("button", { name: "Overview", exact: true }).click();
    await detail.getByRole("button", { name: "Create backup" }).click();
    await detail.getByText(`Backup saved for ${name}.`, { exact: false }).waitFor();
    await detail.getByRole("button", { name: "Close database details" }).click();

    await page.getByRole("link", { name: "Databases", exact: true }).click();
    await page.keyboard.press("/");
    assert.equal(await page.evaluate(() => document.activeElement.id), "search");
    await page.locator("#search").fill(name);
    await page.getByRole("button", { name: `Inspect ${name}` }).waitFor();
    assert.equal(await page.locator("#fleet-list tbody tr").count(), 1);
    await page.getByRole("link", { name: "Backups", exact: true }).click();
    const backup = page.locator("#backup-list tr").filter({ hasText: name });
    await backup.getByRole("button", { name: "Restore" }).click();
    assert.equal(await page.getByLabel("Instance name").inputValue(), `${name}-restore`);
    assert.equal(await page.getByLabel("Engine", { exact: true }).isDisabled(), true);
    assert.equal(await disk.inputValue(), quotas ? "2" : "");
    await page.locator("#editor").getByRole("button", { name: "Cancel" }).click();
    await page.getByRole("link", { name: "Activity", exact: true }).click();
    await page.locator("#result-filter").selectOption("completed");
    await page.locator("#activity-list tr").filter({ hasText: name }).filter({ hasText: "Create backup" }).waitFor();

    await page.getByRole("link", { name: "Overview", exact: true }).click();
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `horizontal overflow at ${width}px`);
      const inspect = await page.getByRole("button", { name: `Inspect ${name}` }).boundingBox();
      assert.ok(inspect && inspect.x >= 0 && inspect.x + inspect.width <= width, `Inspect is off-screen at ${width}px`);
      if (process.env.OPENRDS_SCREENSHOTS && width !== 320) {
        await page.screenshot({ path: path.join(process.env.OPENRDS_SCREENSHOTS, `openrds-${width}.png`), fullPage: true });
      }
    }
    await page.getByRole("button", { name: "Sign out" }).click();
    await page.getByRole("button", { name: "Sign in" }).waitFor();
    assert.deepEqual(errors, []);
    console.log("Browser: sign in, provision, metrics, connection, logs, backup, filters, restore dialog, mobile layout and sign out passed.");
  } finally {
    await browser.close();
    for (const db of await api("instances")) if (db.name === name) await api(`instances/${name}`, "DELETE");
    for (const b of await api("backups")) if (b.name === name) await api(`backups/${b.id}`, "DELETE");
  }
}
run().catch(err => { console.error(err); process.exitCode = 1; });
