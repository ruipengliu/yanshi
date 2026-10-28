import { XinliView } from "./xinli.js";
import { CaptureView } from "./captures.js";

const config = await fetch("/config.json").then((r) => r.json());
const query = new URLSearchParams(location.search);
const bl = document.getElementById("business-line");
const eu = document.getElementById("end-user");
bl.value = query.get("business_line") || config.business_lines[0] || "";
eu.value = query.get("end_user") || "demo";
let identity = [bl.value, eu.value];
const context = {
  identity: () => JSON.stringify(identity),
  fixturePath: () => `/v1/fixture/${identity.map(encodeURIComponent).join("/")}`,
  consoleURL: config.console_url,
  orchestration: config.orchestration,
  devPath: () => `/v1/dev/${identity.map(encodeURIComponent).join("/")}`,
  http: async (method, path, body) => {
    const r = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
    return { status: r.status, json: await r.json() };
  },
};
const view = new XinliView(context);
const captures = new CaptureView(context);
document.getElementById("app").append(captures.root, view.root);
document.getElementById("identity").addEventListener("submit", (e) => {
  e.preventDefault();
  identity = [bl.value.trim(), eu.value.trim()];
  const q = new URLSearchParams({ business_line: identity[0], end_user: identity[1] });
  history.replaceState(null, "", `?${q}`);
  captures.reset();
  view.state = null;
  view.error = "";
  view.openProject = "";
  view.render();
  view.refresh();
});
view.start();
captures.start();
window.addEventListener("pagehide", () => { view.stop(); captures.stop(); });
