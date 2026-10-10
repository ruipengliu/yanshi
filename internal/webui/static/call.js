// 网页通话页：网页 SDK（yanshi.js，由 sdk/web 打包）+ 浏览器音频。
//   上行：getUserMedia（回声消除、降噪）→ AudioWorklet 重采样为 16 kHz、PCM 16 位 → 每 20 ms 一帧发给网关；
//   下行：24 kHz PCM 16 位按到达顺序排进播放队列；用户开口（onSpeech）时立即停止并丢弃该回复迟到的音频。
import { Client, Draft, PendingQuestions, textOf } from "/call/yanshi.js";

const $ = (id) => document.getElementById(id);
const state = { client: null, sid: "", call: null, muted: false };

// 设备 ID 持久保存：在场与提醒按它区分设备。
function deviceId() {
  let id = localStorage.getItem("yanshi.device");
  if (!id) {
    id = "web-" + crypto.randomUUID();
    localStorage.setItem("yanshi.device", id);
  }
  return id;
}

for (const k of ["bl", "user", "token", "agent", "sid"]) {
  const v = localStorage.getItem("yanshi." + k);
  if (v !== null) $(k).value = v;
  $(k).addEventListener("change", () => localStorage.setItem("yanshi." + k, $(k).value));
}

function status(text) {
  $("status").textContent = text;
}

// —— 对话显示 ——

const log = $("log");
function bubble(role, text, extra = "") {
  const el = document.createElement("div");
  el.className = `msg ${role} ${extra}`;
  el.textContent = text;
  log.append(el);
  log.scrollTop = log.scrollHeight;
  return el;
}
function note(text) {
  const el = document.createElement("div");
  el.className = "note";
  el.textContent = text;
  log.append(el);
  log.scrollTop = log.scrollHeight;
}

// 实时显示：识别中的用户的话、生成中的助手的话（通话）与文字对话的草稿。整句以日志中的事件为准。
const live = { user: null, assistant: null, draft: null };
function setLive(key, role, text, extra) {
  if (!text) {
    live[key]?.remove();
    live[key] = null;
    return;
  }
  if (!live[key]) live[key] = bubble(role, "", `partial ${extra}`);
  live[key].textContent = text;
  log.scrollTop = log.scrollHeight;
}

const draft = new Draft();
// 通话中派生的 Run：它的文字回复来自后台助手（语音会另行转述）。
const callRuns = new Set();
const questions = new PendingQuestions();
const approvals = new Map(); // call_id → 摘要

function renderPending() {
  const box = $("pending");
  box.replaceChildren();
  for (const [callId, summary] of approvals) {
    const card = document.createElement("div");
    card.className = "card pending";
    card.innerHTML = `<div>需要你确认：<b></b></div><div class="row"><button class="primary">批准</button><button>拒绝</button></div>`;
    card.querySelector("b").textContent = summary;
    const [ok, no] = card.querySelectorAll("button");
    ok.onclick = () => state.client.decide(state.sid, callId, true).catch((e) => note("审批失败：" + e.message));
    no.onclick = () => state.client.decide(state.sid, callId, false).catch((e) => note("审批失败：" + e.message));
    box.append(card);
  }
  for (const q of questions.questions) {
    const card = document.createElement("div");
    card.className = "card pending";
    card.innerHTML = `<div></div><div class="row"></div>`;
    card.firstChild.textContent = "提问：" + q.question;
    const row = card.lastChild;
    for (const o of q.options) {
      const b = document.createElement("button");
      b.textContent = o.label;
      b.onclick = () => state.client.answer(state.sid, q.callId, { selected: [o.id] }).catch((e) => note("回答失败：" + e.message));
      row.append(b);
    }
    box.append(card);
  }
}

function onEvent(e) {
  const p = e.payload;
  if (draft.onEvent(e)) setLive("draft", "assistant", draft.text, "");
  if (questions.onEvent(e)) renderPending();
  switch (p.case) {
    case "runRequested":
    case "steered":
      if (p.value.fromCall) {
        callRuns.add(p.value.runId);
        note("🎙 交给后台：" + textOf(p.value.input));
      } else bubble("user", textOf(p.value.input));
      break;
    case "assistantMessage": {
      const t = textOf(p.value.content);
      if (t) bubble("assistant", (callRuns.has(p.value.runId) ? "后台助手：" : "") + t);
      for (const c of p.value.toolCalls) if (c.capability !== "ask_user") note("调用 " + c.capability);
      break;
    }
    case "callTranscript":
      bubble(p.value.role, p.value.text + (p.value.interrupted ? "……" : ""), "voice");
      break;
    case "callStarted":
      note(`通话开始（${p.value.deviceId}）`);
      break;
    case "callEnded":
      note("通话结束：" + p.value.reason);
      break;
    case "approvalRequested":
      approvals.set(p.value.callId, p.value.summary);
      renderPending();
      break;
    case "approvalDecided":
    case "toolCallStarted":
    case "toolResult":
      if (approvals.delete(p.value.callId)) renderPending();
      break;
    case "runCompleted":
      note("任务完成");
      break;
    case "runFailed":
      note("任务失败：" + p.value.reason);
      break;
    case "runInterrupted":
      note("任务已中断");
      break;
  }
}

// —— 连接 ——

$("connect").onclick = async () => {
  state.client?.stop();
  const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/connect`;
  const client = new Client({
    url,
    deviceId: deviceId(),
    token: $("token").value.trim(),
    businessLine: $("bl").value.trim(),
    endUser: $("user").value.trim(),
    label: "网页",
    kind: "browser",
    onConnected: () => status("已连接"),
    onDisconnected: () => status("连接断开，正在重连…"),
  });
  state.client = client;
  client.start();
  status("连接中…");
  try {
    let sid = $("sid").value.trim();
    if (!sid) {
      sid = await client.createSession($("agent").value.trim());
      $("sid").value = sid;
      localStorage.setItem("yanshi.sid", sid);
    }
    state.sid = sid;
    log.replaceChildren();
    approvals.clear();
    client.subscribe(sid, 0n, {
      onEvent,
      onDelta: (d) => draft.onDelta(d) && setLive("draft", "assistant", draft.text, ""),
      onPresence: (p) => {
        const others = p.viewers.filter((v) => v.deviceId !== deviceId()).map((v) => v.label || v.deviceId);
        status(`已连接 · ${sid}` + (others.length ? ` · 也在：${others.join("、")}` : ""));
      },
      onEnded: (reason) => status("订阅结束：" + reason),
    });
    const visible = () => client.setActivity(sid, document.visibilityState === "visible");
    document.onvisibilitychange = visible;
    visible();
    for (const id of ["call", "text", "send"]) $(id).disabled = false;
  } catch (e) {
    status("失败：" + e.message);
  }
};

$("send").onclick = async () => {
  const text = $("text").value.trim();
  if (!text) return;
  $("text").value = "";
  try {
    await state.client.submitText(state.sid, text);
  } catch (e) {
    note("发送失败：" + e.message);
  }
};
$("text").onkeydown = (e) => e.key === "Enter" && !e.isComposing && $("send").onclick();

// —— 音频 ——

// 重采样到 16 kHz（采样率已是 16 kHz 时直通），按 320 个采样（20 ms）一帧交给主线程，并报告音量。
const worklet = `
class Mic extends AudioWorkletProcessor {
  constructor() { super(); this.ratio = sampleRate / 16000; this.t = 0; this.prev = 0; this.out = new Int16Array(320); this.n = 0; this.peak = 0; }
  process(inputs) {
    const x = inputs[0][0];
    if (!x) return true;
    while (this.t < x.length - 1) {
      const i = Math.floor(this.t), f = this.t - i;
      const a = i < 0 ? this.prev : x[i], b = x[i + 1];
      const v = Math.max(-1, Math.min(1, a + (b - a) * f));
      this.peak = Math.max(this.peak, Math.abs(v));
      this.out[this.n++] = v * 0x7fff;
      if (this.n === 320) { this.port.postMessage({ pcm: this.out.slice(), peak: this.peak }); this.n = 0; this.peak = 0; }
      this.t += this.ratio;
    }
    this.t -= x.length;
    this.prev = x[x.length - 1];
    return true;
  }
}
registerProcessor("mic", Mic);`;

const audio = { ctx: null, stream: null, node: null, play: null, head: 0, sources: new Set(), current: "", dropped: new Set() };

async function startMic(onFrame) {
  audio.stream = await navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 },
  });
  // 优先让浏览器把麦克风重采样到 16 kHz（带抗混叠）；不支持时在 worklet 中线性插值，前置低通滤波。
  let ctx;
  let src;
  try {
    ctx = new AudioContext({ sampleRate: 16000 });
    src = ctx.createMediaStreamSource(audio.stream);
  } catch {
    ctx = new AudioContext();
    src = ctx.createMediaStreamSource(audio.stream);
  }
  await ctx.audioWorklet.addModule(URL.createObjectURL(new Blob([worklet], { type: "text/javascript" })));
  const node = new AudioWorkletNode(ctx, "mic");
  node.port.onmessage = (e) => {
    onFrame(e.data.pcm);
    $("meter").firstChild.style.width = Math.min(100, e.data.peak * 200) + "%";
  };
  let input = src;
  if (ctx.sampleRate !== 16000) {
    const lp = ctx.createBiquadFilter();
    lp.type = "lowpass";
    lp.frequency.value = 7000;
    src.connect(lp);
    input = lp;
  }
  input.connect(node);
  audio.ctx = ctx;
  audio.node = node;
  audio.play = new AudioContext();
  audio.head = 0;
}

function stopMic() {
  audio.stream?.getTracks().forEach((t) => t.stop());
  audio.ctx?.close();
  audio.play?.close();
  audio.ctx = audio.stream = audio.node = audio.play = null;
  audio.sources.clear();
  $("meter").firstChild.style.width = "0";
}

function play(pcm, responseId, rate) {
  const ctx = audio.play;
  if (!ctx || audio.dropped.has(responseId)) return;
  audio.current = responseId;
  const buf = ctx.createBuffer(1, pcm.length, rate);
  const ch = buf.getChannelData(0);
  for (let i = 0; i < pcm.length; i++) ch[i] = pcm[i] / 32768;
  const src = ctx.createBufferSource();
  src.buffer = buf;
  src.connect(ctx.destination);
  // 留 60 ms 余量，吸收网络抖动。
  const at = Math.max(ctx.currentTime + 0.06, audio.head);
  src.start(at);
  audio.head = at + buf.duration;
  audio.sources.add(src);
  src.onended = () => audio.sources.delete(src);
}

// 打断：停止已排队的音频，丢弃该回复迟到的音频。
function stopPlayback() {
  for (const s of audio.sources) s.stop();
  audio.sources.clear();
  audio.head = 0;
  if (audio.current) audio.dropped.add(audio.current);
}

// —— 通话 ——

let timer = 0;
function setCallUI(on) {
  $("call").textContent = on ? "挂断" : "开始通话";
  $("call").className = on ? "danger" : "primary";
  $("mute").disabled = $("interrupt").disabled = !on;
  clearInterval(timer);
  if (on) {
    const t0 = Date.now();
    timer = setInterval(() => {
      const s = Math.floor((Date.now() - t0) / 1000);
      $("callStatus").textContent = `通话中 ${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")} · ${state.call?.model ?? ""}`;
    }, 500);
  } else {
    $("callStatus").textContent = "";
  }
}

$("call").onclick = async () => {
  if (state.call) {
    state.call.hangup();
    return;
  }
  $("call").disabled = true;
  try {
    let call = null;
    // 先打开麦克风（需要用户授权），接通前的音频丢弃。
    await startMic((pcm) => {
      if (call && !state.muted) call.sendAudio(pcm);
    });
    call = await state.client.startCall(state.sid, {
      onAudio: (pcm, rid) => play(pcm, rid, call?.outputSampleRate ?? 24000),
      onSpeech: stopPlayback,
      onText: (t) => {
        if (t.role === "user") setLive("user", "user", t.final ? "" : t.text, "voice");
        else setLive("assistant", "assistant", t.final ? "" : (live.assistant?.textContent ?? "") + t.text, "voice");
      },
      onResponseDone: () => setLive("assistant", "", "", ""),
      onEnded: (reason) => {
        stopMic();
        state.call = null;
        setCallUI(false);
        setLive("user", "", "", "");
        setLive("assistant", "", "", "");
        if (reason !== "hangup") note("通话结束：" + reason);
      },
    });
    state.call = call;
    state.muted = false;
    $("mute").textContent = "静音";
    setCallUI(true);
  } catch (e) {
    stopMic();
    note("通话失败：" + e.message);
  } finally {
    $("call").disabled = false;
  }
};

$("mute").onclick = () => {
  state.muted = !state.muted;
  state.call?.mute(state.muted);
  $("mute").textContent = state.muted ? "取消静音" : "静音";
};

$("interrupt").onclick = () => {
  stopPlayback();
  state.call?.interrupt();
};
