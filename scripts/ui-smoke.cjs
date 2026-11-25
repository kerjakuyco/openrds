// Run against an installed test workspace with playwright-core on NODE_PATH.
const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { randomBytes } = require("node:crypto");
const { createServer } = require("node:net");
const path = require("node:path");
const { chromium } = require("playwright-core");

const compose = (...args) => execFileSync("docker", ["compose", ...args], { encoding: "utf8" }).trim();
const base = `http://${compose("port", "openrds", "8080")}`;
const token = compose("exec", "-T", "openrds", "openrds", "api-key");
const name = `ui-${randomBytes(5).toString("hex")}`;
const admin = { username: `ci-${randomBytes(4).toString("hex")}`, password: randomBytes(12).toString("hex") };
const viewer = { username: `${admin.username}-v`, password: randomBytes(12).toString("hex") };
async function api(resource, method = "GET", body) { // authenticated with the API key
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
    // A fresh install asks for the administrator account straight away. If
    // accounts already exist, sign in through an invite made with the API key.
    await page.goto(base);
    const setup = page.getByRole("heading", { name: "Create your administrator account" });
    const signIn = page.getByRole("heading", { name: "Open your workspace" });
    await setup.or(signIn).waitFor();
    assert.equal(await page.getByLabel("Admin token").count(), 0, "setup must not ask for a token");
    if (await setup.isVisible()) {
      await page.getByLabel("Your name").fill("CI Administrator");
      await page.getByLabel("Username", { exact: true }).fill(admin.username);
      await page.getByLabel("Password", { exact: true }).fill(admin.password);
      await page.getByLabel("Confirm password", { exact: true }).fill(admin.password);
      await page.getByRole("button", { name: "Create account and continue" }).click();
    } else {
      const invite = await api("users", "POST", { username: admin.username, name: "CI Administrator", role: "admin" });
      await page.goto(`${base}/#/invite/${invite.secret}`);
      await page.getByLabel("Choose a password", { exact: true }).fill(admin.password);
      await page.getByLabel("Confirm password", { exact: true }).fill(admin.password);
      await page.getByRole("button", { name: "Create my account" }).click();
    }
    await page.locator(".dash-hero h1").waitFor();
    // A new server offers its engines; with databases, the next step instead.
    const firstRun = page.getByRole("heading", { name: "Create your first database" });
    await firstRun.or(page.getByLabel("Next step")).first().waitFor();

    // Create through the sectioned form, starting from the dashboard's engine picker when it is there.
    if (await firstRun.isVisible()) {
      await page.getByRole("button", { name: /^PostgreSQL/ }).click();
    } else {
      await page.locator('nav#nav [data-nav="databases"]').click();
      await page.locator("#new-db").click();
    }
    await page.getByRole("heading", { name: "Create database", level: 1 }).waitFor();
    await page.getByLabel("DB instance identifier").fill(name);
    await page.getByText("Dev and test", { exact: true }).click();
    await page.getByText("Custom", { exact: true }).click();
    await page.getByLabel("CPU limit (vCPU)").fill("0.5");
    await page.getByLabel("Memory limit (MiB)").fill("256");
    const quotas = (await api("host")).volumeQuota.state === "supported";
    const disk = page.getByRole("spinbutton", { name: /^Data volume limit \(GiB\)/ });
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

    // Connection guide: one string to copy; show the password, then open the code examples.
    const conn = page.locator(".conn-string pre");
    await conn.filter({ hasText: "openrds:••••••••@" }).waitFor();
    await page.getByRole("button", { name: "Show password" }).click();
    const uri = await conn.filter({ hasText: /openrds:[a-f0-9]{48}@/ }).innerText();
    assert.ok(/^postgresql:\/\/openrds:[a-f0-9]{48}@127\.0\.0\.1:\d+\/app$/.test(uri), `Expected a loopback URI with the password, got ${uri}`);
    await page.getByText("Code examples and options").click();
    await page.getByLabel("Language or tool").selectOption("node");
    await page.locator("pre").filter({ hasText: "new pg.Pool" }).waitFor();

    // Connection pooling: turned on in place, with its own connection string.
    await page.getByRole("button", { name: "Turn on pooling" }).click();
    await page.getByRole("dialog").getByText("Each transaction borrows", { exact: false }).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "Turn on" }).click();
    await page.getByText("Through the connection pooler").waitFor({ timeout: 60000 });
    await page.locator("code").filter({ hasText: `openrds-${name}-pooler:6432` }).waitFor();

    // Database users: create a read-only user and see its password once,
    // change the owner password, then delete the user.
    await page.getByRole("tab", { name: "Database users" }).click();
    await page.getByRole("button", { name: "Create user" }).click();
    let dialog = page.getByRole("dialog");
    await dialog.getByLabel("Username").fill("reporting");
    await dialog.getByText("Read only", { exact: true }).click();
    await dialog.getByRole("button", { name: "Create user" }).click();
    dialog = page.getByRole("dialog").filter({ hasText: "reporting is ready" });
    await dialog.locator("pre").filter({ hasText: /^[a-f0-9]{48}$/ }).waitFor();
    await dialog.getByRole("button", { name: "Done" }).click();
    const reporting = page.locator("tbody tr").filter({ hasText: "reporting" });
    await reporting.filter({ hasText: "Read only" }).waitFor();
    await page.locator("tbody tr").filter({ hasText: "Owner" }).getByRole("button", { name: "Actions" }).click();
    await page.getByRole("menuitem", { name: "Change password" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Change password" }).click();
    await page.getByRole("dialog").filter({ hasText: "New password for openrds" }).getByRole("button", { name: "Done" }).click();
    await reporting.getByRole("button", { name: "Actions" }).click();
    await page.getByRole("menuitem", { name: "Delete user" }).click();
    dialog = page.getByRole("dialog");
    await dialog.getByPlaceholder("reporting").fill("reporting");
    await dialog.getByRole("button", { name: "Delete user" }).click();
    await reporting.waitFor({ state: "detached" });

    // Network access: open a public port for one address, then close it.
    await page.getByRole("tab", { name: "Connectivity & security" }).click();
    const network = page.locator("section.panel").filter({ has: page.getByRole("heading", { name: "Connectivity & security" }) });
    await network.getByText("Off", { exact: true }).waitFor();
    await network.getByRole("button", { name: "Edit network" }).click();
    dialog = page.getByRole("dialog");
    await dialog.getByRole("switch", { name: /Public access/ }).check();
    const publicPort = await freePort();
    await dialog.getByLabel("Public port").fill(String(publicPort));
    await dialog.getByRole("button", { name: "Add source" }).click();
    await dialog.getByPlaceholder("203.0.113.7 or 203.0.113.0/24").fill("203.0.113.7");
    await dialog.getByRole("button", { name: "Save network access" }).click();
    await network.getByText(`On · port ${publicPort}`).waitFor({ timeout: 60000 });
    await page.locator(".conn-string pre").filter({ hasText: `:${publicPort}/app` }).waitFor(); // the internet string appears
    const firewall = page.locator("section.panel").filter({ has: page.getByRole("heading", { name: /^Firewall rules/ }) });
    await firewall.getByRole("cell", { name: "203.0.113.7/32" }).waitFor();
    await page.getByText("TLS is not required", { exact: false }).waitFor();
    await network.getByRole("button", { name: "Edit network" }).click();
    await page.getByRole("dialog").getByRole("switch", { name: /Public access/ }).uncheck();
    await page.getByRole("dialog").getByRole("button", { name: "Save network access" }).click();
    await network.getByText("Off", { exact: true }).waitFor({ timeout: 60000 });
    assert.ok(await page.getByRole("link", { name: "CA certificate" }).getAttribute("href") === "/api/tls/ca.pem");

    // Monitoring renders the container's four charts, the engine's three, and their table view.
    await page.getByRole("tab", { name: "Monitoring" }).click();
    await page.getByRole("heading", { name: "CPU utilization" }).waitFor();
    await page.getByRole("heading", { name: "Buffer cache hit ratio" }).waitFor();
    assert.equal(await page.locator("figure.chart").count(), 7);
    await page.getByRole("button", { name: "Table" }).first().click();
    await page.locator(".chart-table").first().waitFor();

    // Performance: what runs now and the statements that take the most time.
    await page.getByRole("tab", { name: "Performance" }).click();
    await page.getByRole("heading", { name: "Running now" }).waitFor();
    await page.getByRole("heading", { name: "Top statements" }).waitFor();
    await page.locator("table").filter({ hasText: "Total time" }).or(page.getByText("No statements yet")).first().waitFor({ timeout: 30000 });

    // Logs, configuration and storage
    await page.getByRole("tab", { name: "Logs & events" }).click();
    await page.locator("pre.logs").filter({ hasText: "ready to accept connections" }).waitFor();
    await page.getByRole("tab", { name: "Configuration" }).click();
    const row = page.locator("tr").filter({ hasText: "max_connections" });
    await row.getByText("120", { exact: true }).waitFor();
    await row.getByText("Applied").waitFor();
    await page.getByRole("button", { name: "Measure usage" }).click();
    await page.getByRole("status").filter({ hasText: quotas ? "used of 2 GiB" : "used · uncapped" }).waitFor();
    // Settings change in place, one at a time.
    const backupsRow = page.locator(".set-row").filter({ hasText: "Automated backups" });
    await backupsRow.getByRole("button", { name: "Change" }).click();
    await page.getByRole("dialog").getByLabel("Keep automated backups for").selectOption("3");
    await page.getByRole("dialog").getByLabel("Frequency", { exact: true }).selectOption("6");
    await page.getByRole("dialog").getByText("Keeps about 12 backups", { exact: false }).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "Save backups" }).click();
    await backupsRow.getByText("3 days", { exact: false }).waitFor({ timeout: 30000 });
    await backupsRow.getByText("Every 6 hours from", { exact: false }).waitFor();

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
    await page.getByText("Changes apply at the next restart", { exact: false }).waitFor({ timeout: 30000 });
    await page.getByRole("button", { name: "Actions" }).click();
    await page.getByRole("menuitem", { name: "Reboot" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Reboot database" }).click();
    await page.getByText(`${name} rebooted.`, { exact: false }).waitFor({ timeout: 300000 });
    await page.getByText("Changes apply at the next restart", { exact: false }).waitFor({ state: "detached", timeout: 30000 });
    await page.getByText("1 vCPU · 1 GiB").first().waitFor();

    // Inventory search
    await page.locator('nav#nav [data-nav="databases"]').click();
    await page.getByRole("heading", { name: "Databases", level: 1 }).waitFor();
    await page.keyboard.press("/");
    assert.equal(await page.evaluate(() => document.activeElement.id), "search");
    await page.locator("#search").fill("env=ci");
    await page.getByRole("link", { name: `Open ${name}` }).waitFor();
    assert.equal(await page.locator("#fleet-list tbody tr").count(), 1);

    // Automated backups: one row per database, changed in place.
    await page.locator('nav#nav [data-nav="automated"]').click();
    const schedule = page.locator(".sched-row").filter({ hasText: name });
    await schedule.getByText("Kept 3 days", { exact: false }).waitFor();
    await schedule.getByText("Every 6 hours from", { exact: false }).waitFor();
    await schedule.getByRole("button", { name: "Change" }).click();
    await page.getByRole("dialog").getByRole("heading", { name: "Automated backups" }).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();

    // Backup storage: providers fill in their usual settings.
    await page.locator('nav#nav [data-nav="storage"]').click();
    await page.getByRole("heading", { name: "Connect storage" }).waitFor();
    await page.getByText("Cloudflare R2", { exact: true }).click();
    assert.equal(await page.getByLabel("Region", { exact: true }).inputValue(), "auto");
    assert.ok((await page.getByLabel("Endpoint URL").inputValue()).includes("r2.cloudflarestorage.com"));

    // Notifications: a channel whose test message cannot arrive is not saved.
    await page.locator('nav#nav [data-nav="notifications"]').click();
    await page.getByRole("heading", { name: "Notifications", level: 1 }).waitFor();
    await page.getByRole("button", { name: "Add channel" }).first().click();
    const channel = page.getByRole("dialog");
    await channel.getByText("Webhook", { exact: true }).click();
    await channel.getByLabel("Name", { exact: true }).fill("ci hook");
    await channel.getByLabel("Webhook URL").fill(`http://127.0.0.1:${await freePort()}/hook`);
    await channel.getByRole("button", { name: "Send test and add" }).click();
    await channel.getByText("The test message did not arrive", { exact: false }).waitFor();
    await channel.getByRole("button", { name: "Cancel" }).click();
    await page.getByText("No channels yet").waitFor();

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

    const account = page.locator("#account button").first();
    await account.click();
    await page.getByRole("menuitemradio", { name: "Dark" }).click();
    assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), "dark");
    await account.click();
    await page.getByRole("menuitemradio", { name: "Match system" }).click();

    // Invite a viewer, accept the link in another browser, and check what a viewer can do.
    await page.locator('nav#nav [data-nav="users"]').click();
    await page.getByRole("button", { name: "Invite user" }).click();
    const invite = page.getByRole("dialog");
    await invite.getByLabel("Name", { exact: true }).fill("CI Viewer");
    await invite.getByLabel("Username").fill(viewer.username);
    await invite.getByText("Viewer", { exact: true }).click();
    await invite.getByRole("button", { name: "Create invite link" }).click();
    const link = await page.getByRole("dialog").locator("pre").filter({ hasText: "/#/invite/" }).innerText();
    await page.getByRole("dialog").getByRole("button", { name: "Done" }).click();
    const other = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const guest = await other.newPage();
    guest.on("pageerror", err => errors.push(`viewer: ${err.message}`));
    await guest.goto(link);
    await guest.getByRole("heading", { name: "Welcome, CI Viewer" }).waitFor();
    await guest.getByLabel("Choose a password", { exact: true }).fill(viewer.password);
    await guest.getByLabel("Confirm password", { exact: true }).fill(viewer.password);
    await guest.getByRole("button", { name: "Create my account" }).click();
    await guest.locator(".dash-hero h1").waitFor();
    assert.equal(await guest.locator("#nav-create").isVisible(), false, "viewers cannot create databases");
    assert.equal(await guest.locator('nav#nav [data-nav="users"]').isVisible(), false, "viewers cannot manage users");
    await guest.goto(`${base}/#/databases/${name}`);
    await guest.getByText("Read only").first().waitFor();
    await guest.getByText("Viewers can’t see passwords", { exact: false }).waitFor();
    await other.close();
    await page.getByRole("cell", { name: /CI Viewer/ }).first().waitFor();

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
    await page.locator("#account button").first().click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    await page.getByRole("heading", { name: "Open your workspace" }).waitFor();
    assert.deepEqual(errors, []);
    console.log("Browser: first-run setup, create with parameters and tags, connectivity guide, connection pooling, database users and passwords, network access, backup storage, notifications, monitoring, performance, logs, configuration, snapshot, deferred modify and reboot, search, restore form, events, themes, invite and viewer role, mobile layout and sign out passed.");
  } finally {
    await browser.close();
    for (const db of await api("instances")) if (db.name === name) await api(`instances/${name}`, "DELETE");
    for (const b of await api("backups")) if (b.name === name) await api(`backups/${b.id}`, "DELETE");
    for (const u of await api("users")) if ([admin.username, viewer.username].includes(u.username)) await api(`users/${u.id}`, "DELETE");
  }
}
run().catch(err => { console.error(err); process.exitCode = 1; });
