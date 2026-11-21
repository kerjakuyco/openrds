// "Connect your application": pick where the app runs and its language, then
// copy the tunnel or network step, the environment variable and the code.
import { h, icon, btn, api, toast, codeBlock, alertBox, nextId } from "./lib.mjs";

const INTERNAL_PORT = { postgres: 5432, mysql: 3306, valkey: 6379 };
const PLACEHOLDER = "<password>";
export const markConnected = () => { try { localStorage.setItem("openrds-connected", "1"); } catch { /* private mode */ } };

const WHERE = [
  ["vps", "On this VPS", "server"],
  ["container", "In a Docker container here", "box"],
  ["computer", "On my computer", "laptop"],
];

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

function settings(d, eng, where, c) {
  const container = where === "container";
  const host = container ? `openrds-${d.name}` : "127.0.0.1";
  const port = container ? INTERNAL_PORT[d.engine] : d.port;
  const password = c?.password || PLACEHOLDER;
  const scheme = { postgres: "postgresql", mysql: "mysql", valkey: "redis" }[d.engine];
  return { host, port, user: eng.user, password, db: eng.database, uri: `${scheme}://${eng.user}:${password}@${host}:${port}/${eng.database}` };
}

let remembered = { where: "vps", stack: {} };
try { remembered = { ...remembered, ...JSON.parse(localStorage.getItem("openrds-connect") || "{}") }; } catch { /* private mode */ }
const remember = () => { try { localStorage.setItem("openrds-connect", JSON.stringify(remembered)); } catch { /* private mode */ } };

export function connectGuide(d, eng, { canReveal }) {
  const stacks = STACKS[d.engine];
  let where = remembered.where, stack = remembered.stack[d.engine] || stacks[0][0], c = null;
  const whereButtons = WHERE.map(([key, label]) => h("button", { type: "button", "aria-pressed": String(key === where), onclick: () => { where = key; remembered.where = key; remember(); render(); } }, label));
  const stackID = nextId("stack");
  const stackSelect = h("select", { id: stackID }, stacks.map(([key, label]) => h("option", { value: key, selected: key === stack }, label)));
  stackSelect.addEventListener("change", () => { stack = stackSelect.value; remembered.stack[d.engine] = stack; remember(); render(); });
  const credentials = h("div", { class: "credentials" });
  const steps = h("ol", { class: "steps" });

  async function reveal(b) {
    b.disabled = true;
    try { c = await api(`instances/${d.name}/connection`); render(); }
    catch (err) { toast(err.message, "error"); b.disabled = false; }
  }

  function step(title, ...body) {
    steps.append(h("li", null, h("h4", null, title), ...body));
  }

  function render() {
    whereButtons.forEach((b, i) => b.setAttribute("aria-pressed", String(WHERE[i][0] === where)));
    const p = settings(d, eng, where, c);
    credentials.replaceChildren(c
      ? h("p", { class: "muted" }, icon("key"), " The snippets include the password. Keep them out of version control.", " ", btn("Hide password", () => { c = null; render(); }, "small ghost"))
      : canReveal
        ? h("div", { class: "row" }, h("span", { class: "muted" }, `Snippets show ${PLACEHOLDER} until you reveal the password.`), btn([icon("eye"), "Reveal password"], reveal, "small"))
        : alertBox("Your Viewer role cannot see database passwords. Ask an operator or administrator for the connection details.", "info"));
    steps.replaceChildren();
    if (where === "computer") {
      step("Open an SSH tunnel and keep it running", h("p", { class: "muted" }, "Replace user@your-vps with your SSH login. The database stays private to the VPS."),
        codeBlock(`ssh -N -L ${d.port}:127.0.0.1:${d.port} user@your-vps`));
    }
    if (where === "container") {
      step("Put your container on the database’s network",
        c ? codeBlock(`docker network connect ${c.network} YOUR_APP_CONTAINER`)
          : h("p", { class: "muted" }, canReveal ? "Reveal the password to see the network name." : "An operator can share the network name."),
        c && h("details", { class: "disclosure" }, h("summary", null, "Using Docker Compose?"), codeBlock(`services:
  app:
    networks: [default, ${d.name}-db]

networks:
  ${d.name}-db:
    external: true
    name: ${c.network}`, { caption: "compose.yaml" })));
    }
    const out = stacks.find(([key]) => key === stack)[2](p);
    if (out.env) step("Add the connection settings to your environment", codeBlock(out.env, { caption: ".env", onCopy: markConnected }));
    if (out.code) step(out.env ? "Read them in your code" : "Connect", codeBlock(out.code, { onCopy: markConnected }), out.note && h("p", { class: "muted" }, out.note));
    if (!out.code) step("Restart your application", h("p", { class: "muted" }, "It now reads the settings above."));
  }
  render();

  const el = h("section", { class: "panel connect-guide" },
    h("header", { class: "panel-head" }, h("div", null, h("h2", null, icon("link"), "Connect your application"),
      h("p", { class: "muted" }, "Choose where your app runs and what it is written in. Everything below updates to match."))),
    h("div", { class: "panel-body" },
      h("div", { class: "guide-controls" },
        h("div", { class: "control" }, h("span", { class: "field-label", id: `${stackID}-where` }, "Where does your app run?"),
          h("div", { class: "segmented", role: "group", "aria-labelledby": `${stackID}-where` }, whereButtons)),
        h("div", { class: "control" }, h("label", { class: "field-label", for: stackID }, "Language or tool"), stackSelect)),
      credentials, steps));
  return { el };
}
