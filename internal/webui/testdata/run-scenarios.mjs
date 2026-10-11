// 以 Node.js 运行调试控制台的验证场景（static/console/scenarios.js），由 internal/e2e 的 TestConsoleScenarios 调用：
//
//   YANSHI_URL=ws://…/v1/connect YANSHI_HTTP=http://… YANSHI_AGENT=… \
//   YANSHI_USER_A=… YANSHI_TOKEN_A=… YANSHI_USER_B=… YANSHI_TOKEN_B=… node run-scenarios.mjs [场景 ID…]
//
// 逐个输出场景的步骤与结果；全部通过时最后一行为 "ok"，否则以 1 退出。

import { SCENARIOS, runScenario } from "../static/console/scenarios.js";

const env = {
  wsUrl: process.env.YANSHI_URL,
  httpBase: process.env.YANSHI_HTTP,
  businessLine: process.env.YANSHI_BUSINESS_LINE ?? "",
  agent: process.env.YANSHI_AGENT ?? "console",
  users: [
    { endUser: process.env.YANSHI_USER_A, token: process.env.YANSHI_TOKEN_A ?? "" },
    { endUser: process.env.YANSHI_USER_B, token: process.env.YANSHI_TOKEN_B ?? "" },
  ],
};
const only = process.argv.slice(2);
let failed = 0;
for (const s of SCENARIOS) {
  if (only.length && !only.includes(s.id)) continue;
  const r = await runScenario(s, env, (m) => console.log(`  ${m}`));
  console.log(`${r.ok ? "PASS" : "FAIL"} ${s.id} ${s.title} (${r.ms} ms)`);
  if (!r.ok) failed++;
}
console.log(failed ? `${failed} scenario(s) failed` : "ok");
process.exit(failed ? 1 : 0);
