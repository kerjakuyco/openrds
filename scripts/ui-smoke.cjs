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
async function api(resource, method = "GET", body) {
  const res = await fetch(`${base}/api/${resource}`, { method, headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" }, body: body && JSON.stringify(body) });
  assert.ok(res.ok, `${method} ${resource} returned ${res.status}`);
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
    await page.getByText("Connected", { exact: true }).waitFor();

    // Create through the sectioned form.
    await page.locator('nav#nav [data-nav="databases"]').click();
    await page.locator("#new-db").click();
    await page.getByRole("heading", { name: "Create database", level: 1 }).waitFor();
    await page.getByLabel("DB instance identifier").fill(name);
    await page.getByText("Dev and test", { exact: true }).click();
    await page.getByText("Custom", { exact: true }).click();
    await page.getByLabel("CPU limit (vCPU)").fill("0.5");
    await page.getByLabel("Memory limit (MiB)").fill("256");
    const quotas = (await api("host")).volumeQuota.state === "supported";
    const disk = page.getByLabel("Data volume limit (GiB)");
    await disk.fill(quotas ? "2" : "");
    await page.getByLabel("Host port", { exact: true }).fill(String(await freePort()));
    await page.getByLabel("Parameter to add").selectOption("max_connections");
    await page.getByRole("button", { name: "Add parameter" }).click();
    await page.getByLabel("max_connections value").fill("120");
    await page.getByRole("button", { name: "Add tag" }).click();
    await page.getByLabel("Tag key").fill("env");
    await page.getByLabel("Tag value").fill("ci");
    await page.getByRole("button", { name: "Create database" }).click();
    await page.waitForURL(new RegExp(`#/databases/${name}$`), { timeout: 60000 });
    await page.locator("h1 .status").filter({ hasText: "Running" }).waitFor({ timeout: 360000 });
    await page.getByText("0.5 vCPU · 256 MiB").first().waitFor();

    // Connectivity
    await page.getByRole("button", { name: "Reveal connection URI" }).click();
    assert.ok(/@127\.0\.0\.1:\d+\/app/.test(await page.locator("pre").filter({ hasText: "@127.0.0.1:" }).innerText()), "Expected a loopback connection URI");
    await page.locator("pre").filter({ hasText: `@openrds-${name}:5432/app` }).waitFor();

    // Monitoring renders four charts and their table view.
    await page.getByRole("tab", { name: "Monitoring" }).click();
    await page.getByRole("heading", { name: "CPU utilization" }).waitFor();
    assert.equal(await page.locator("figure.chart").count(), 4);
    await page.getByRole("button", { name: "Table" }).first().click();
    await page.locator(".chart-table").first().waitFor();

    // Logs, configuration and storage
    await page.getByRole("tab", { name: "Logs & events" }).click();
    await page.locator("pre.logs").filter({ hasText: "ready to accept connections" }).waitFor();
    await page.getByRole("tab", { name: "Configuration" }).click();
    const row = page.locator("tr").filter({ hasText: "max_connections" });
    await row.getByText("120", { exact: true }).waitFor();
    await row.getByText("Applied").waitFor();
    await page.getByRole("button", { name: "Measure volume usage" }).click();
    await page.getByRole("status").filter({ hasText: quotas ? "used of 2 GiB" : "used · uncapped" }).waitFor();

    // Snapshot from the maintenance tab
    await page.getByRole("tab", { name: "Maintenance & backups" }).click();
    await page.getByRole("button", { name: "Take snapshot" }).click();
    await page.getByText(`Snapshot of ${name} saved`, { exact: false }).waitFor({ timeout: 120000 });

    // Modify with a deferred apply, then reboot to apply it.
    await page.getByRole("link", { name: "Modify" }).first().click();
    await page.getByRole("heading", { name: `Modify ${name}` }).waitFor();
    await page.getByText("Small", { exact: true }).click();
    await page.getByRole("button", { name: "Continue" }).click();
    const review = page.getByRole("dialog");
    await review.getByRole("heading", { name: "Summary of modifications" }).waitFor();
    await review.getByRole("row").filter({ hasText: "CPU limit" }).waitFor();
    await review.getByRole("button", { name: "Modify database" }).click();
    await page.waitForURL(new RegExp(`#/databases/${name}$`));
    await page.getByText("Pending modifications apply at the next restart", { exact: false }).waitFor({ timeout: 30000 });
    await page.getByRole("button", { name: "Actions" }).click();
    await page.getByRole("menuitem", { name: "Reboot" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Reboot database" }).click();
    await page.getByText(`${name} rebooted.`, { exact: false }).waitFor({ timeout: 300000 });
    await page.getByText("Pending modifications apply at the next restart", { exact: false }).waitFor({ state: "detached", timeout: 30000 });
    await page.getByText("1 vCPU · 1 GiB").first().waitFor();

    // Inventory search
    await page.locator('nav#nav [data-nav="databases"]').click();
    await page.getByRole("heading", { name: "Databases", level: 1 }).waitFor();
    await page.keyboard.press("/");
    assert.equal(await page.evaluate(() => document.activeElement.id), "search");
    await page.locator("#search").fill("env=ci");
    await page.getByRole("link", { name: `Open ${name}` }).waitFor();
    assert.equal(await page.locator("#fleet-list tbody tr").count(), 1);

    // Restore form is prefilled from the snapshot.
    await page.locator('nav#nav [data-nav="snapshots"]').click();
    const snapshot = page.locator("tbody tr").filter({ hasText: name });
    await snapshot.getByRole("link", { name: /Restore the/ }).click();
    await page.getByRole("heading", { name: "Restore snapshot" }).waitFor();
    assert.equal(await page.getByLabel("DB instance identifier").inputValue(), `${name}-restore`);
    assert.equal(await disk.inputValue(), quotas ? "2" : "");
    assert.equal(await page.getByLabel("max_connections value").inputValue(), "120");
    await page.getByRole("link", { name: "Cancel" }).click();

    await page.locator('nav#nav [data-nav="events"]').click();
    await page.locator("#result-filter").selectOption("completed");
    await page.locator("#activity-list tr").filter({ hasText: name }).filter({ hasText: "Create snapshot" }).waitFor();

    await page.getByRole("button", { name: "Dark theme" }).click();
    assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), "dark");
    await page.getByRole("button", { name: "Match the system theme" }).click();

    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      for (const hash of ["#/", "#/databases", `#/databases/${name}`]) {
        await page.goto(`${base}/${hash}`);
        await page.getByText(name, { exact: true }).first().waitFor();
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `horizontal overflow at ${width}px on ${hash}`);
      }
      if (process.env.OPENRDS_SCREENSHOTS && width !== 320) {
        await page.screenshot({ path: path.join(process.env.OPENRDS_SCREENSHOTS, `openrds-${width}.png`), fullPage: true });
      }
    }
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.getByRole("button", { name: "Sign out" }).click();
    await page.getByRole("button", { name: "Sign in" }).waitFor();
    assert.deepEqual(errors, []);
    console.log("Browser: create with parameters and tags, connectivity, monitoring, logs, configuration, snapshot, deferred modify and reboot, search, restore form, events, themes, mobile layout and sign out passed.");
  } finally {
    await browser.close();
    for (const db of await api("instances")) if (db.name === name) await api(`instances/${name}`, "DELETE");
    for (const b of await api("backups")) if (b.name === name) await api(`backups/${b.id}`, "DELETE");
  }
}
run().catch(err => { console.error(err); process.exitCode = 1; });
