import { XinliView } from "./xinli.js";

const config = await fetch("/config.json").then((r) => r.json());
const query = new URLSearchParams(location.search);
const bl = document.getElementById("business-line");
const eu = document.getElementById("end-user");
bl.value = query.get("business_line") || config.business_lines[0] || "";
eu.value = query.get("end_user") || "demo";
let identity = [bl.value, eu.value];
const view = new XinliView({
  identity: () => JSON.stringify(identity),
  fixturePath: () => `/v1/fixture/${identity.map(encodeURIComponent).join("/")}`,
  consoleURL: config.console_url,
  http: async (method, path, body) => {
    const r = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
    return { status: r.status, json: await r.json() };
  },
});
document.getElementById("app").append(view.root);
document.getElementById("identity").addEventListener("submit", (e) => {
  e.preventDefault();
  identity = [bl.value.trim(), eu.value.trim()];
  const q = new URLSearchParams({ business_line: identity[0], end_user: identity[1] });
  history.replaceState(null, "", `?${q}`);
  view.state = null;
  view.error = "";
  view.openProject = "";
  view.render();
  view.refresh();
});
view.start();
window.addEventListener("pagehide", () => view.stop());
