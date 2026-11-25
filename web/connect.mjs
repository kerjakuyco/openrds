// "Connect your application": the connection string to copy, a second one
// for the internet while public access is on, and code examples tucked away.
import { h, icon, btn, api, toast, codeBlock, alertBox, nextId, copyText } from "./lib.mjs";

const PLACEHOLDER = "<password>";
const MASK = "••••••••";
export const markConnected = () => { try { localStorage.setItem("openrds-connected", "1"); } catch { /* private mode */ } };

// Which user the guide shows, per database, so other pages can preselect one.
const chosenUser = new Map();
export const connectAs = (name, user) => chosenUser.set(name, user);

const TLS_NOTE = {
  mysql: "Turn on TLS in your client and trust openrds-ca.pem.",
  valkey: "Use rediss:// and trust openrds-ca.pem.",
  postgres: "Uses TLS and checks the server with openrds-ca.pem.",
};

// Each stack: env lines (.env) and code that reads them. p: host, port, user, password, db, uri.
const STACKS = {
  postgres: [
    ["env", ".env file", p => ({ env: `DATABASE_URL=${p.uri}` })],
    ["node", "Node.js · pg", p => ({ env: `DATABASE_URL=${p.uri}`, code: `// npm install pg
import pg from "pg";

const pool = new pg.Pool({ connectionString: process.env.DATABASE_URL });
const { rows } = await pool.query("select now()");
console.log(rows[0]);` })],
    ["python", "Python · psycopg", p => ({ env: `DATABASE_URL=${p.uri}`, code: `# pip install "psycopg[binary]"
import os
import psycopg

with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
    print(conn.execute("select now()").fetchone())` })],
    ["go", "Go · pgx", p => ({ env: `DATABASE_URL=${p.uri}`, code: `// go get github.com/jackc/pgx/v5
conn, err := pgx.Connect(context.Background(), os.Getenv("DATABASE_URL"))
if err != nil {
	log.Fatal(err)
}
defer conn.Close(context.Background())` })],
    ["prisma", "Prisma", p => ({ env: `DATABASE_URL=${p.uri}`, code: `// schema.prisma
datasource db {
  provider = "postgresql"
  url      = env("DATABASE_URL")
}` })],
    ["django", "Django", p => ({ env: `DB_PASSWORD=${p.password}`, code: `# settings.py
import os

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "NAME": "${p.db}",
        "USER": "${p.user}",
        "PASSWORD": os.environ["DB_PASSWORD"],
        "HOST": "${p.host}",
        "PORT": "${p.port}",
    }
}` })],
    ["laravel", "Laravel", p => ({ env: `DB_CONNECTION=pgsql
DB_HOST=${p.host}
DB_PORT=${p.port}
DB_DATABASE=${p.db}
DB_USERNAME=${p.user}
DB_PASSWORD=${p.password}` })],
    ["java", "Java · JDBC", p => ({ env: `DB_PASSWORD=${p.password}`, code: `// Maven: org.postgresql:postgresql
String url = "jdbc:postgresql://${p.host}:${p.port}/${p.db}";
try (Connection conn = DriverManager.getConnection(url, "${p.user}", System.getenv("DB_PASSWORD"))) {
    // run queries
}` })],
    ["cli", "psql", p => ({ code: `psql "${p.uri}"` })],
  ],
  mysql: [
    ["env", ".env file", p => ({ env: `DATABASE_URL=${p.uri}` })],
    ["node", "Node.js · mysql2", p => ({ env: `DATABASE_URL=${p.uri}`, code: `// npm install mysql2
import mysql from "mysql2/promise";

const db = await mysql.createConnection(process.env.DATABASE_URL);
const [rows] = await db.query("select now() as now");
console.log(rows[0]);` })],
    ["python", "Python · PyMySQL", p => ({ env: `DB_PASSWORD=${p.password}`, code: `# pip install PyMySQL
import os
import pymysql

conn = pymysql.connect(host="${p.host}", port=${p.port}, user="${p.user}",
                       password=os.environ["DB_PASSWORD"], database="${p.db}")
with conn.cursor() as cur:
    cur.execute("select now()")
    print(cur.fetchone())` })],
    ["go", "Go · go-sql-driver", p => ({ env: `MYSQL_DSN=${p.user}:${p.password}@tcp(${p.host}:${p.port})/${p.db}?parseTime=true`, code: `// go get github.com/go-sql-driver/mysql
import _ "github.com/go-sql-driver/mysql"

db, err := sql.Open("mysql", os.Getenv("MYSQL_DSN"))
if err != nil {
	log.Fatal(err)
}
defer db.Close()` })],
    ["prisma", "Prisma", p => ({ env: `DATABASE_URL=${p.uri}`, code: `// schema.prisma
datasource db {
  provider = "mysql"
  url      = env("DATABASE_URL")
}` })],
    ["django", "Django", p => ({ env: `DB_PASSWORD=${p.password}`, code: `# settings.py
import os

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.mysql",
        "NAME": "${p.db}",
        "USER": "${p.user}",
        "PASSWORD": os.environ["DB_PASSWORD"],
        "HOST": "${p.host}",
        "PORT": "${p.port}",
    }
}` })],
    ["laravel", "Laravel", p => ({ env: `DB_CONNECTION=mysql
DB_HOST=${p.host}
DB_PORT=${p.port}
DB_DATABASE=${p.db}
DB_USERNAME=${p.user}
DB_PASSWORD=${p.password}` })],
    ["java", "Java · JDBC", p => ({ env: `DB_PASSWORD=${p.password}`, code: `// Maven: com.mysql:mysql-connector-j
String url = "jdbc:mysql://${p.host}:${p.port}/${p.db}";
try (Connection conn = DriverManager.getConnection(url, "${p.user}", System.getenv("DB_PASSWORD"))) {
    // run queries
}` })],
    ["cli", "mysql client", p => ({ code: `mysql --host=${p.host} --port=${p.port} --user=${p.user} --password ${p.db}`, note: "The client asks for the password." })],
  ],
  valkey: [
    ["env", ".env file", p => ({ env: `REDIS_URL=${p.uri}` })],
    ["node", "Node.js · ioredis", p => ({ env: `REDIS_URL=${p.uri}`, code: `// npm install ioredis
import Redis from "ioredis";

const redis = new Redis(process.env.REDIS_URL);
await redis.set("greeting", "hello");
console.log(await redis.get("greeting"));` })],
    ["noderedis", "Node.js · node-redis", p => ({ env: `REDIS_URL=${p.uri}`, code: `// npm install redis
import { createClient } from "redis";

const client = createClient({ url: process.env.REDIS_URL });
await client.connect();
console.log(await client.ping());` })],
    ["python", "Python · redis-py", p => ({ env: `REDIS_URL=${p.uri}`, code: `# pip install redis
import os
import redis

r = redis.from_url(os.environ["REDIS_URL"])
print(r.ping())` })],
    ["go", "Go · go-redis", p => ({ env: `REDIS_URL=${p.uri}`, code: `// go get github.com/redis/go-redis/v9
opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
if err != nil {
	log.Fatal(err)
}
rdb := redis.NewClient(opt)
fmt.Println(rdb.Ping(context.Background()).Val())` })],
    ["laravel", "Laravel", p => ({ env: `REDIS_HOST=${p.host}
REDIS_PORT=${p.port}
REDIS_USERNAME=${p.user}
REDIS_PASSWORD=${p.password}` })],
    ["cli", "valkey-cli / redis-cli", p => ({ code: `valkey-cli -u "${p.uri}"` })],
  ],
};

// Whether a string uses TLS: always when it is required, and over the
// internet whenever the engine offers it next to plain text. The pooler
// follows the database.
const usesTLS = (d, where) => d.tlsReady && (d.requireTLS || (where === "internet" && d.engine !== "valkey"));

function settings(d, eng, where, user, password) {
  const internet = where === "internet";
  const host = internet ? (d.publicHost || "YOUR_SERVER_ADDRESS") : "127.0.0.1";
  const port = internet ? d.publicPort : where === "pooler" ? d.pooler.port : d.port;
  const tls = usesTLS(d, where);
  const scheme = { postgres: "postgresql", mysql: "mysql", valkey: tls ? "rediss" : "redis" }[d.engine];
  const query = tls && d.engine === "postgres" ? "?sslmode=verify-full&sslrootcert=openrds-ca.pem" : "";
  const uriWith = (pw) => `${scheme}://${user}:${pw}@${host}:${port}/${eng.database}${query}`;
  return { host, port, user, password, db: eng.database, tls, uri: uriWith(password), uriWith };
}

// Command-line clients need their own TLS flags.
function tlsCommand(d, p, code) {
  if (!p.tls || !code) return code;
  if (d.engine === "mysql") return code.replace("mysql --host", "mysql --ssl-mode=VERIFY_IDENTITY --ssl-ca=openrds-ca.pem --host");
  if (d.engine === "valkey") return code.replace("valkey-cli -u", "valkey-cli --tls --cacert openrds-ca.pem -u");
  return code;
}

let remembered = { stack: {} };
try { remembered = { ...remembered, ...JSON.parse(localStorage.getItem("openrds-connect") || "{}") }; } catch { /* private mode */ }
const remember = () => { try { localStorage.setItem("openrds-connect", JSON.stringify(remembered)); } catch { /* private mode */ } };

export function connectGuide(d, eng, { canReveal }) {
  const stacks = STACKS[d.engine];
  const saved = remembered.stack[d.engine];
  let stack = stacks.some(([key]) => key === saved) ? saved : (stacks[1] || stacks[0])[0];
  const users = [eng.user, ...(d.users || []).map(u => u.name)];
  let user = users.includes(chosenUser.get(d.name)) ? chosenUser.get(d.name) : eng.user;
  let c = null, shown = false;

  async function load() {
    c ||= await api(`instances/${d.name}/connection${user === eng.user ? "" : `?user=${encodeURIComponent(user)}`}`);
    return c;
  }
  const visible = () => shown && c ? c.password : canReveal ? MASK : PLACEHOLDER;

  // A connection string with its show and copy buttons. Copy fetches the
  // password itself, so one click is enough.
  const shows = [];
  function stringRow(where, title) {
    const pre = h("pre");
    const show = h("button", { type: "button", class: "icon-btn ghost", "aria-pressed": "false", "aria-label": "Show password" }, icon("eye"));
    show.addEventListener("click", async () => {
      if (!shown) {
        try { await load(); } catch (err) { toast(err.message, "error"); return; }
      }
      shown = !shown;
      render();
    });
    shows.push(show);
    const copy = btn([icon("copy"), "Copy"], async () => {
      try {
        if (canReveal) await load();
        const ok = await copyText(settings(d, eng, where, user, "").uriWith(c?.password || PLACEHOLDER));
        toast(ok ? (c ? "Copied, with the password." : "Copied.") : "Copy failed. Select and copy it.", ok ? "success" : "warn");
        if (ok) markConnected();
      } catch (err) { toast(err.message, "error"); }
    }, "small");
    const row = h("div", { class: "code-wrap" }, h("p", { class: "conn-label" }, title),
      h("div", { class: "code conn-string" }, pre, h("div", { class: "conn-actions" }, canReveal ? show : null, copy)));
    row.update = () => { pre.textContent = settings(d, eng, where, user, visible()).uri; };
    return row;
  }
  const local = stringRow("vps", "Connection string");
  const remote = d.publicAccess ? stringRow("internet", "From the internet") : null;
  const pooled = d.pooler ? stringRow("pooler", "Through the connection pooler") : null;
  const notes = h("div", { class: "conn-after" });

  // Code examples and another user, out of the way until wanted.
  const stackID = nextId("stack"), userID = nextId("as");
  const stackSelect = h("select", { id: stackID }, stacks.map(([key, title]) => h("option", { value: key, selected: key === stack }, title)));
  stackSelect.addEventListener("change", () => { stack = stackSelect.value; remembered.stack[d.engine] = stack; remember(); render(); });
  const userSelect = h("select", { id: userID }, users.map(name => h("option", { value: name, selected: name === user }, name === eng.user ? `${name} (owner)` : name)));
  userSelect.addEventListener("change", () => { user = userSelect.value; chosenUser.set(d.name, user); c = null; shown = false; render(); });
  const example = h("div", { class: "conn-example" });
  const more = h("details", { class: "disclosure conn-more", open: user !== eng.user },
    h("summary", null, "Code examples and options"),
    h("div", { class: "conn-options" },
      h("div", { class: "conn-controls" },
        h("div", { class: "control" }, h("label", { class: "field-label", for: stackID }, "Language or tool"), stackSelect),
        users.length > 1 ? h("div", { class: "control" }, h("label", { class: "field-label", for: userID }, "Connect as"), userSelect) : null),
      example));

  function render() {
    for (const b of shows) {
      b.setAttribute("aria-pressed", String(shown));
      b.setAttribute("aria-label", shown ? "Hide password" : "Show password");
    }
    local.update();
    remote?.update();
    pooled?.update();

    const tls = usesTLS(d, "vps") || (d.publicAccess && usesTLS(d, "internet"));
    const list = [];
    if (tls) {
      list.push(h("p", { class: "conn-note" }, icon("lock"), " Uses TLS. ",
        h("a", { href: "/api/tls/ca.pem", download: "openrds-ca.pem" }, "Download the CA certificate"), " and keep it next to your app."));
    }
    if (d.publicAccess) {
      list.push(h("p", { class: "conn-note" }, icon("globe"), " Allow your app’s address under Firewall rules.", d.publicHost ? "" : " Replace YOUR_SERVER_ADDRESS with your server’s IP."));
      if (!usesTLS(d, "internet")) list.push(alertBox("Internet traffic isn’t encrypted. Turn on Require TLS first.", "warn"));
    }
    if (!canReveal) list.push(alertBox("Viewers can’t see passwords. Ask an operator.", "info"));
    notes.replaceChildren(...list);

    // Snippets get a placeholder rather than the mask, so copying them never copies dots.
    const ex = settings(d, eng, "vps", user, shown && c ? c.password : PLACEHOLDER);
    const out = stacks.find(([key]) => key === stack)[2](ex);
    const code = stack === "cli" ? tlsCommand(d, ex, out.code) : out.code;
    example.replaceChildren(...[
      out.env && codeBlock(out.env, { caption: ".env", onCopy: markConnected }),
      code && codeBlock(code, { onCopy: markConnected }),
      out.note && h("p", { class: "muted" }, out.note),
      ex.tls && stack !== "cli" && d.engine !== "postgres" ? h("p", { class: "muted" }, icon("lock"), " ", TLS_NOTE[d.engine]) : null,
    ].filter(Boolean));
  }
  render();

  const el = h("section", { class: "panel connect-guide" },
    h("header", { class: "panel-head" },
      h("div", null, h("h2", null, icon("link"), "Connect your application"), h("p", { class: "muted" }, "Copy this into your app’s settings."))),
    h("div", { class: "panel-body" }, local, pooled, remote, notes, more));
  return { el };
}
